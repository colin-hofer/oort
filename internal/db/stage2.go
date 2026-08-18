package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"time"
)

var ErrLeaseLost = errors.New("job lease lost")

type Dataset struct {
	ID                string          `json:"id"`
	TenantID          string          `json:"tenant_id"`
	Slug              string          `json:"slug"`
	CurrentSnapshotID *int64          `json:"current_snapshot_id,omitempty"`
	Schema            json.RawMessage `json:"schema,omitempty"`
	RowCount          *int64          `json:"row_count,omitempty"`
	ByteCount         *int64          `json:"byte_count,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}

type SyncRun struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenant_id"`
	DatasetID  string          `json:"dataset_id"`
	Status     string          `json:"status"`
	Format     string          `json:"format"`
	ObjectKey  string          `json:"-"`
	SnapshotID *int64          `json:"snapshot_id,omitempty"`
	RowCount   *int64          `json:"row_count,omitempty"`
	ByteCount  int64           `json:"byte_count"`
	Schema     json.RawMessage `json:"schema,omitempty"`
	Error      *string         `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

type DatasetUpload struct {
	Dataset Dataset `json:"dataset"`
	Run     SyncRun `json:"run"`
}

type ImportJob struct {
	ID        string
	TenantID  string
	SyncRunID string
	WorkerID  string
}

type ImportDetails struct {
	ImportJob
	DatasetID   string
	DatasetSlug string
	Format      string
	ObjectKey   string
	ByteCount   int64
}

type ImportResult struct {
	SnapshotID int64
	RowCount   int64
	Schema     json.RawMessage
}

type QueryRevision struct {
	ID             string            `json:"id"`
	TenantID       string            `json:"tenant_id"`
	QueryID        string            `json:"query_id"`
	Slug           string            `json:"slug"`
	Version        int               `json:"version"`
	SQL            string            `json:"sql"`
	ParameterTypes map[string]string `json:"parameter_types"`
	CreatedAt      time.Time         `json:"created_at"`
}

func ResolveTenant(ctx context.Context, database *sql.DB, actor User, slug string) (Tenant, error) {
	var tenant Tenant
	err := database.QueryRowContext(ctx, `SELECT t.id, t.slug, m.role, t.created_at
		FROM memberships m JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1 AND t.slug = $2`, actor.ID, slug).
		Scan(&tenant.ID, &tenant.Slug, &tenant.Role, &tenant.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Tenant{}, sql.ErrNoRows
		}
		return Tenant{}, fmt.Errorf("resolve tenant: %w", err)
	}
	tenant.CreatedAt = tenant.CreatedAt.UTC()
	return tenant, nil
}

func CreateDatasetUpload(ctx context.Context, database *sql.DB, tenant Tenant, actor User, slug, format string, byteCount int64, idempotencyKey, requestID string) (DatasetUpload, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return DatasetUpload{}, fmt.Errorf("start upload transaction: %w", err)
	}
	defer tx.Rollback()

	var existing DatasetUpload
	err = tx.QueryRowContext(ctx, `SELECT d.id, d.tenant_id, d.slug, d.current_snapshot_id,
		COALESCE(d.schema_json, 'null'::jsonb), d.row_count, d.byte_count, d.created_at,
		r.id, r.tenant_id, r.dataset_id, r.status, r.format, r.object_key,
		r.snapshot_id, r.row_count, r.byte_count, COALESCE(r.schema_json, 'null'::jsonb),
		r.error, r.created_at, r.started_at, r.finished_at
		FROM jobs j
		JOIN sync_runs r ON (r.tenant_id, r.id) = (j.tenant_id, j.sync_run_id)
		JOIN datasets d ON (d.tenant_id, d.id) = (r.tenant_id, r.dataset_id)
		WHERE j.tenant_id = $1 AND j.kind = 'dataset_import' AND j.idempotency_key = $2`,
		tenant.ID, idempotencyKey).Scan(datasetUploadScan(&existing)...)
	if err == nil {
		if existing.Dataset.Slug != slug || existing.Run.Format != format || existing.Run.ByteCount != byteCount {
			return DatasetUpload{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DatasetUpload{}, fmt.Errorf("find idempotent upload: %w", err)
	}

	dataset := Dataset{ID: newID(), TenantID: tenant.ID, Slug: slug}
	err = tx.QueryRowContext(ctx, `INSERT INTO datasets (id, tenant_id, slug)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, slug) DO UPDATE SET slug = EXCLUDED.slug
		RETURNING id, created_at`, dataset.ID, tenant.ID, slug).
		Scan(&dataset.ID, &dataset.CreatedAt)
	if err != nil {
		return DatasetUpload{}, fmt.Errorf("create dataset: %w", err)
	}
	run := SyncRun{
		ID:        newID(),
		TenantID:  tenant.ID,
		DatasetID: dataset.ID,
		Status:    "awaiting_upload",
		Format:    format,
		ByteCount: byteCount,
		CreatedAt: time.Now().UTC(),
	}
	run.ObjectKey = fmt.Sprintf("tenants/%s/uploads/%s/source.%s", tenant.ID, run.ID, format)
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_runs
		(id, tenant_id, dataset_id, actor_user_id, status, format, object_key, byte_count, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, run.ID, tenant.ID, dataset.ID,
		actor.ID, run.Status, format, run.ObjectKey, byteCount, run.CreatedAt); err != nil {
		return DatasetUpload{}, fmt.Errorf("create sync run: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs
		(id, tenant_id, kind, idempotency_key, sync_run_id, status)
		VALUES ($1, $2, 'dataset_import', $3, $4, 'awaiting_upload')`,
		newID(), tenant.ID, idempotencyKey, run.ID); err != nil {
		if sqlState(err) == "23505" {
			return DatasetUpload{}, ErrConflict
		}
		return DatasetUpload{}, fmt.Errorf("create import job: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events
		(id, tenant_id, actor_user_id, action, resource_type, resource_id, request_id)
		VALUES ($1, $2, $3, 'dataset.upload_created', 'sync_run', $4, $5)`,
		newID(), tenant.ID, actor.ID, run.ID, requestID); err != nil {
		return DatasetUpload{}, fmt.Errorf("audit upload creation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DatasetUpload{}, fmt.Errorf("commit upload: %w", err)
	}
	return DatasetUpload{Dataset: dataset, Run: run}, nil
}

func CompleteDatasetUpload(ctx context.Context, database *sql.DB, tenantID, runID string) (SyncRun, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return SyncRun{}, fmt.Errorf("start upload completion: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE sync_runs SET status = 'queued'
		WHERE tenant_id = $1 AND id = $2 AND status = 'awaiting_upload'`, tenantID, runID)
	if err != nil {
		return SyncRun{}, fmt.Errorf("queue sync run: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'queued', available_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND sync_run_id = $2 AND status = 'awaiting_upload'`, tenantID, runID); err != nil {
			return SyncRun{}, fmt.Errorf("queue import job: %w", err)
		}
	}
	run, err := getSyncRun(ctx, tx, tenantID, runID)
	if err != nil {
		return SyncRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return SyncRun{}, fmt.Errorf("commit upload completion: %w", err)
	}
	return run, nil
}

