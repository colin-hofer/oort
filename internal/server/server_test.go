package server

import (
	"bytes"
	"context"
	stdsql "database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	nebulouscli "nebulous/internal/cli"
	"nebulous/internal/db"
	"nebulous/internal/jobs"
	"nebulous/internal/queryexec"
	"nebulous/internal/storage"
)

func TestLocalAuthRequiresLoopback(t *testing.T) {
	if loopbackAddress("0.0.0.0:8080") || !loopbackAddress("127.0.0.1:8080") || !loopbackAddress("[::1]:8080") {
		t.Fatal("loopback listener validation failed")
	}
	if err := Run(context.Background(), Config{Listen: "0.0.0.0:8080", LocalAuth: true}); err == nil {
		t.Fatal("local authentication accepted a public listener")
	}
	if err := Run(context.Background(), Config{Listen: "127.0.0.1:8080"}); err == nil {
		t.Fatal("server started without production authentication")
	}
}

func TestLocalStatePermissions(t *testing.T) {
	dir := t.TempDir()
	if err := writeLocalState(dir, "127.0.0.1:8080", db.User{ID: "user", Email: "local@test.invalid"}, "secret"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "local.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("local state mode is %o; want 600", info.Mode().Perm())
	}
}

func TestDashboardAssetsAndLocalSession(t *testing.T) {
	server := &Server{localSession: "local-token"}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	request.RemoteAddr = "127.0.0.1:43210"
	response := httptest.NewRecorder()
	server.web().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Nebulous") {
		t.Fatalf("dashboard returned %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("dashboard CSP is missing: %q", response.Header().Get("Content-Security-Policy"))
	}
	cookie := response.Header().Get("Set-Cookie")
	if !strings.Contains(cookie, controlSessionCookie+"=local-token") ||
		!strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Strict") {
		t.Fatalf("local dashboard session is not protected: %q", cookie)
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1/favicon.svg", nil)
	request.RemoteAddr = "192.0.2.10:43210"
	response = httptest.NewRecorder()
	server.web().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "<svg") {
		t.Fatalf("dashboard asset returned %d", response.Code)
	}
	if response.Header().Get("Set-Cookie") != "" {
		t.Fatal("dashboard issued its local session to a non-loopback request")
	}
}

func TestLoopbackRemote(t *testing.T) {
	if !loopbackRemote("127.0.0.1:8080") || !loopbackRemote("[::1]:8080") || loopbackRemote("192.0.2.1:8080") {
		t.Fatal("remote loopback validation failed")
	}
}

func TestLocalControlHostAliases(t *testing.T) {
	control := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	local := (&Server{config: Config{LocalAuth: true, ControlHost: "127.0.0.1"}}).surfaces(control)
	for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		request := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		response := httptest.NewRecorder()
		local.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("local control host %q returned %d", host, response.Code)
		}
	}

	for name, server := range map[string]*Server{
		"local rebinding":  {config: Config{LocalAuth: true, ControlHost: "127.0.0.1"}},
		"production alias": {config: Config{ControlHost: "127.0.0.1"}},
	} {
		request := httptest.NewRequest(http.MethodGet, "http://localhost:8080/", nil)
		if name == "local rebinding" {
			request.Host = "attacker.example:8080"
		}
		response := httptest.NewRecorder()
		server.surfaces(control).ServeHTTP(response, request)
		if response.Code != http.StatusMisdirectedRequest {
			t.Fatalf("%s returned %d", name, response.Code)
		}
	}
}

