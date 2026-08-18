package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"nebulous/internal/db"
	"nebulous/internal/manifest"
	appRuntime "nebulous/internal/runtime"
	"nebulous/internal/storage"
)

type Config struct {
	DatabaseURL            string
	Listen                 string
	LocalAuth              bool
	StateDir               string
	CatalogSecret          string
	ExtensionDir           string
	Storage                storage.Config
	QueryTimeout           time.Duration
	ControlHost            string
	AppHostSuffix          string
	AppScheme              string
	SecureCookies          bool
	OIDCIssuer             string
	OIDCClientID           string
	OIDCSecret             string
	PublicURL              string
	SecretKey              string
	AllowPrivateConnectors bool
	Log                    io.Writer
}

type Server struct {
	database     *sql.DB
	config       Config
	objects      *storage.Client
	executable   string
	runtime      http.Handler
	localSession string
	oidc         *oidcAuth
	logger       *slog.Logger
	queryMu      sync.Mutex
	querySlots   map[string]chan struct{}
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
	} else if config.OIDCIssuer == "" || config.OIDCClientID == "" || config.PublicURL == "" {
		return fmt.Errorf("production authentication requires NEB_OIDC_ISSUER, NEB_OIDC_CLIENT_ID, and NEB_PUBLIC_URL")
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
	var user db.User
	var token, localSession string
	if config.LocalAuth {
		if existing, stateUser, stateErr := readLocalState(config.StateDir); stateErr == nil {
			if principal, authErr := db.AuthenticatePrincipal(ctx, database, existing, "tenants:read"); authErr == nil {
				token, user = existing, principal.User
			} else {
				_ = stateUser
			}
		}
		if token == "" {
			user, token, err = db.CreateLocalIdentity(ctx, database, "local@nebulous.invalid", 24*time.Hour)
			if err != nil {
				return err
			}
		}
		localSession, err = db.CreateControlSession(ctx, database, db.FullPrincipal(user), 24*time.Hour)
		if err != nil {
			return err
		}
		if err := writeLocalState(config.StateDir, config.Listen, user, token); err != nil {
			return err
		}
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
	listenHost, _, _ := net.SplitHostPort(config.Listen)
	if config.ControlHost == "" {
		config.ControlHost = listenHost
	}
	if config.AppHostSuffix == "" {
		config.AppHostSuffix = "apps.localhost"
	}
	if config.AppScheme == "" {
		config.AppScheme = "http"
	}
	server := &Server{database: database, config: config, objects: objects, executable: executable,
		localSession: localSession, querySlots: map[string]chan struct{}{}, logger: slog.New(slog.NewJSONHandler(config.Log, nil))}
	if !config.LocalAuth {
		server.oidc, err = newOIDCAuth(ctx, config)
		if err != nil {
			return err
		}
	}
	server.runtime = appRuntime.New(appRuntime.Config{
		Database: database, Objects: objects, Executable: executable, DatabaseURL: config.DatabaseURL,
		CatalogSecret: config.CatalogSecret, ExtensionDir: config.ExtensionDir, Storage: config.Storage,
		HostSuffix: config.AppHostSuffix, SecureCookies: config.SecureCookies,
		QueryTimeout: config.QueryTimeout, Log: config.Log,
	})
	httpServer := &http.Server{
		Handler:           newHandler(server),
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
	if server.runtime == nil && server.objects != nil && server.executable != "" && server.config.AppHostSuffix != "" {
		server.runtime = appRuntime.New(appRuntime.Config{
			Database: server.database, Objects: server.objects, Executable: server.executable,
			DatabaseURL: server.config.DatabaseURL, CatalogSecret: server.config.CatalogSecret,
			ExtensionDir: server.config.ExtensionDir, Storage: server.config.Storage,
			HostSuffix: server.config.AppHostSuffix, SecureCookies: server.config.SecureCookies,
			QueryTimeout: server.config.QueryTimeout, Log: server.config.Log,
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /auth/login", server.startOIDCLogin)
	mux.HandleFunc("GET /auth/callback", server.finishOIDCLogin)
	mux.HandleFunc("POST /v1/auth/cli-exchange", server.exchangeCLILogin)
	mux.Handle("GET /v1/me", server.authenticate("tenants:read", http.HandlerFunc(server.me)))
	mux.Handle("DELETE /v1/tokens/current", server.authenticate("tokens:write", http.HandlerFunc(server.revokeCurrentToken)))
	mux.Handle("POST /v1/control-session", server.authenticate("tenants:read", http.HandlerFunc(server.createControlSession)))
	mux.HandleFunc("DELETE /v1/control-session", server.clearControlSession)
	mux.Handle("POST /v1/tenants", server.authenticate("tenants:write", http.HandlerFunc(server.createTenant)))
	mux.Handle("GET /v1/tenants", server.authenticate("tenants:read", http.HandlerFunc(server.listTenants)))
	mux.Handle("GET /v1/tenants/{tenant}/dashboard", server.authenticate("dashboard:read", http.HandlerFunc(server.dashboard)))
	mux.Handle("GET /v1/tenants/{tenant}/members", server.authenticate("members:read", http.HandlerFunc(server.listMembers)))
	mux.Handle("POST /v1/tenants/{tenant}/members", server.authenticate("members:write", http.HandlerFunc(server.addMember)))
	mux.Handle("PATCH /v1/tenants/{tenant}/members/{user}", server.authenticate("members:write", http.HandlerFunc(server.changeMemberRole)))
	mux.Handle("DELETE /v1/tenants/{tenant}/members/{user}", server.authenticate("members:write", http.HandlerFunc(server.removeMember)))
	mux.Handle("GET /v1/tenants/{tenant}/tokens", server.authenticate("tokens:read", http.HandlerFunc(server.listTokens)))
	mux.Handle("POST /v1/tenants/{tenant}/tokens", server.authenticate("tokens:write", http.HandlerFunc(server.createToken)))
	mux.Handle("DELETE /v1/tenants/{tenant}/tokens/{token}", server.authenticate("tokens:write", http.HandlerFunc(server.revokeToken)))
	mux.Handle("POST /v1/tenants/{tenant}/dataset-uploads", server.authenticate("datasets:write", http.HandlerFunc(server.createDatasetUpload)))
	mux.Handle("PUT /v1/tenants/{tenant}/dataset-uploads/{upload}/content", server.authenticate("datasets:write", http.HandlerFunc(server.uploadDatasetContent)))
	mux.Handle("POST /v1/tenants/{tenant}/dataset-uploads/{upload}/complete", server.authenticate("datasets:write", http.HandlerFunc(server.completeDatasetUpload)))
	mux.Handle("GET /v1/tenants/{tenant}/datasets", server.authenticate("datasets:read", http.HandlerFunc(server.listDatasets)))
	mux.Handle("GET /v1/tenants/{tenant}/datasets/{dataset}", server.authenticate("datasets:read", http.HandlerFunc(server.getDataset)))
	mux.Handle("GET /v1/tenants/{tenant}/datasets/{dataset}/syncs", server.authenticate("datasets:read", http.HandlerFunc(server.listDatasetSyncs)))
	mux.Handle("GET /v1/tenants/{tenant}/datasets/{dataset}/sample", server.authenticate("datasets:read", http.HandlerFunc(server.sampleDataset)))
	mux.Handle("POST /v1/tenants/{tenant}/queries/validate", server.authenticate("queries:write", http.HandlerFunc(server.validateQuery)))
	mux.Handle("POST /v1/tenants/{tenant}/queries/execute", server.authenticate("queries:run", http.HandlerFunc(server.executeDraftQuery)))
	mux.Handle("GET /v1/tenants/{tenant}/queries", server.authenticate("queries:read", http.HandlerFunc(server.listQueries)))
	mux.Handle("GET /v1/tenants/{tenant}/queries/{query}", server.authenticate("queries:read", http.HandlerFunc(server.getQuery)))
	mux.Handle("PUT /v1/tenants/{tenant}/queries/{query}", server.authenticate("queries:write", http.HandlerFunc(server.saveQuery)))
	mux.Handle("POST /v1/tenants/{tenant}/queries/{query}/execute", server.authenticate("queries:run", http.HandlerFunc(server.executeSavedQuery)))
	mux.Handle("POST /v1/tenants/{tenant}/deployments", server.authenticate("apps:write", http.HandlerFunc(server.createDeployment)))
	mux.Handle("POST /v1/tenants/{tenant}/deployments/{deployment}/complete", server.authenticate("apps:write", http.HandlerFunc(server.completeDeployment)))
	mux.Handle("GET /v1/tenants/{tenant}/deployments/{deployment}", server.authenticate("apps:read", http.HandlerFunc(server.getDeployment)))
	mux.Handle("POST /v1/tenants/{tenant}/deployments/{deployment}/rollback", server.authenticate("apps:write", http.HandlerFunc(server.rollbackDeployment)))
	mux.Handle("POST /v1/tenants/{tenant}/apps/{app}/login-link", server.authenticate("apps:read", http.HandlerFunc(server.appLoginLink)))
	mux.Handle("GET /v1/tenants/{tenant}/apps", server.authenticate("apps:read", http.HandlerFunc(server.listApps)))
	mux.Handle("GET /v1/tenants/{tenant}/apps/{app}", server.authenticate("apps:read", http.HandlerFunc(server.getApp)))
	mux.Handle("GET /v1/tenants/{tenant}/deployments", server.authenticate("apps:read", http.HandlerFunc(server.listDeployments)))
	mux.Handle("GET /v1/tenants/{tenant}/jobs", server.authenticate("jobs:read", http.HandlerFunc(server.listJobs)))
	mux.Handle("GET /v1/tenants/{tenant}/jobs/{job}", server.authenticate("jobs:read", http.HandlerFunc(server.getJob)))
	mux.Handle("GET /v1/tenants/{tenant}/jobs/{job}/logs", server.authenticate("jobs:read", http.HandlerFunc(server.jobLogs)))
	mux.Handle("POST /v1/tenants/{tenant}/jobs/{job}/cancel", server.authenticate("jobs:write", http.HandlerFunc(server.cancelJob)))
	mux.Handle("GET /v1/tenants/{tenant}/connectors", server.authenticate("connectors:read", http.HandlerFunc(server.listConnectors)))
	mux.Handle("POST /v1/tenants/{tenant}/connectors", server.authenticate("connectors:write", http.HandlerFunc(server.createConnector)))
	mux.Handle("GET /v1/tenants/{tenant}/connectors/{connector}", server.authenticate("connectors:read", http.HandlerFunc(server.getConnector)))
	mux.Handle("PUT /v1/tenants/{tenant}/connectors/{connector}", server.authenticate("connectors:write", http.HandlerFunc(server.updateConnector)))
	mux.Handle("DELETE /v1/tenants/{tenant}/connectors/{connector}", server.authenticate("connectors:write", http.HandlerFunc(server.deleteConnector)))
	mux.Handle("POST /v1/tenants/{tenant}/connectors/{connector}/sync", server.authenticate("connectors:write", http.HandlerFunc(server.syncConnector)))
	mux.Handle("/", server.web())
	return requestIDs(requestLog(server.logger, server.surfaces(mux)))
}

type userContextKey struct{}
type principalContextKey struct{}
type requestContextKey struct{}

func requestIDs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestID()
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestContextKey{}, id)))
	})
}

