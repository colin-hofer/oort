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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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
	compose := filepath.Join(root, "compose.yaml")
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
	httpServer := httptest.NewServer(New(database))
	defer httpServer.Close()

	tenantA := createTenant(t, httpServer.URL, tokenA, "a-"+suffix)
	tenantB := createTenant(t, httpServer.URL, tokenB, "b-"+suffix)
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
	if os.Getenv("NEB_STAGE2_INTEGRATION") != "1" {
		t.Skip("set NEB_STAGE2_INTEGRATION=1 to run the upload-to-query test")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	compose := filepath.Join(root, "compose.yaml")
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
	platform := filepath.Join(tempDir, "nebulous")
	command = exec.CommandContext(ctx, "go", "build", "-o", platform, "./cmd/nebulous")
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
	catalogSecret := "stage2-test-catalog-secret"
	extensionDir := filepath.Join(tempDir, "extensions")
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
			Storage: storageConfig, QueryTimeout: 20 * time.Second, Log: os.Stderr},
	}))
	defer httpServer.Close()

	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	_, tokenA, err := db.CreateLocalIdentity(ctx, database, "stage2-a-"+suffix+"@test.invalid", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, tokenB, err := db.CreateLocalIdentity(ctx, database, "stage2-b-"+suffix+"@test.invalid", time.Hour)
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
		Run integrationSync `json:"run"`
	}
	if err := json.Unmarshal(cliOutput.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	succeeded := uploaded.Run
	if succeeded.Status != "succeeded" || succeeded.RowCount == nil || *succeeded.RowCount != 2 {
		message := ""
		if succeeded.Error != nil {
			message = *succeeded.Error
		}
		t.Fatalf("unexpected successful sync: %+v: %s", succeeded, message)
	}
	response := request(t, http.MethodGet, httpServer.URL+"/v1/tenants/"+tenantB.Slug+"/sync-runs/"+succeeded.ID, tokenB, "")
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant sync-run lookup returned %d", response.StatusCode)
	}

	queryName := "customer-query-" + suffix
	querySQL := fmt.Sprintf(`SELECT name FROM %q ORDER BY id LIMIT $limit`, datasetSlug)
	queryFile := filepath.Join(tempDir, "customers.sql")
	if err := os.WriteFile(queryFile, []byte(querySQL), 0o600); err != nil {
		t.Fatal(err)
	}
	cliOutput.Reset()
	cliDiagnostics.Reset()
	if err := nebulouscli.Run(ctx, []string{"query", "run", queryFile, "--name", queryName,
		"--param", "limit=10", "--tenant", tenantA.Slug, "--json"}, &cliOutput, &cliDiagnostics); err != nil {
		t.Fatalf("CLI query failed: %v: %s", err, cliDiagnostics.String())
	}
	var first integrationQuery
	if err := json.Unmarshal(cliOutput.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Result.Rows) != 2 || first.Result.SnapshotID == 0 {
		t.Fatalf("unexpected first query result: %+v", first)
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
	privateRun := uploadDataset(t, httpServer.URL, tokenB, tenantB.Slug, "private-"+suffix, "private-"+suffix,
		[]byte("id,value\n1,tenant-b\n"))
	var claimNanoseconds int64
	if err := database.QueryRowContext(ctx, `SELECT (extract(epoch FROM (r.started_at - j.available_at)) * 1000000000)::bigint
		FROM sync_runs r JOIN jobs j ON (j.tenant_id, j.sync_run_id) = (r.tenant_id, r.id)
		WHERE r.tenant_id = $1 AND r.id = $2`, tenantB.ID, privateRun.ID).Scan(&claimNanoseconds); err != nil {
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
	expectQueryFailure(t, httpServer.URL, tokenA, tenantA.Slug, "network-"+suffix,
		`SELECT * FROM read_csv('http://127.0.0.1:9000/') LIMIT $limit`, 10)

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

func expectQueryFailure(t *testing.T, baseURL, token, tenantSlug, name, sqlText string, limit int) {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"sql":%q,"parameters":{"limit":%d}}`, name, sqlText, limit)
	response := request(t, http.MethodPost, baseURL+"/v1/tenants/"+tenantSlug+"/queries/run", token, body)
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

type integrationSync struct {
	ID       string  `json:"id"`
	Status   string  `json:"status"`
	RowCount *int64  `json:"row_count"`
	Error    *string `json:"error"`
}

func uploadDataset(t *testing.T, baseURL, token, tenantSlug, datasetSlug, idempotencyKey string, contents []byte) integrationSync {
	t.Helper()
	body := fmt.Sprintf(`{"slug":%q,"format":"csv","byte_count":%d,"idempotency_key":%q}`,
		datasetSlug, len(contents), idempotencyKey)
	response := request(t, http.MethodPost, baseURL+"/v1/tenants/"+tenantSlug+"/dataset-uploads", token, body)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create dataset upload returned %d", response.StatusCode)
	}
	var created struct {
		Run    integrationSync `json:"run"`
		Upload struct {
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		} `json:"upload"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	put, err := http.NewRequest(http.MethodPut, created.Upload.URL, bytes.NewReader(contents))
	if err != nil {
		t.Fatal(err)
	}
	put.ContentLength, put.Header = int64(len(contents)), created.Upload.Headers
	putResponse, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	putResponse.Body.Close()
	if putResponse.StatusCode < 200 || putResponse.StatusCode >= 300 {
		t.Fatalf("signed upload returned %d", putResponse.StatusCode)
	}
	response = request(t, http.MethodPost, baseURL+"/v1/tenants/"+tenantSlug+"/dataset-uploads/"+created.Run.ID+"/complete", token, `{}`)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("complete upload returned %d", response.StatusCode)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		response = request(t, http.MethodGet, baseURL+"/v1/tenants/"+tenantSlug+"/sync-runs/"+created.Run.ID, token, "")
		var current struct {
			Run integrationSync `json:"run"`
		}
		if err := json.NewDecoder(response.Body).Decode(&current); err != nil {
			response.Body.Close()
			t.Fatal(err)
		}
		response.Body.Close()
		if current.Run.Status == "succeeded" || current.Run.Status == "failed" {
			return current.Run
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("dataset import timed out")
	return integrationSync{}
}

type integrationQuery struct {
	Result struct {
		Rows       [][]any `json:"rows"`
		SnapshotID int64   `json:"snapshot_id"`
	} `json:"result"`
}

func executeQuery(t *testing.T, baseURL, token, tenantSlug, name, sqlText string, limit int) integrationQuery {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"sql":%q,"parameters":{"limit":%d}}`, name, sqlText, limit)
	response := request(t, http.MethodPost, baseURL+"/v1/tenants/"+tenantSlug+"/queries/run", token, body)
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

func createTenant(t *testing.T, baseURL, token, slug string) db.Tenant {
	t.Helper()
	response := request(t, http.MethodPost, baseURL+"/v1/tenants", token, fmt.Sprintf(`{"slug":%q}`, slug))
	defer response.Body.Close()
	if response.Header.Get("X-Request-ID") == "" {
		t.Fatal("create tenant response omitted request ID")
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create tenant %s returned %d", slug, response.StatusCode)
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