func GetSyncRun(ctx context.Context, database *sql.DB, tenantID, runID string) (SyncRun, error) {
	return getSyncRun(ctx, database, tenantID, runID)
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getSyncRun(ctx context.Context, query rowQuerier, tenantID, runID string) (SyncRun, error) {
	var run SyncRun
	err := query.QueryRowContext(ctx, `SELECT id, tenant_id, dataset_id, status, format, object_key,
		snapshot_id, row_count, byte_count, COALESCE(schema_json, 'null'::jsonb), error,
		created_at, started_at, finished_at
		FROM sync_runs WHERE tenant_id = $1 AND id = $2`, tenantID, runID).
		Scan(&run.ID, &run.TenantID, &run.DatasetID, &run.Status, &run.Format, &run.ObjectKey,
			&run.SnapshotID, &run.RowCount, &run.ByteCount, &run.Schema, &run.Error,
			&run.CreatedAt, &run.StartedAt, &run.FinishedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SyncRun{}, sql.ErrNoRows
		}
		return SyncRun{}, fmt.Errorf("get sync run: %w", err)
	}
	return run, nil
}

func ClaimImportJob(ctx context.Context, database *sql.DB, workerID string, lease time.Duration) (ImportJob, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return ImportJob{}, fmt.Errorf("start job claim: %w", err)
	}
	defer tx.Rollback()
	var job ImportJob
	err = tx.QueryRowContext(ctx, `WITH candidate AS (
		SELECT j.id FROM jobs j
		WHERE j.kind = 'dataset_import'
		  AND j.available_at <= now()
		  AND (j.status = 'queued' OR (j.status = 'running' AND j.lease_expires_at < now()))
		  AND NOT EXISTS (
			SELECT 1 FROM jobs active
			WHERE active.tenant_id = j.tenant_id AND active.id <> j.id
			  AND active.kind = 'dataset_import' AND active.status = 'running'
			  AND active.lease_expires_at >= now()
		  )
		ORDER BY j.available_at, j.created_at
		FOR UPDATE SKIP LOCKED LIMIT 1
	)
	UPDATE jobs j SET status = 'running', leased_by = $1,
		lease_expires_at = now() + $2::interval, attempts = attempts + 1, updated_at = now()
	FROM candidate WHERE j.id = candidate.id
	RETURNING j.id, j.tenant_id, j.sync_run_id`, workerID, lease.String()).
		Scan(&job.ID, &job.TenantID, &job.SyncRunID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ImportJob{}, sql.ErrNoRows
		}
		return ImportJob{}, fmt.Errorf("claim import job: %w", err)
	}
	job.WorkerID = workerID
	if _, err := tx.ExecContext(ctx, `UPDATE sync_runs SET status = 'running', started_at = COALESCE(started_at, now())
		WHERE tenant_id = $1 AND id = $2`, job.TenantID, job.SyncRunID); err != nil {
		return ImportJob{}, fmt.Errorf("start sync run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ImportJob{}, fmt.Errorf("commit job claim: %w", err)
	}
	return job, nil
}