type responseStatus struct {
	http.ResponseWriter
	status int
}

func (w *responseStatus) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseStatus) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

func requestLog(logger *slog.Logger, next http.Handler) http.Handler {
	if logger == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		response := &responseStatus{ResponseWriter: w}
		next.ServeHTTP(response, r)
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		logger.Info("http request", "request_id", requestIDFrom(r), "method", r.Method,
			"path", r.URL.Path, "status", status, "duration_ms", time.Since(started).Milliseconds())
	})
}

func (s *Server) authenticate(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authorization, "Bearer ")
		fromCookie := false
		if !strings.HasPrefix(authorization, "Bearer ") || token == "" || strings.Contains(token, " ") {
			cookie, err := r.Cookie(controlSessionCookie)
			if err == nil {
				token, fromCookie = cookie.Value, true
			}
		}
		if token == "" {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "provide a valid Bearer token")
			return
		}
		if fromCookie && r.Method != http.MethodGet && r.Method != http.MethodHead &&
			r.Header.Get("X-Nebulous-Request") != "browser" {
			writeError(w, r, http.StatusForbidden, "csrf_rejected", "browser mutations require the Nebulous request header")
			return
		}
		var principal db.Principal
		var err error
		if fromCookie {
			principal, err = db.AuthenticateControlSession(r.Context(), s.database, token, scope)
		} else {
			principal, err = db.AuthenticatePrincipal(r.Context(), s.database, token, scope)
		}
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, r, http.StatusUnauthorized, "unauthenticated", "the API token is invalid or expired")
				return
			}
			writeError(w, r, http.StatusInternalServerError, "internal", "authentication failed")
			return
		}
		ctx := context.WithValue(r.Context(), userContextKey{}, principal.User)
		ctx = context.WithValue(ctx, principalContextKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "identity lookup does not accept query parameters")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": r.Context().Value(userContextKey{}).(db.User),
		"principal": r.Context().Value(principalContextKey{}).(db.Principal)})
}

