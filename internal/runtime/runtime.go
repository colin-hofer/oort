package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"nebulous/internal/db"
	"nebulous/internal/manifest"
	"nebulous/internal/queryexec"
	"nebulous/internal/storage"
)

const sessionCookie = "nebulous_runtime"

type Config struct {
	Database      *sql.DB
	Objects       *storage.Client
	Executable    string
	DatabaseURL   string
	CatalogSecret string
	ExtensionDir  string
	Storage       storage.Config
	HostSuffix    string
	SecureCookies bool
	QueryTimeout  time.Duration
	Log           io.Writer
}

type Server struct{ config Config }

func New(config Config) *Server {
	if config.QueryTimeout <= 0 {
		config.QueryTimeout = 10 * time.Second
	}
	if config.Log == nil {
		config.Log = io.Discard
	}
	return &Server{config: config}
}

func ParseHost(host, suffix string) (appSlug, tenantSlug string, ok bool) {
	if name, _, err := net.SplitHostPort(host); err == nil {
		host = name
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	suffix = strings.Trim(strings.ToLower(suffix), ".")
	tail := "." + suffix
	if suffix == "" || !strings.HasSuffix(host, tail) {
		return "", "", false
	}
	label := strings.TrimSuffix(host, tail)
	appSlug, tenantSlug, ok = strings.Cut(label, "--")
	if !ok || strings.Contains(appSlug, "--") ||
		!validSlug(appSlug) || !validSlug(tenantSlug) {
		return "", "", false
	}
	return appSlug, tenantSlug, true
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	appSlug, tenantSlug, ok := ParseHost(r.Host, s.config.HostSuffix)
	if !ok {
		http.Error(w, "misdirected app host", http.StatusMisdirectedRequest)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/" && r.URL.Query().Has("code") {
		s.exchangeCode(w, r, tenantSlug, appSlug)
		return
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		http.Error(w, "app login required", http.StatusUnauthorized)
		return
	}
	access, err := db.RuntimeDeployment(r.Context(), s.config.Database, tenantSlug, appSlug, cookie.Value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "app login required", http.StatusUnauthorized)
			return
		}
		http.Error(w, "app runtime unavailable", http.StatusInternalServerError)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/runtime/") {
		s.query(w, r, access)
		return
	}
	s.asset(w, r, access)
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, tenantSlug, appSlug string) {
	if len(r.URL.Query()) != 1 || len(r.URL.Query()["code"]) != 1 || r.URL.Query().Get("code") == "" {
		http.Error(w, "invalid app login code", http.StatusBadRequest)
		return
	}
	token, err := db.ExchangeRuntimeLoginCode(r.Context(), s.config.Database, tenantSlug, appSlug,
		r.URL.Query().Get("code"), 12*time.Hour)
	if err != nil {
		http.Error(w, "invalid or expired app login code", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: s.config.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: int((12 * time.Hour).Seconds())})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) query(w http.ResponseWriter, r *http.Request, access db.RuntimeAccess) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/runtime/v1/queries/") {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/runtime/v1/queries/")
	if !validSlug(name) || strings.Contains(name, "/") || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	var input struct {
		Parameters map[string]any `json:"parameters"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must contain parameters"})
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must contain one JSON object"})
		return
	}
	if input.Parameters == nil {
		input.Parameters = map[string]any{}
	}
	revision, err := db.RuntimeQuery(r.Context(), s.config.Database, access, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "query is not granted to this deployment"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query lookup failed"})
		return
	}
	_, types, err := queryexec.Validate(revision.SQL, input.Parameters)
	if err != nil || !sameTypes(types, revision.ParameterTypes) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "query parameters do not match the deployment contract"})
		return
	}
	catalogURL, _, _, err := db.TenantCatalog(s.config.DatabaseURL, s.config.CatalogSecret, access.TenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query configuration failed"})
		return
	}
	queryContext, cancel := context.WithTimeout(r.Context(), s.config.QueryTimeout)
	defer cancel()
	result, err := os.CreateTemp("", "nebulous-runtime-result-*.json")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query staging failed"})
		return
	}
	defer os.Remove(result.Name())
	defer result.Close()
	err = queryexec.Run(queryContext, s.config.Executable, queryexec.Request{
		CatalogURL: catalogURL, DataPath: s.config.Objects.DataPath(access.TenantID),
		ExtensionDir: s.config.ExtensionDir, Storage: s.config.Storage, SQL: revision.SQL,
		Parameters: input.Parameters, ParameterTypes: revision.ParameterTypes, MaxRows: 10_000, MaxBytes: 10 << 20,
	}, result)
	if err != nil {
		fmt.Fprintf(s.config.Log, "runtime query tenant=%s deployment=%s failed: %v\n", access.TenantID, access.DeploymentID, err)
		if errors.Is(err, context.DeadlineExceeded) {
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "query timed out"})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "query failed"})
		return
	}
	if _, err := result.Seek(0, io.SeekStart); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query staging failed"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, result)
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request, access db.RuntimeAccess) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var bundle bytes.Buffer
	read, err := s.config.Objects.Download(r.Context(), access.ObjectKey, &bundle, access.ByteCount)
	if err != nil || read != access.ByteCount {
		http.Error(w, "app bundle unavailable", http.StatusBadGateway)
		return
	}
	checksum := sha256.Sum256(bundle.Bytes())
	if !bytes.Equal(checksum[:], access.Checksum) {
		http.Error(w, "app bundle failed integrity check", http.StatusBadGateway)
		return
	}
	m, err := manifest.Parse(access.Manifest)
	if err != nil {
		http.Error(w, "app deployment is invalid", http.StatusInternalServerError)
		return
	}
	asset, extension, err := manifest.Asset(bundle.Bytes(), m.App.Dir, r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	contentType := mime.TypeByExtension(extension)
	if contentType == "" {
		contentType = http.DetectContentType(asset)
	}
	w.Header().Set("Content-Type", contentType)
	if extension == ".html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	w.Header().Set("ETag", `"`+fmt.Sprintf("%x", access.Checksum)+`"`)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(asset)
	}
}

func validSlug(value string) bool {
	if len(value) < 3 || len(value) > 63 || ((value[0] < 'a' || value[0] > 'z') && (value[0] < '0' || value[0] > '9')) {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	last := value[len(value)-1]
	return last != '-'
}

func sameTypes(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, value := range left {
		if right[name] != value {
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