func GetImportDetails(ctx context.Context, database *sql.DB, job ImportJob) (ImportDetails, error) {
	var details ImportDetails
	details.ImportJob = job
	err := database.QueryRowContext(ctx, `SELECT r.dataset_id, d.slug, r.format, r.object_key, r.byte_count
		FROM jobs j
		JOIN sync_runs r ON (r.tenant_id, r.id) = (j.tenant_id, j.sync_run_id)
		JOIN datasets d ON (d.tenant_id, d.id) = (r.tenant_id, r.dataset_id)
		WHERE j.id = $1 AND j.tenant_id = $2 AND j.status = 'running' AND j.leased_by = $3`,
		job.ID, job.TenantID, job.WorkerID).
		Scan(&details.DatasetID, &details.DatasetSlug, &details.Format, &details.ObjectKey, &details.ByteCount)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ImportDetails{}, ErrLeaseLost
		}
		return ImportDetails{}, fmt.Errorf("load import job: %w", err)
	}
	return details, nil
}

func HeartbeatImportJob(ctx context.Context, database *sql.DB, job ImportJob, lease time.Duration) error {
	result, err := database.ExecContext(ctx, `UPDATE jobs SET lease_expires_at = now() + $1::interval, updated_at = now()
		WHERE id = $2 AND tenant_id = $3 AND status = 'running' AND leased_by = $4`,
		lease.String(), job.ID, job.TenantID, job.WorkerID)
	if err != nil {
		return fmt.Errorf("heartbeat import job: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrLeaseLost
	}
	return nil
}

func CompleteImportJob(ctx context.Context, database *sql.DB, job ImportJob, result ImportResult) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start import completion: %w", err)
	}
	defer tx.Rollback()
	var datasetID, runID string
	err = tx.QueryRowContext(ctx, `SELECT r.dataset_id, r.id FROM jobs j
		JOIN sync_runs r ON (r.tenant_id, r.id) = (j.tenant_id, j.sync_run_id)
		WHERE j.id = $1 AND j.tenant_id = $2 AND j.status = 'running' AND j.leased_by = $3
		FOR UPDATE`, job.ID, job.TenantID, job.WorkerID).Scan(&datasetID, &runID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lock import job: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE datasets SET current_snapshot_id = $1, schema_json = $2,
		row_count = $3, byte_count = (SELECT byte_count FROM sync_runs WHERE id = $4), updated_at = now()
		WHERE tenant_id = $5 AND id = $6`, result.SnapshotID, result.Schema, result.RowCount,
		runID, job.TenantID, datasetID); err != nil {
		return fmt.Errorf("publish dataset snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sync_runs SET status = 'succeeded', snapshot_id = $1,
		row_count = $2, schema_json = $3, finished_at = now(), error = NULL
		WHERE tenant_id = $4 AND id = $5`, result.SnapshotID, result.RowCount, result.Schema,
		job.TenantID, runID); err != nil {
		return fmt.Errorf("complete sync run: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'succeeded', leased_by = NULL,
		lease_expires_at = NULL, updated_at = now(), last_error = NULL WHERE id = $1`, job.ID); err != nil {
		return fmt.Errorf("complete import job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit import completion: %w", err)
	}
	return nil
}

func FailImportJob(ctx context.Context, database *sql.DB, job ImportJob, failure error) error {
	message := failure.Error()
	if len(message) > 2000 {
		message = message[:2000]
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start import failure: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'failed', leased_by = NULL,
		lease_expires_at = NULL, last_error = $1, updated_at = now()
		WHERE id = $2 AND tenant_id = $3 AND status = 'running' AND leased_by = $4`,
		message, job.ID, job.TenantID, job.WorkerID)
	if err != nil {
		return fmt.Errorf("fail import job: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrLeaseLost
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sync_runs SET status = 'failed', error = $1, finished_at = now()
		WHERE tenant_id = $2 AND id = $3`, message, job.TenantID, job.SyncRunID); err != nil {
		return fmt.Errorf("fail sync run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit import failure: %w", err)
	}
	return nil
}

func SaveQueryRevision(ctx context.Context, database *sql.DB, tenant Tenant, actor User, slug, sqlText string, parameterTypes map[string]string, requestID string) (QueryRevision, error) {
	typesJSON, err := json.Marshal(parameterTypes)
	if err != nil {
		return QueryRevision{}, fmt.Errorf("encode parameter types: %w", err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return QueryRevision{}, fmt.Errorf("start query revision: %w", err)
	}
	defer tx.Rollback()
	queryID := newID()
	if err := tx.QueryRowContext(ctx, `INSERT INTO queries (id, tenant_id, slug) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, slug) DO UPDATE SET slug = EXCLUDED.slug
		RETURNING id`, queryID, tenant.ID, slug).Scan(&queryID); err != nil {
		return QueryRevision{}, fmt.Errorf("save query: %w", err)
	}
	var current QueryRevision
	var currentTypes []byte
	err = tx.QueryRowContext(ctx, `SELECT r.id, r.version, r.sql_text, r.parameter_types, r.created_at
		FROM queries q JOIN query_revisions r ON (r.tenant_id, r.id) = (q.tenant_id, q.current_revision_id)
		WHERE q.tenant_id = $1 AND q.id = $2`, tenant.ID, queryID).
		Scan(&current.ID, &current.Version, &current.SQL, &currentTypes, &current.CreatedAt)
	if err == nil && current.SQL == sqlText && equalJSON(currentTypes, typesJSON) {
		current.TenantID, current.QueryID, current.Slug, current.ParameterTypes = tenant.ID, queryID, slug, parameterTypes
		return current, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return QueryRevision{}, fmt.Errorf("read current query revision: %w", err)
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM query_revisions
		WHERE tenant_id = $1 AND query_id = $2`, tenant.ID, queryID).Scan(&version); err != nil {
		return QueryRevision{}, fmt.Errorf("allocate query revision: %w", err)
	}
	revision := QueryRevision{ID: newID(), TenantID: tenant.ID, QueryID: queryID, Slug: slug,
		Version: version, SQL: sqlText, ParameterTypes: parameterTypes, CreatedAt: time.Now().UTC()}
	if _, err := tx.ExecContext(ctx, `INSERT INTO query_revisions
		(id, tenant_id, query_id, version, sql_text, parameter_types, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, revision.ID, tenant.ID, queryID, version,
		sqlText, typesJSON, revision.CreatedAt); err != nil {
		return QueryRevision{}, fmt.Errorf("create query revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE queries SET current_revision_id = $1, updated_at = now()
		WHERE tenant_id = $2 AND id = $3`, revision.ID, tenant.ID, queryID); err != nil {
		return QueryRevision{}, fmt.Errorf("publish query revision: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events
		(id, tenant_id, actor_user_id, action, resource_type, resource_id, request_id)
		VALUES ($1, $2, $3, 'query.revision_created', 'query_revision', $4, $5)`,
		newID(), tenant.ID, actor.ID, revision.ID, requestID); err != nil {
		return QueryRevision{}, fmt.Errorf("audit query revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return QueryRevision{}, fmt.Errorf("commit query revision: %w", err)
	}
	return revision, nil
}

func TenantCatalog(databaseURL, secret, tenantID string) (catalogURL, role, name string, err error) {
	if secret == "" {
		return "", "", "", fmt.Errorf("catalog secret is required")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", "", "", fmt.Errorf("invalid PostgreSQL URL")
	}
	compactID := strings.ReplaceAll(tenantID, "-", "")
	if len(compactID) != 32 {
		return "", "", "", fmt.Errorf("invalid tenant ID")
	}
	name = "nebcat_" + compactID
	role = "nebtenant_" + compactID
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(tenantID))
	password := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	parsed.User = url.UserPassword(role, password)
	parsed.Path = "/" + name
	return parsed.String(), role, name, nil
}

func EnsureTenantCatalog(ctx context.Context, database *sql.DB, databaseURL, secret, tenantID string) (string, error) {
	catalogURL, role, name, err := TenantCatalog(databaseURL, secret, tenantID)
	if err != nil {
		return "", err
	}
	var roleExists bool
	if err := database.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&roleExists); err != nil {
		return "", fmt.Errorf("check catalog role: %w", err)
	}
	roleSQL := quoteIdentifier(role)
	password := quoteLiteral(catalogPassword(secret, tenantID))
	if !roleExists {
		if _, err := database.ExecContext(ctx, "CREATE ROLE "+roleSQL+" LOGIN PASSWORD "+password); err != nil && sqlState(err) != "42710" {
			return "", fmt.Errorf("create catalog role: %w", err)
		}
	} else if _, err := database.ExecContext(ctx, "ALTER ROLE "+roleSQL+" PASSWORD "+password); err != nil {
		return "", fmt.Errorf("refresh catalog role: %w", err)
	}
	var databaseExists bool
	if err := database.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&databaseExists); err != nil {
		return "", fmt.Errorf("check catalog database: %w", err)
	}
	if !databaseExists {
		if _, err := database.ExecContext(ctx, "CREATE DATABASE "+quoteIdentifier(name)+" OWNER "+roleSQL); err != nil && sqlState(err) != "42P04" {
			return "", fmt.Errorf("create catalog database: %w", err)
		}
	}
	return catalogURL, nil
}

func catalogPassword(secret, tenantID string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(tenantID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
func quoteLiteral(value string) string    { return `'` + strings.ReplaceAll(value, `'`, `''`) + `'` }

func datasetUploadScan(upload *DatasetUpload) []any {
	return []any{
		&upload.Dataset.ID, &upload.Dataset.TenantID, &upload.Dataset.Slug,
		&upload.Dataset.CurrentSnapshotID, &upload.Dataset.Schema, &upload.Dataset.RowCount,
		&upload.Dataset.ByteCount, &upload.Dataset.CreatedAt,
		&upload.Run.ID, &upload.Run.TenantID, &upload.Run.DatasetID, &upload.Run.Status,
		&upload.Run.Format, &upload.Run.ObjectKey, &upload.Run.SnapshotID, &upload.Run.RowCount,
		&upload.Run.ByteCount, &upload.Run.Schema, &upload.Run.Error, &upload.Run.CreatedAt,
		&upload.Run.StartedAt, &upload.Run.FinishedAt,
	}
}

func equalJSON(left, right []byte) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && reflect.DeepEqual(a, b)
}
