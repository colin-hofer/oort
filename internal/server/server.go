package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"nebulous/internal/db"
	"nebulous/internal/queryexec"
	"nebulous/internal/storage"
)

type Config struct {
	DatabaseURL   string
	Listen        string
	LocalAuth     bool
	StateDir      string
	CatalogSecret string
	ExtensionDir  string
	Storage       storage.Config
	QueryTimeout  time.Duration
	Log           io.Writer
}

type Server struct {
	database   *sql.DB
	config     Config
	objects    *storage.Client
	executable string
}

var (
	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

func Run(ctx context.Context, config Config) error {
	if config.LocalAuth {
		if !loopbackAddress(config.Listen) {
			return fmt.Errorf("local authentication requires a loopback listener, got %q", config.Listen)
		}
	} else {
		return fmt.Errorf("production authentication is not configured; refusing to start")
	}
	database, err := db.Open(ctx, config.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", config.Listen, err)
	}
	defer listener.Close()
	user, token, err := db.CreateLocalIdentity(ctx, database, "local@nebulous.invalid", 24*time.Hour)
	if err != nil {
		return err
	}
	if err := writeLocalState(config.StateDir, config.Listen, user, token); err != nil {
		return err
	}
	objects, err := storage.New(config.Storage)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate platform executable: %w", err)
	}
	if config.QueryTimeout <= 0 {
		config.QueryTimeout = 10 * time.Second
	}
	if config.Log == nil {
		config.Log = os.Stderr
	}

	httpServer := &http.Server{
		Handler:           newHandler(&Server{database: database, config: config, objects: objects, executable: executable}),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
	}
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdown)
	}
}

func New(database *sql.DB) http.Handler {
	return newHandler(&Server{database: database})
}

func newHandler(server *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("POST /v1/tenants", server.authenticate("tenants:write", http.HandlerFunc(server.createTenant)))
	mux.Handle("GET /v1/tenants", server.authenticate("tenants:read", http.HandlerFunc(server.listTenants)))
	mux.Handle("POST /v1/tenants/{tenant}/dataset-uploads", server.authenticate("datasets:write", http.HandlerFunc(server.createDatasetUpload)))
	mux.Handle("POST /v1/tenants/{tenant}/dataset-uploads/{run}/complete", server.authenticate("datasets:write", http.HandlerFunc(server.completeDatasetUpload)))
	mux.Handle("GET /v1/tenants/{tenant}/sync-runs/{run}", server.authenticate("datasets:read", http.HandlerFunc(server.getSyncRun)))
	mux.Handle("POST /v1/tenants/{tenant}/queries/run", server.authenticate("queries:write", http.HandlerFunc(server.runQuery)))
	return requestIDs(mux)
}

type userContextKey struct{}
type requestContextKey struct{}

func requestIDs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestID()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestContextKey{}, id)))
	})
}