func (s *Server) createControlSession(w http.ResponseWriter, r *http.Request) {
	if r.Context().Value(principalContextKey{}).(db.Principal).Kind != "token" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "a Bearer token is required to start a browser session")
		return
	}
	if err := decodeJSON(w, r, &struct{}{}); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "body must be an empty JSON object")
		return
	}
	principal := r.Context().Value(principalContextKey{}).(db.Principal)
	token, err := db.CreateControlSession(r.Context(), s.database, principal, 12*time.Hour)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "browser session creation failed")
		return
	}
	s.setControlCookie(w, token, 12*time.Hour)
	writeJSON(w, http.StatusCreated, map[string]any{"user": r.Context().Value(userContextKey{}).(db.User)})
}

func (s *Server) clearControlSession(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(controlSessionCookie); err == nil {
		_ = db.DeleteControlSession(r.Context(), s.database, cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: controlSessionCookie, Path: "/", HttpOnly: true,
		Secure: s.config.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setControlCookie(w http.ResponseWriter, token string, lifetime time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: controlSessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: s.config.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: int(lifetime.Seconds())})
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	if principal := r.Context().Value(principalContextKey{}).(db.Principal); principal.TenantID != nil {
		writeError(w, r, http.StatusForbidden, "forbidden", "tenant-scoped credentials cannot create tenants")
		return
	}
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
	if principal := r.Context().Value(principalContextKey{}).(db.Principal); principal.TenantID != nil {
		filtered := tenants[:0]
		for _, tenant := range tenants {
			if tenant.ID == *principal.TenantID {
				filtered = append(filtered, tenant)
			}
		}
		tenants = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": tenants})
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "dashboard lookup does not accept query parameters")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	dashboard, err := db.LoadDashboard(r.Context(), s.database, tenant)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "dashboard loading failed")
		return
	}
	for index := range dashboard.Apps {
		dashboard.Apps[index].URL = s.appURL(dashboard.Apps[index].Slug, tenant.Slug, "")
	}
	writeJSON(w, http.StatusOK, dashboard)
}

