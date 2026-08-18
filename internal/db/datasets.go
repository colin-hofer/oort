package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

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
	Sync    SyncRun `json:"sync"`
	JobID   string  `json:"-"`
}

type ImportJob struct {
	ID        string
	TenantID  string
	SyncRunID string
	WorkerID  string
	Attempts  int
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
		r.error, r.created_at, r.started_at, r.finished_at, j.id
		FROM jobs j
		JOIN sync_runs r ON (r.tenant_id, r.id) = (j.tenant_id, j.sync_run_id)
		JOIN datasets d ON (d.tenant_id, d.id) = (r.tenant_id, r.dataset_id)
		WHERE j.tenant_id = $1 AND j.kind = 'dataset_import' AND j.idempotency_key = $2`,
		tenant.ID, idempotencyKey).Scan(append(datasetUploadScan(&existing), &existing.JobID)...)
	if err == nil {
		if existing.Dataset.Slug != slug || existing.Sync.Format != format || existing.Sync.ByteCount != byteCount {
			return DatasetUpload{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DatasetUpload{}, fmt.Errorf("find idempotent upload: %w", err)
	}
	if err := checkStorageQuota(ctx, tx, tenant.ID, byteCount, nil); err != nil {
		return DatasetUpload{}, err
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
	jobID := newID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs
		(id, tenant_id, kind, idempotency_key, sync_run_id, status)
		VALUES ($1, $2, 'dataset_import', $3, $4, 'awaiting_upload')`,
		jobID, tenant.ID, idempotencyKey, run.ID); err != nil {
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
	return DatasetUpload{Dataset: dataset, Sync: run, JobID: jobID}, nil
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
	if changed, _ := result.RowsAffected(); changed > 0 {
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

func getSyncRun(ctx context.Context, query rowQuerier, tenantID, runID string) (SyncRun, error) {
	var run SyncRun
	err := query.QueryRowContext(ctx, `SELECT id, tenant_id, dataset_id, status, format, object_key,
		snapshot_id, row_count, byte_count, COALESCE(schema_json, 'null'::jsonb), error,
		created_at, started_at, finished_at
		FROM sync_runs WHERE tenant_id = $1 AND id = $2`, tenantID, runID).
		Scan(&run.ID, &run.TenantID, &run.DatasetID, &run.Status, &run.Format, &run.ObjectKey,
			&run.SnapshotID, &run.RowCount, &run.ByteCount, &run.Schema, &run.Error,
			&run.CreatedAt, &run.StartedAt, &run.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SyncRun{}, sql.ErrNoRows
	}
	if err != nil {
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
			  AND active.kind IN ('dataset_import', 'connector_sync') AND active.status = 'running'
			  AND active.lease_expires_at >= now()
		  )
		ORDER BY j.available_at, j.created_at
		FOR UPDATE SKIP LOCKED LIMIT 1
	)
	UPDATE jobs j SET status = 'running', leased_by = $1,
		lease_expires_at = now() + $2::interval, attempts = attempts + 1, updated_at = now()
	FROM candidate WHERE j.id = candidate.id
	RETURNING j.id, j.tenant_id, j.sync_run_id, j.attempts`, workerID, lease.String()).
		Scan(&job.ID, &job.TenantID, &job.SyncRunID, &job.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportJob{}, sql.ErrNoRows
	}
	if err != nil {
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
	if errors.Is(err, sql.ErrNoRows) {
		return ImportDetails{}, ErrLeaseLost
	}
	if err != nil {
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
		  AND j.cancel_requested_at IS NULL
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
	message := failureMessage(failure)
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

func datasetUploadScan(upload *DatasetUpload) []any {
	return []any{
		&upload.Dataset.ID, &upload.Dataset.TenantID, &upload.Dataset.Slug,
		&upload.Dataset.CurrentSnapshotID, &upload.Dataset.Schema, &upload.Dataset.RowCount,
		&upload.Dataset.ByteCount, &upload.Dataset.CreatedAt,
		&upload.Sync.ID, &upload.Sync.TenantID, &upload.Sync.DatasetID, &upload.Sync.Status,
		&upload.Sync.Format, &upload.Sync.ObjectKey, &upload.Sync.SnapshotID, &upload.Sync.RowCount,
		&upload.Sync.ByteCount, &upload.Sync.Schema, &upload.Sync.Error, &upload.Sync.CreatedAt,
		&upload.Sync.StartedAt, &upload.Sync.FinishedAt,
	}
}