func TestTenantBoundary(t *testing.T) {
	if os.Getenv("NEB_INTEGRATION") != "1" {
		t.Skip("set NEB_INTEGRATION=1 to run the PostgreSQL-backed tenant test")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	compose := filepath.Join(root, "internal", "cli", "compose.yaml")
	command := exec.CommandContext(ctx, "docker", "compose", "-f", compose, "-p", "nebulous", "up", "-d", "--wait", "postgres")
	command.Dir, command.Stdout, command.Stderr = root, os.Stderr, os.Stderr
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	databaseURL := envTest("NEB_DATABASE_URL", "postgresql://nebulous:nebulous-local@127.0.0.1:55432/nebulous?sslmode=disable")
	database, err := db.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	userA, tokenA, err := db.CreateLocalIdentity(ctx, database, "a-"+suffix+"@test.invalid", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	userB, tokenB, err := db.CreateLocalIdentity(ctx, database, "b-"+suffix+"@test.invalid", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AuthenticatePrincipal(ctx, database, tokenA, "tenants:write"); err != nil {
		t.Fatalf("authenticate local API token: %v", err)
	}
	httpServer := httptest.NewServer(New(database))
	defer httpServer.Close()

	tenantA := createTenant(t, httpServer.URL, tokenA, "a-"+suffix)
	tenantB := createTenant(t, httpServer.URL, tokenB, "b-"+suffix)
	scopedSecret, _, err := db.CreateAPIToken(ctx, database, userA, &tenantA.ID, "scoped-browser",
		[]string{"tenants:read", "dashboard:read"}, time.Now().Add(time.Hour), requestID())
	if err != nil {
		t.Fatal(err)
	}
	sessionResponse := request(t, http.MethodPost, httpServer.URL+"/v1/control-session", scopedSecret, `{}`)
	sessionCookies := sessionResponse.Cookies()
	sessionResponse.Body.Close()
	if sessionResponse.StatusCode != http.StatusCreated || len(sessionCookies) != 1 {
		t.Fatalf("scoped browser session returned %d with %d cookies", sessionResponse.StatusCode, len(sessionCookies))
	}
	scopedRequest, _ := http.NewRequest(http.MethodGet, httpServer.URL+"/v1/tenants/"+tenantB.Slug+"/dashboard", nil)
	scopedRequest.AddCookie(sessionCookies[0])
	scopedResponse, err := http.DefaultClient.Do(scopedRequest)
	if err != nil {
		t.Fatal(err)
	}
	scopedResponse.Body.Close()
	if scopedResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("tenant-scoped browser session crossed boundary: %d", scopedResponse.StatusCode)
	}
	listed := listTenants(t, httpServer.URL, tokenA)
	if len(listed) != 1 || listed[0].ID != tenantA.ID || listed[0].ID == tenantB.ID {
		t.Fatalf("tenant A listing crossed boundary: %+v", listed)
	}
	response := request(t, http.MethodGet, httpServer.URL+"/v1/tenants?user_id="+userB.ID, tokenA, "")
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("user-ID query tampering returned %d", response.StatusCode)
	}
	response.Body.Close()
	response = request(t, http.MethodPost, httpServer.URL+"/v1/tenants", tokenA, fmt.Sprintf(`{"slug":%q}`, tenantB.Slug))
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("cross-tenant slug reuse returned %d", response.StatusCode)
	}
	response.Body.Close()
	response = request(t, http.MethodPost, httpServer.URL+"/v1/tenants", tokenA,
		fmt.Sprintf(`{"slug":"c-%s","user_id":%q}`, suffix, userB.ID))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("body identity tampering returned %d", response.StatusCode)
	}
	response.Body.Close()
	response = request(t, http.MethodGet, httpServer.URL+"/v1/tenants", tokenA+"x", "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("changed token returned %d", response.StatusCode)
	}
	response.Body.Close()
	if _, err := database.ExecContext(ctx, `INSERT INTO audit_events
		(id, tenant_id, actor_user_id, action, resource_type, resource_id, request_id)
		VALUES ($1, $2, $3, 'cross-tenant', 'tenant', $2, $4)`,
		requestID(), tenantB.ID, userA.ID, requestID()); err == nil {
		t.Fatal("database accepted a cross-tenant actor relationship")
	}
}