func (s *Server) createDatasetUpload(w http.ResponseWriter, r *http.Request) {
	if s.objects == nil {
		writeError(w, r, http.StatusServiceUnavailable, "not_configured", "dataset storage is not configured")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
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
		if errors.Is(err, db.ErrQuota) {
			writeError(w, r, http.StatusUnprocessableEntity, "storage_quota_exceeded", "the tenant has reached its 10 GiB stored-data limit")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal", "dataset upload creation failed")
		return
	}
	job, err := db.GetJob(r.Context(), s.database, tenant.ID, upload.JobID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "dataset job lookup failed")
		return
	}
	response := map[string]any{"dataset": upload.Dataset, "upload": upload.Sync, "job": job}
	if upload.Sync.Status == "awaiting_upload" {
		putURL, headers, err := s.objects.PresignPut(upload.Sync.ObjectKey, upload.Sync.ByteCount, 15*time.Minute)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal", "signing the dataset upload failed")
			return
		}
		response["content"] = map[string]any{"url": putURL, "headers": headers}
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) completeDatasetUpload(w http.ResponseWriter, r *http.Request) {
	if !uuidPattern.MatchString(r.PathValue("upload")) {
		writeError(w, r, http.StatusNotFound, "not_found", "upload was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	if err := decodeJSON(w, r, &struct{}{}); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "body must be an empty JSON object")
		return
	}
	upload, err := db.CompleteDatasetUpload(r.Context(), s.database, tenant.ID, r.PathValue("upload"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, r, http.StatusNotFound, "not_found", "upload was not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal", "dataset upload completion failed")
		return
	}
	job, err := db.GetJobBySync(r.Context(), s.database, tenant.ID, upload.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "dataset job lookup failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"upload": upload, "job": job})
}

func (s *Server) uploadDatasetContent(w http.ResponseWriter, r *http.Request) {
	if s.objects == nil {
		writeError(w, r, http.StatusServiceUnavailable, "not_configured", "dataset storage is not configured")
		return
	}
	if !uuidPattern.MatchString(r.PathValue("upload")) {
		writeError(w, r, http.StatusNotFound, "not_found", "upload was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	upload, err := db.GetSyncRun(r.Context(), s.database, tenant.ID, r.PathValue("upload"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "upload was not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "upload lookup failed")
		return
	}
	if upload.Status != "awaiting_upload" {
		writeError(w, r, http.StatusConflict, "upload_closed", "this upload is no longer accepting content")
		return
	}
	if r.ContentLength != upload.ByteCount {
		writeError(w, r, http.StatusBadRequest, "invalid_upload_size", fmt.Sprintf("upload must be exactly %d bytes", upload.ByteCount))
		return
	}
	// ponytail: proxy browser uploads until every supported S3 deployment has a proven CORS policy.
	body := http.MaxBytesReader(w, r.Body, upload.ByteCount)
	if err := s.objects.Upload(r.Context(), upload.ObjectKey, body, upload.ByteCount); err != nil {
		writeError(w, r, http.StatusBadGateway, "upload_failed", "storing the dataset upload failed")
		return
	}
	upload, err = db.CompleteDatasetUpload(r.Context(), s.database, tenant.ID, upload.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "dataset upload completion failed")
		return
	}
	job, err := db.GetJobBySync(r.Context(), s.database, tenant.ID, upload.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "dataset job lookup failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"upload": upload, "job": job})
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	if s.objects == nil {
		writeError(w, r, http.StatusServiceUnavailable, "not_configured", "app storage is not configured")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	var input struct {
		Manifest       json.RawMessage `json:"manifest"`
		Checksum       string          `json:"checksum"`
		ByteCount      int64           `json:"byte_count"`
		IdempotencyKey string          `json:"idempotency_key"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	m, err := manifest.Parse(input.Manifest)
	if err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_manifest", err.Error())
		return
	}
	checksum, err := hex.DecodeString(input.Checksum)
	if err != nil || len(checksum) != sha256.Size {
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_checksum", "checksum must be a SHA-256 hex value")
		return
	}
	if input.ByteCount < 1 || input.ByteCount > manifest.MaxBundleBytes {
		writeError(w, r, http.StatusRequestEntityTooLarge, "bundle_too_large", "bundle must be between 1 byte and 25 MiB")
		return
	}
	if len(input.IdempotencyKey) < 1 || len(input.IdempotencyKey) > 200 {
		writeError(w, r, http.StatusBadRequest, "invalid_idempotency_key", "idempotency_key must be 1-200 characters")
		return
	}
	deployment, err := db.CreateDeployment(r.Context(), s.database, tenant, actor, m, checksum,
		input.ByteCount, input.IdempotencyKey, r.Context().Value(requestContextKey{}).(string))
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			writeError(w, r, http.StatusConflict, "idempotency_conflict", "that idempotency key belongs to a different deployment")
			return
		}
		if errors.Is(err, db.ErrQuota) {
			writeError(w, r, http.StatusUnprocessableEntity, "storage_quota_exceeded", "the tenant has reached its 10 GiB stored-data limit")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "internal", "deployment creation failed")
		return
	}
	job, err := db.GetJobByDeployment(r.Context(), s.database, tenant.ID, deployment.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "deployment job lookup failed")
		return
	}
	response := map[string]any{"deployment": deployment, "job": job}
	if deployment.Status == "awaiting_upload" {
		putURL, headers, err := s.objects.PresignPut(deployment.ObjectKey, deployment.ByteCount, 15*time.Minute)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "internal", "signing the app upload failed")
			return
		}
		response["upload"] = map[string]any{"url": putURL, "headers": headers}
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) completeDeployment(w http.ResponseWriter, r *http.Request) {
	if !uuidPattern.MatchString(r.PathValue("deployment")) {
		writeError(w, r, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	if err := decodeJSON(w, r, &struct{}{}); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "body must be an empty JSON object")
		return
	}
	deployment, err := db.CompleteDeploymentUpload(r.Context(), s.database, tenant.ID, r.PathValue("deployment"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "deployment upload completion failed")
		return
	}
	job, err := db.GetJobByDeployment(r.Context(), s.database, tenant.ID, deployment.ID)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "deployment job lookup failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"deployment": deployment, "job": job})
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	if !uuidPattern.MatchString(r.PathValue("deployment")) {
		writeError(w, r, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	deployment, err := db.GetDeployment(r.Context(), s.database, tenant.ID, r.PathValue("deployment"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "deployment lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployment": deployment})
}

func (s *Server) rollbackDeployment(w http.ResponseWriter, r *http.Request) {
	if !uuidPattern.MatchString(r.PathValue("deployment")) {
		writeError(w, r, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.developerTenant(w, r, actor)
	if !ok {
		return
	}
	if err := decodeJSON(w, r, &struct{}{}); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "body must be an empty JSON object")
		return
	}
	deployment, err := db.RollbackDeployment(r.Context(), s.database, tenant, actor,
		r.PathValue("deployment"), r.Context().Value(requestContextKey{}).(string))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "published deployment was not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "deployment rollback failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployment": deployment, "app_url": s.appURL(deployment.AppSlug, tenant.Slug, "")})
}

func (s *Server) appLoginLink(w http.ResponseWriter, r *http.Request) {
	if !slugPattern.MatchString(r.PathValue("app")) {
		writeError(w, r, http.StatusNotFound, "not_found", "app was not found")
		return
	}
	actor := r.Context().Value(userContextKey{}).(db.User)
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return
	}
	if err := decodeJSON(w, r, &struct{}{}); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "body must be an empty JSON object")
		return
	}
	code, err := db.CreateRuntimeLoginCode(r.Context(), s.database, tenant, actor, r.PathValue("app"), time.Minute)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, r, http.StatusNotFound, "not_found", "published app was not found")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "internal", "app login link creation failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"url": s.appURL(r.PathValue("app"), tenant.Slug, code)})
}

func (s *Server) surfaces(control http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := requestHost(r.Host)
		suffix := strings.Trim(s.config.AppHostSuffix, ".")
		if suffix != "" && strings.HasSuffix(strings.ToLower(host), "."+strings.ToLower(suffix)) {
			if s.runtime == nil {
				http.Error(w, "app runtime is not configured", http.StatusMisdirectedRequest)
				return
			}
			s.runtime.ServeHTTP(w, r)
			return
		}
		localAlias := s.config.LocalAuth && loopbackHost(host) && loopbackHost(s.config.ControlHost)
		if s.config.ControlHost != "" && !strings.EqualFold(host, s.config.ControlHost) && !localAlias {
			http.Error(w, "misdirected control-plane host", http.StatusMisdirectedRequest)
			return
		}
		control.ServeHTTP(w, r)
	})
}

func (s *Server) appURL(appSlug, tenantSlug, code string) string {
	host := appSlug + "--" + tenantSlug + "." + strings.Trim(s.config.AppHostSuffix, ".")
	_, port, err := net.SplitHostPort(s.config.Listen)
	if err == nil && port != "" && !((s.config.AppScheme == "http" && port == "80") || (s.config.AppScheme == "https" && port == "443")) {
		host = net.JoinHostPort(host, port)
	}
	target := url.URL{Scheme: s.config.AppScheme, Host: host, Path: "/"}
	if code != "" {
		target.RawQuery = url.Values{"code": []string{code}}.Encode()
	}
	return target.String()
}

func requestHost(value string) string {
	if host, _, err := net.SplitHostPort(value); err == nil {
		return strings.TrimSuffix(host, ".")
	}
	return strings.TrimSuffix(value, ".")
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) developerTenant(w http.ResponseWriter, r *http.Request, actor db.User) (db.Tenant, bool) {
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return db.Tenant{}, false
	}
	if tenant.Role == "viewer" {
		writeError(w, r, http.StatusForbidden, "forbidden", "developer access is required")
		return db.Tenant{}, false
	}
	return tenant, true
}

func (s *Server) memberTenant(w http.ResponseWriter, r *http.Request, actor db.User) (db.Tenant, bool) {
	tenant, err := db.ResolveTenant(r.Context(), s.database, actor, r.PathValue("tenant"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, r, http.StatusNotFound, "not_found", "tenant was not found")
		} else {
			writeError(w, r, http.StatusInternalServerError, "internal", "tenant lookup failed")
		}
		return db.Tenant{}, false
	}
	if principal, ok := r.Context().Value(principalContextKey{}).(db.Principal); ok &&
		principal.TenantID != nil && *principal.TenantID != tenant.ID {
		writeError(w, r, http.StatusNotFound, "not_found", "tenant was not found")
		return db.Tenant{}, false
	}
	return tenant, true
}

func (s *Server) adminTenant(w http.ResponseWriter, r *http.Request, actor db.User) (db.Tenant, bool) {
	tenant, ok := s.memberTenant(w, r, actor)
	if !ok {
		return db.Tenant{}, false
	}
	if tenant.Role != "owner" && tenant.Role != "admin" {
		writeError(w, r, http.StatusForbidden, "forbidden", "administrator access is required")
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

func readLocalState(dir string) (string, db.User, error) {
	contents, err := os.ReadFile(filepath.Join(dir, "local.json"))
	if err != nil {
		return "", db.User{}, err
	}
	var state struct {
		Token string  `json:"token"`
		User  db.User `json:"user"`
	}
	if err := json.Unmarshal(contents, &state); err != nil || state.Token == "" {
		return "", db.User{}, fmt.Errorf("invalid local state")
	}
	return state.Token, state.User, nil
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