func (s *Server) authenticate(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "Bearer ") || strings.Contains(strings.TrimPrefix(authorization, "Bearer "), " ") {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "provide a valid Bearer token")
			return
		}
		user, err := db.Authenticate(r.Context(), s.database, strings.TrimPrefix(authorization, "Bearer "), scope)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, r, http.StatusUnauthorized, "unauthenticated", "the API token is invalid or expired")
				return
			}
			writeError(w, r, http.StatusInternalServerError, "internal", "authentication failed")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, user)))
	})
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "tenant creation does not accept query parameters")
		return
	}
	var input struct {
		Slug string `json:"slug"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "body must be a JSON object containing only slug")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "body must contain one JSON object")
		return
	}
	if !slugPattern.MatchString(input.Slug) {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_slug", "slug must be 3-63 lowercase letters, digits, or hyphens")
		return
	}
	user := r.Context().Value(userContextKey{}).(db.User)
	tenant, err := db.CreateTenant(r.Context(), s.database, user, input.Slug, r.Context().Value(requestContextKey{}).(string))
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			writeError(w, r, http.StatusConflict, "slug_taken", "that tenant slug is already in use")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal", "tenant creation failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tenant": tenant})
}

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "tenant listing does not accept query parameters")
		return
	}
	user := r.Context().Value(userContextKey{}).(db.User)
	tenants, err := db.ListTenants(r.Context(), s.database, user)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "tenant listing failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": tenants})
}

func (s *Server) createDatasetUpload(w http.ResponseWriter, r *http.Request) {
	if s.objects == nil {
		writeError(w, r, http.StatusServiceUnavailable, "not_configured", "dataset storage is not configured")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.builderTenant(w, r, actor)
	if !ok {
		return
	}
	var input struct {
		Slug           string `json:"slug"`
		Format         string `json:"format"`
		ByteCount      int64  `json:"byte_count"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !slugPattern.MatchString(input.Slug) {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_slug", "dataset slug must be 3-63 lowercase letters, digits, or hyphens")
		return
	}
	if input.Format != "csv" && input.Format != "parquet" {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_format", "dataset format must be csv or parquet")
		return
	}
	if input.ByteCount < 1 || input.ByteCount > 1<<30 {
		writeError(w, r, http.StatusRequestEntityTooLarge, "upload_too_large", "upload must be between 1 byte and 1 GiB")
		return
	}
	if len(input.IdempotencyKey) < 1 || len(input.IdempotencyKey) > 200 {
		writeError(w, r, http.StatusBadRequest, "invalid_idempotency_key", "idempotency_key must be 1-200 characters")
		return
	}
	upload, err := db.CreateDatasetUpload(r.Context(), s.database, tenant, actor, input.Slug, input.Format,
		input.ByteCount, input.IdempotencyKey, r.Context().Value(requestContextKey{}).(string))
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			writeError(w, r, http.StatusConflict, "idempotency_conflict", "that idempotency key belongs to a different upload")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal", "dataset upload creation failed")
		return
	}
	response := map[string]any{"dataset": upload.Dataset, "run": upload.Run}
	if upload.Run.Status == "awaiting_upload" {
		putURL, headers, err := s.objects.PresignPut(upload.Run.ObjectKey, upload.Run.ByteCount, 15*time.Minute)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal", "signing the dataset upload failed")
			return
		}
		response["upload"] = map[string]any{"url": putURL, "headers": headers}
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) completeDatasetUpload(w http.ResponseWriter, r *http.Request) {
	if !uuidPattern.MatchString(r.PathValue("run")) {
		writeError(w, r, http.StatusNotFound, "not_found", "sync run was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.builderTenant(w, r, actor)
	if !ok {
		return
	}
	if err := decodeJSON(w, r, &struct{}{}); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "body must be an empty JSON object")
		return
	}
	run, err := db.CompleteDatasetUpload(r.Context(), s.database, tenant.ID, r.PathValue("run"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, r, http.StatusNotFound, "not_found", "sync run was not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal", "dataset upload completion failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
}

func (s *Server) getSyncRun(w http.ResponseWriter, r *http.Request) {
	if !uuidPattern.MatchString(r.PathValue("run")) {
		writeError(w, r, http.StatusNotFound, "not_found", "tenant or sync run was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, err := db.ResolveTenant(r.Context(), s.database, actor, r.PathValue("tenant"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, r, http.StatusNotFound, "not_found", "tenant or sync run was not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal", "tenant lookup failed")
		return
	}
	run, err := db.GetSyncRun(r.Context(), s.database, tenant.ID, r.PathValue("run"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, r, http.StatusNotFound, "not_found", "tenant or sync run was not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal", "sync run lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

func (s *Server) runQuery(w http.ResponseWriter, r *http.Request) {
	if s.objects == nil || s.executable == "" {
		writeError(w, r, http.StatusServiceUnavailable, "not_configured", "query execution is not configured")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.builderTenant(w, r, actor)
	if !ok {
		return
	}
	var input struct {
		Name       string         `json:"name"`
		SQL        string         `json:"sql"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !slugPattern.MatchString(input.Name) {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_slug", "query name must be 3-63 lowercase letters, digits, or hyphens")
		return
	}
	cleaned, parameterTypes, err := queryexec.Validate(input.SQL, input.Parameters)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_query", err.Error())
		return
	}
	revision, err := db.SaveQueryRevision(r.Context(), s.database, tenant, actor, input.Name, cleaned,
		parameterTypes, r.Context().Value(requestContextKey{}).(string))
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "saving the query revision failed")
		return
	}
	catalogURL, _, _, err := db.TenantCatalog(s.config.DatabaseURL, s.config.CatalogSecret, tenant.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "tenant catalog configuration failed")
		return
	}
	queryContext, cancel := context.WithTimeout(r.Context(), s.config.QueryTimeout)
	defer cancel()
	result, err := os.CreateTemp("", "nebulous-result-*.json")
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "query result staging failed")
		return
	}
	resultPath := result.Name()
	defer os.Remove(resultPath)
	defer result.Close()
	err = queryexec.Run(queryContext, s.executable, queryexec.Request{
		CatalogURL: catalogURL, DataPath: s.objects.DataPath(tenant.ID), ExtensionDir: s.config.ExtensionDir,
		Storage: s.config.Storage, SQL: cleaned, Parameters: input.Parameters, ParameterTypes: parameterTypes,
		MaxRows: 10_000, MaxBytes: 10 << 20,
	}, result)
	if err != nil {
		if s.config.Log != nil {
			fmt.Fprintf(s.config.Log, "query request=%s tenant=%s failed\n",
				r.Context().Value(requestContextKey{}).(string), tenant.ID)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(w, r, http.StatusGatewayTimeout, "query_timeout", "query exceeded the 10-second limit")
			return
		}
		writeError(w, r, http.StatusUnprocessableEntity, "query_failed", "the saved query could not be executed")
		return
	}
	if _, err := result.Seek(0, io.SeekStart); err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "query result staging failed")
		return
	}
	revisionJSON, _ := json.Marshal(revision)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, `{"query":%s,"result":`, revisionJSON)
	_, _ = io.Copy(w, result)
	_, _ = fmt.Fprintln(w, "}")
}

func (s *Server) builderTenant(w http.ResponseWriter, r *http.Request, actor db.User) (db.Tenant, bool) {
	tenant, err := db.ResolveTenant(r.Context(), s.database, actor, r.PathValue("tenant"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, r, http.StatusNotFound, "not_found", "tenant was not found")
		} else {
			writeError(w, r, http.StatusInternalServerError, "internal", "tenant lookup failed")
		}
		return db.Tenant{}, false
	}
	if tenant.Role == "viewer" {
		writeError(w, r, http.StatusForbidden, "forbidden", "builder access is required")
		return db.Tenant{}, false
	}
	return tenant, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any) error {
	if r.URL.RawQuery != "" {
		return fmt.Errorf("query parameters are not accepted")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("body must be one valid JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("body must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"code": code, "message": message, "request_id": r.Context().Value(requestContextKey{}).(string),
	}})
}

func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeLocalState(dir, listen string, user db.User, token string) error {
	if dir == "" {
		return fmt.Errorf("local state directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create local state directory: %w", err)
	}
	path := filepath.Join(dir, "local.json")
	file, err := os.CreateTemp(dir, ".local-*.tmp")
	if err != nil {
		return fmt.Errorf("open local state: %w", err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return fmt.Errorf("protect local state: %w", err)
	}
	err = json.NewEncoder(file).Encode(map[string]any{
		"api_url": "http://" + listen,
		"token":   token,
		"user":    user,
	})
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write local state: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install local state: %w", err)
	}
	return nil
}

func requestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