func TestUploadToQuery(t *testing.T) {
	if os.Getenv("NEB_UPLOAD_INTEGRATION") != "1" {
		t.Skip("set NEB_UPLOAD_INTEGRATION=1 to run the upload-to-query test")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	compose := filepath.Join(root, "internal", "cli", "compose.yaml")
	command := exec.CommandContext(ctx, "docker", "compose", "-f", compose, "-p", "nebulous", "up", "-d", "--wait")
	command.Dir, command.Stdout, command.Stderr = root, os.Stderr, os.Stderr
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	databaseURL := envTest("NEB_DATABASE_URL", "postgresql://nebulous:nebulous-local@127.0.0.1:55432/nebulous?sslmode=disable")
	database, err := db.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	platform := filepath.Join(tempDir, "neb")
	command = exec.CommandContext(ctx, "go", "build", "-o", platform, "./cmd/neb")
	command.Dir, command.Stdout, command.Stderr = root, os.Stderr, os.Stderr
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	storageConfig := storage.Config{
		Endpoint: "http://127.0.0.1:" + envTest("NEB_LOCAL_S3_PORT", "9000"),
		Region:   "us-east-1", AccessKey: envTest("NEB_LOCAL_S3_ACCESS_KEY", "nebulous"),
		SecretKey: envTest("NEB_LOCAL_S3_SECRET_KEY", "nebulous-local-secret"),
		Bucket:    envTest("NEB_LOCAL_S3_BUCKET", "nebulous"),
	}
	objects, err := storage.New(storageConfig)
	if err != nil {
		t.Fatal(err)
	}
	catalogSecret := envTest("NEB_CATALOG_SECRET", "nebulous-local-catalog-secret")
	extensionDir := filepath.Join(tempDir, "extensions")
	if err := queryexec.EnsureExtensions(ctx, extensionDir); err != nil {
		t.Fatal(err)
	}
	workerContext, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- jobs.Run(workerContext, jobs.Config{
			DatabaseURL: databaseURL, CatalogSecret: catalogSecret, ExtensionDir: extensionDir,
			Storage: storageConfig, Log: os.Stderr,
		})
	}()
	httpServer := httptest.NewServer(newHandler(&Server{
		database: database, executable: platform, objects: objects,
		config: Config{DatabaseURL: databaseURL, CatalogSecret: catalogSecret, ExtensionDir: extensionDir,
			Storage: storageConfig, QueryTimeout: 20 * time.Second, Log: os.Stderr,
			Listen: "127.0.0.1:80", AppHostSuffix: "apps.example.test", AppScheme: "http"},
	}))
	defer httpServer.Close()

	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	_, tokenA, err := db.CreateLocalIdentity(ctx, database, "upload-a-"+suffix+"@test.invalid", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, tokenB, err := db.CreateLocalIdentity(ctx, database, "upload-b-"+suffix+"@test.invalid", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tenantA := createTenant(t, httpServer.URL, tokenA, "u-a-"+suffix)
	tenantB := createTenant(t, httpServer.URL, tokenB, "u-b-"+suffix)
	datasetSlug := "customers-" + suffix
	good := []byte("id,name\n1,Ada\n2,Grace\n")
	stateHome := filepath.Join(tempDir, "state")
	if err := os.MkdirAll(filepath.Join(stateHome, "nebulous"), 0o700); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(map[string]any{
		"api_url": httpServer.URL, "token": tokenA,
		"user": map[string]string{"id": "cli-test", "email": "cli@test.invalid"},
	})
	if err := os.WriteFile(filepath.Join(stateHome, "nebulous", "local.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	csvFile := filepath.Join(tempDir, "customers.csv")
	if err := os.WriteFile(csvFile, good, 0o600); err != nil {
		t.Fatal(err)
	}
	var cliOutput, cliDiagnostics bytes.Buffer
	if err := nebulouscli.Run(ctx, []string{"dataset", "upload", csvFile, "--name", datasetSlug,
		"--tenant", tenantA.Slug, "--json"}, &cliOutput, &cliDiagnostics); err != nil {
		t.Fatalf("CLI upload failed: %v: %s", err, cliDiagnostics.String())
	}
	var uploaded struct {
		Job integrationJob `json:"job"`
	}
	decodeCLIData(t, cliOutput.Bytes(), &uploaded)
	succeeded := uploaded.Job
	if succeeded.Status != "succeeded" || succeeded.RowCount == nil || *succeeded.RowCount != 2 {
		message := ""
		if succeeded.Error != nil {
			message = *succeeded.Error
		}
		t.Fatalf("unexpected successful sync: %+v: %s", succeeded, message)
	}
	cliOutput.Reset()
	if err := nebulouscli.Run(ctx, []string{"job", "wait", succeeded.ID, "--tenant", tenantA.Slug, "--json"},
		&cliOutput, &cliDiagnostics); err != nil {
		t.Fatalf("CLI job wait failed: %v", err)
	}
	var waited struct {
		Job integrationJob `json:"job"`
	}
	decodeCLIData(t, cliOutput.Bytes(), &waited)
	if waited.Job.ID != succeeded.ID || waited.Job.Status != "succeeded" {
		t.Fatalf("unexpected waited job: %+v", waited.Job)
	}
	response := request(t, http.MethodGet, httpServer.URL+"/v1/tenants/"+tenantA.Slug+"/runs/"+succeeded.ID, tokenA, "")
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("removed runs endpoint returned %d", response.StatusCode)
	}
	response = request(t, http.MethodGet, httpServer.URL+"/v1/tenants/"+tenantB.Slug+"/jobs/"+succeeded.ID, tokenB, "")
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant job lookup returned %d", response.StatusCode)
	}

	queryName := "customer-query-" + suffix
	querySQL := fmt.Sprintf(`SELECT name FROM %q ORDER BY id LIMIT $limit`, datasetSlug)
	queryFile := filepath.Join(tempDir, "customers.sql")
	if err := os.WriteFile(queryFile, []byte(querySQL), 0o600); err != nil {
		t.Fatal(err)
	}
	cliOutput.Reset()
	cliDiagnostics.Reset()
	if err := nebulouscli.Run(ctx, []string{"query", "run", queryFile,
		"--param", "limit=10", "--tenant", tenantA.Slug, "--json"}, &cliOutput, &cliDiagnostics); err != nil {
		t.Fatalf("CLI query failed: %v: %s", err, cliDiagnostics.String())
	}
	var first integrationQuery
	decodeCLIData(t, cliOutput.Bytes(), &first)
	if len(first.Result.Rows) != 2 || first.Result.SnapshotID == 0 {
		t.Fatalf("unexpected first query result: %+v", first)
	}
	cliOutput.Reset()
	if err := nebulouscli.Run(ctx, []string{"query", "save", queryFile, "--name", queryName,
		"--param", "limit=10", "--tenant", tenantA.Slug, "--json"}, &cliOutput, &cliDiagnostics); err != nil {
		t.Fatalf("CLI query save failed: %v: %s", err, cliDiagnostics.String())
	}
	parquetFile := filepath.Join(tempDir, "customers.parquet")
	duck, err := stdsql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := duck.ExecContext(ctx, "COPY (SELECT 1 AS id, 'Parquet' AS name) TO '"+
		strings.ReplaceAll(parquetFile, "'", "''")+"' (FORMAT parquet)"); err != nil {
		duck.Close()
		t.Fatal(err)
	}
	duck.Close()
	parquetSlug := "parquet-" + suffix
	cliOutput.Reset()
	cliDiagnostics.Reset()
	if err := nebulouscli.Run(ctx, []string{"dataset", "upload", parquetFile, "--name", parquetSlug,
		"--tenant", tenantA.Slug, "--json"}, &cliOutput, &cliDiagnostics); err != nil {
		t.Fatalf("CLI Parquet upload failed: %v: %s", err, cliDiagnostics.String())
	}
	parquetResult := executeQuery(t, httpServer.URL, tokenA, tenantA.Slug, "parquet-query-"+suffix,
		fmt.Sprintf(`SELECT name FROM %q LIMIT $limit`, parquetSlug), 10)
	if len(parquetResult.Result.Rows) != 1 || parquetResult.Result.Rows[0][0] != "Parquet" {
		t.Fatalf("unexpected Parquet result: %+v", parquetResult.Result)
	}
	// DuckLake 1.5 inlines up to 10 rows; 12 forces this query to read S3-backed Parquet.
	parquetBackedSlug := "parquet-backed-" + suffix
	parquetBackedRun := uploadDataset(t, httpServer.URL, tokenA, tenantA.Slug, parquetBackedSlug, parquetBackedSlug,
		[]byte("id,name\n1,a\n2,b\n3,c\n4,d\n5,e\n6,f\n7,g\n8,h\n9,i\n10,j\n11,k\n12,l\n"))
	if parquetBackedRun.Status != "succeeded" || parquetBackedRun.RowCount == nil || *parquetBackedRun.RowCount != 12 {
		t.Fatalf("Parquet-backed import failed: %+v", parquetBackedRun)
	}
	parquetBackedResult := executeQuery(t, httpServer.URL, tokenA, tenantA.Slug, "parquet-backed-query-"+suffix,
		fmt.Sprintf(`SELECT name FROM %q ORDER BY id LIMIT $limit`, parquetBackedSlug), 10)
	if len(parquetBackedResult.Result.Rows) != 10 || parquetBackedResult.Result.Rows[0][0] != "a" {
		t.Fatalf("unexpected Parquet-backed result: %+v", parquetBackedResult.Result)
	}
	privateJob := uploadDataset(t, httpServer.URL, tokenB, tenantB.Slug, "private-"+suffix, "private-"+suffix,
		[]byte("id,value\n1,tenant-b\n"))
	if privateJob.SyncID == nil {
		t.Fatal("dataset job omitted its sync ID")
	}
	var claimNanoseconds int64
	if err := database.QueryRowContext(ctx, `SELECT (extract(epoch FROM (r.started_at - j.available_at)) * 1000000000)::bigint
		FROM sync_runs r JOIN jobs j ON (j.tenant_id, j.sync_run_id) = (r.tenant_id, r.id)
		WHERE r.tenant_id = $1 AND r.id = $2`, tenantB.ID, *privateJob.SyncID).Scan(&claimNanoseconds); err != nil {
		t.Fatal(err)
	}
	claimLatency := time.Duration(claimNanoseconds)
	t.Logf("queued job claim latency=%s", claimLatency)
	if claimLatency > 500*time.Millisecond {
		t.Fatalf("queued job claim latency %s exceeds 500ms budget", claimLatency)
	}
	expectQueryFailure(t, httpServer.URL, tokenB, tenantB.Slug, queryName, querySQL, 10)
	expectQueryFailure(t, httpServer.URL, tokenA, tenantA.Slug, "host-file-"+suffix,
		`SELECT * FROM read_csv('/etc/passwd') LIMIT $limit`, 10)
	var externalRequests atomic.Int32
	externalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		externalRequests.Add(1)
		_, _ = io.WriteString(w, "id,name\n1,escaped\n")
	}))
	defer externalServer.Close()
	expectQueryFailure(t, httpServer.URL, tokenA, tenantA.Slug, "network-"+suffix,
		"SELECT * FROM read_csv('"+externalServer.URL+"/data.csv') LIMIT $limit", 10)
	if count := externalRequests.Load(); count != 0 {
		t.Fatalf("sandboxed query made %d arbitrary HTTP requests", count)
	}
	expectQueryFailure(t, httpServer.URL, tokenB, tenantB.Slug, "cross-tenant-file-"+suffix,
		"SELECT * FROM read_parquet('s3://"+storageConfig.Bucket+"/tenants/"+tenantA.ID+"/lake/**/*.parquet') LIMIT $limit", 10)

	durations := make([]time.Duration, 30)
	for index := range durations {
		started := time.Now()
		executeQuery(t, httpServer.URL, tokenA, tenantA.Slug, queryName, querySQL, 10)
		durations[index] = time.Since(started)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	median, p95 := (durations[14]+durations[15])/2, durations[28]
	t.Logf("saved query API: median=%s p95=%s", median, p95)
	if p95 > 500*time.Millisecond {
		t.Fatalf("saved query p95 %s exceeds 500ms budget", p95)
	}
	var revisionCount int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM query_revisions r
		JOIN queries q ON (q.tenant_id, q.id) = (r.tenant_id, r.query_id)
		WHERE q.tenant_id = $1 AND q.slug = $2`, tenantA.ID, queryName).Scan(&revisionCount); err != nil {
		t.Fatal(err)
	}
	if revisionCount != 1 {
		t.Fatalf("identical runs created %d query revisions", revisionCount)
	}
	lastGood := executeQuery(t, httpServer.URL, tokenA, tenantA.Slug, queryName, querySQL, 10)

	catalogURL, _, _, err := db.TenantCatalog(databaseURL, catalogSecret, tenantA.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancelContext, cancelQuery := context.WithTimeout(context.Background(), 200*time.Millisecond)
	started := time.Now()
	err = queryexec.Run(cancelContext, platform, queryexec.Request{
		CatalogURL: catalogURL, DataPath: objects.DataPath(tenantA.ID), ExtensionDir: extensionDir,
		Storage: storageConfig, SQL: `SELECT sum(sin(i)) FROM range(1000000000000) t(i)`,
		Parameters: map[string]any{}, ParameterTypes: map[string]string{}, MaxRows: 10, MaxBytes: 1 << 20,
	}, io.Discard)
	cancelQuery()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pathological query was not cancelled: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("query process cancellation took %s", elapsed)
	}

	bad := []byte("id,full_name\n3,Hopper\n")
	failed := uploadDataset(t, httpServer.URL, tokenA, tenantA.Slug, datasetSlug, "bad-"+suffix, bad)
	if failed.Status != "failed" {
		t.Fatalf("schema-changing import did not fail: %+v", failed)
	}
	second := executeQuery(t, httpServer.URL, tokenA, tenantA.Slug, queryName, querySQL, 10)
	if second.Result.SnapshotID != lastGood.Result.SnapshotID || len(second.Result.Rows) != 2 {
		t.Fatalf("failed import replaced the good snapshot: before=%+v after=%+v", lastGood.Result, second.Result)
	}
	if os.Getenv("NEB_APP_INTEGRATION") == "1" {
		testAppDeployment(t, ctx, tempDir, httpServer.URL, tenantA, queryName, querySQL)
	}
	stopWorker()
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop")
	}
}

func testAppDeployment(t *testing.T, ctx context.Context, tempDir, serverURL string, tenant db.Tenant, queryName, querySQL string) {
	project := filepath.Join(tempDir, "app")
	if err := os.MkdirAll(filepath.Join(project, "dist"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "queries"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "dist", "index.html"), []byte("<h1>release one</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "queries", "customers.sql"), []byte(querySQL), 0o600); err != nil {
		t.Fatal(err)
	}
	appSlug := "sales-" + fmt.Sprintf("%x", time.Now().UnixNano())
	manifestJSON, _ := json.Marshal(map[string]any{
		"app": map[string]string{"slug": appSlug, "dir": "dist"},
		"queries": []any{map[string]any{"name": queryName, "file": "queries/customers.sql",
			"parameters": map[string]string{"limit": "integer"}}},
	})
	if err := os.WriteFile(filepath.Join(project, "nebulous.json"), manifestJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)

	var output, diagnostics bytes.Buffer
	if err := nebulouscli.Run(ctx, []string{"app", "deploy", "--tenant", tenant.Slug, "--json"}, &output, &diagnostics); err != nil {
		t.Fatalf("first app deploy failed: %v: %s", err, diagnostics.String())
	}
	var first struct {
		Deployment struct {
			ID string `json:"id"`
		} `json:"deployment"`
		AppURL string `json:"app_url"`
	}
	decodeCLIData(t, output.Bytes(), &first)
	output.Reset()
	if err := nebulouscli.Run(ctx, []string{"app", "open", "--tenant", tenant.Slug, "--json"}, &output, &diagnostics); err != nil {
		t.Fatalf("neb app open failed: %v", err)
	}
	var opened struct {
		URL string `json:"url"`
	}
	decodeCLIData(t, output.Bytes(), &opened)
	appURL, err := url.Parse(opened.URL)
	if err != nil || appURL.Query().Get("code") == "" {
		t.Fatalf("neb app open did not return a private app login URL: %s", opened.URL)
	}

	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	loginRequest, _ := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/?"+appURL.RawQuery, nil)
	loginRequest.Host = appURL.Host
	loginResponse, err := client.Do(loginRequest)
	if err != nil {
		t.Fatal(err)
	}
	cookies := loginResponse.Cookies()
	loginResponse.Body.Close()
	if loginResponse.StatusCode != http.StatusSeeOther || loginResponse.Header.Get("Location") != "/" || len(cookies) != 1 {
		t.Fatalf("unexpected login exchange: status=%d location=%q cookies=%v", loginResponse.StatusCode, loginResponse.Header.Get("Location"), cookies)
	}
	cookie := cookies[0]
	if cookie.Domain != "" || !cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("runtime cookie is not host-only and hardened: %+v", cookie)
	}
	replay, _ := http.NewRequestWithContext(ctx, http.MethodGet, serverURL+"/?"+appURL.RawQuery, nil)
	replay.Host = appURL.Host
	replayResponse, err := client.Do(replay)
	if err != nil {
		t.Fatal(err)
	}
	replayResponse.Body.Close()
	if replayResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("single-use login code replay returned %d", replayResponse.StatusCode)
	}

	asset := runtimeRequest(t, ctx, client, serverURL, appURL.Host, http.MethodGet, "/", cookie, "")
	assetBody, _ := io.ReadAll(asset.Body)
	asset.Body.Close()
	if asset.StatusCode != http.StatusOK || !bytes.Contains(assetBody, []byte("release one")) || asset.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("private app asset failed: status=%d body=%s", asset.StatusCode, assetBody)
	}
	declared := runtimeRequest(t, ctx, client, serverURL, appURL.Host, http.MethodPost,
		"/runtime/v1/queries/"+queryName, cookie, `{"parameters":{"limit":10}}`)
	var result integrationQuery
	if err := json.NewDecoder(declared.Body).Decode(&result.Result); err != nil {
		declared.Body.Close()
		t.Fatal(err)
	}
	declared.Body.Close()
	if declared.StatusCode != http.StatusOK || len(result.Result.Rows) != 2 {
		t.Fatalf("declared app query failed: status=%d result=%+v", declared.StatusCode, result)
	}
	undeclared := runtimeRequest(t, ctx, client, serverURL, appURL.Host, http.MethodPost,
		"/runtime/v1/queries/not-granted", cookie, `{"parameters":{}}`)
	undeclared.Body.Close()
	if undeclared.StatusCode != http.StatusForbidden {
		t.Fatalf("undeclared app query returned %d", undeclared.StatusCode)
	}
	controlOnAppHost := runtimeRequest(t, ctx, client, serverURL, appURL.Host, http.MethodGet, "/healthz", cookie, "")
	controlOnAppHost.Body.Close()
	if controlOnAppHost.StatusCode == http.StatusOK {
		t.Fatal("app host served the control-plane health endpoint")
	}

	if err := os.WriteFile(filepath.Join(project, "dist", "index.html"), []byte("<h1>release two</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	diagnostics.Reset()
	if err := nebulouscli.Run(ctx, []string{"app", "deploy", "--tenant", tenant.Slug, "--json"}, &output, &diagnostics); err != nil {
		t.Fatalf("second app deploy failed: %v: %s", err, diagnostics.String())
	}
	var secondDeploy struct {
		Rollback string `json:"rollback_command"`
	}
	decodeCLIData(t, output.Bytes(), &secondDeploy)
	wantRollback := "neb app deployment rollback " + first.Deployment.ID + " --tenant " + tenant.Slug
	if secondDeploy.Rollback != wantRollback {
		t.Fatalf("rollback command=%q want %q", secondDeploy.Rollback, wantRollback)
	}
	output.Reset()
	if err := nebulouscli.Run(ctx, []string{"app", "deployment", "rollback", first.Deployment.ID,
		"--tenant", tenant.Slug, "--json"}, &output, &diagnostics); err != nil {
		t.Fatalf("rollback failed: %v", err)
	}
	rolledBack := runtimeRequest(t, ctx, client, serverURL, appURL.Host, http.MethodGet, "/", cookie, "")
	rolledBackBody, _ := io.ReadAll(rolledBack.Body)
	rolledBack.Body.Close()
	if rolledBack.StatusCode != http.StatusOK || !bytes.Contains(rolledBackBody, []byte("release one")) {
		t.Fatalf("rollback did not restore release one: status=%d body=%s", rolledBack.StatusCode, rolledBackBody)
	}
}

func runtimeRequest(t *testing.T, ctx context.Context, client *http.Client, serverURL, host, method, path string, cookie *http.Cookie, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, method, serverURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = host
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func expectQueryFailure(t *testing.T, baseURL, token, tenantSlug, _ string, sqlText string, limit int) {
	t.Helper()
	body := fmt.Sprintf(`{"sql":%q,"parameters":{"limit":%d}}`, sqlText, limit)
	response := request(t, http.MethodPost, baseURL+"/v1/tenants/"+tenantSlug+"/queries/execute", token, body)
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unsafe or cross-tenant query returned %d", response.StatusCode)
	}
	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
		t.Fatal(err)
	}
	if failure.Error.Code != "query_failed" || strings.Contains(strings.ToLower(failure.Error.Message), "password") {
		t.Fatalf("unsafe query returned unsafe diagnostics: %+v", failure)
	}
}

type integrationJob struct {
	ID       string  `json:"id"`
	SyncID   *string `json:"sync_id"`
	Status   string  `json:"status"`
	RowCount *int64  `json:"row_count"`
	Error    *string `json:"error"`
}

func uploadDataset(t *testing.T, baseURL, token, tenantSlug, datasetSlug, idempotencyKey string, contents []byte) integrationJob {
	t.Helper()
	body := fmt.Sprintf(`{"slug":%q,"format":"csv","byte_count":%d,"idempotency_key":%q}`,
		datasetSlug, len(contents), idempotencyKey)
	response := request(t, http.MethodPost, baseURL+"/v1/tenants/"+tenantSlug+"/dataset-uploads", token, body)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create dataset upload returned %d", response.StatusCode)
	}
	var created struct {
		Upload  integrationJob `json:"upload"`
		Job     integrationJob `json:"job"`
		Content struct {
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		} `json:"content"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	put, err := http.NewRequest(http.MethodPut, created.Content.URL, bytes.NewReader(contents))
	if err != nil {
		t.Fatal(err)
	}
	put.ContentLength, put.Header = int64(len(contents)), created.Content.Headers
	putResponse, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	putResponse.Body.Close()
	if putResponse.StatusCode < 200 || putResponse.StatusCode >= 300 {
		t.Fatalf("signed upload returned %d", putResponse.StatusCode)
	}
	response = request(t, http.MethodPost, baseURL+"/v1/tenants/"+tenantSlug+"/dataset-uploads/"+created.Upload.ID+"/complete", token, `{}`)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("complete upload returned %d", response.StatusCode)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		response = request(t, http.MethodGet, baseURL+"/v1/tenants/"+tenantSlug+"/jobs/"+created.Job.ID, token, "")
		var current struct {
			Job integrationJob `json:"job"`
		}
		if err := json.NewDecoder(response.Body).Decode(&current); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		response.Body.Close()
		if current.Job.Status == "succeeded" || current.Job.Status == "failed" {
			return current.Job
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("dataset import timed out")
	return integrationJob{}
}

type integrationQuery struct {
	Result struct {
		Rows       [][]any `json:"rows"`
		SnapshotID int64   `json:"snapshot_id"`
	} `json:"result"`
}

func executeQuery(t *testing.T, baseURL, token, tenantSlug, _ string, sqlText string, limit int) integrationQuery {
	t.Helper()
	body := fmt.Sprintf(`{"sql":%q,"parameters":{"limit":%d}}`, sqlText, limit)
	response := request(t, http.MethodPost, baseURL+"/v1/tenants/"+tenantSlug+"/queries/execute", token, body)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var failure any
		_ = json.NewDecoder(response.Body).Decode(&failure)
		t.Fatalf("run query returned %d: %#v", response.StatusCode, failure)
	}
	var result integrationQuery
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func decodeCLIData(t *testing.T, output []byte, destination any) {
	t.Helper()
	var envelope struct {
		SchemaVersion int             `json:"schema_version"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 {
		t.Fatalf("CLI schema version=%d want 1", envelope.SchemaVersion)
	}
	if err := json.Unmarshal(envelope.Data, destination); err != nil {
		t.Fatal(err)
	}
}

func createTenant(t *testing.T, baseURL, token, slug string) db.Tenant {
	t.Helper()
	response := request(t, http.MethodPost, baseURL+"/v1/tenants", token, fmt.Sprintf(`{"slug":%q}`, slug))
	defer response.Body.Close()
	if response.Header.Get("X-Request-ID") == "" {
		t.Fatal("create tenant response omitted request ID")
	}
	if response.StatusCode != http.StatusCreated {
		contents, _ := io.ReadAll(response.Body)
		t.Fatalf("create tenant %s returned %d: %s", slug, response.StatusCode, contents)
	}
	var body struct {
		Tenant db.Tenant `json:"tenant"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Tenant
}

func listTenants(t *testing.T, baseURL, token string) []db.Tenant {
	t.Helper()
	response := request(t, http.MethodGet, baseURL+"/v1/tenants", token, "")
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("list tenants returned %d", response.StatusCode)
	}
	var body struct {
		Tenants []db.Tenant `json:"tenants"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Tenants
}

func request(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func envTest(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
