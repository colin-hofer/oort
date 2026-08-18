package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Connector struct {
	ID                string     `json:"id"`
	TenantID          string     `json:"tenant_id"`
	DatasetID         string     `json:"dataset_id"`
	DatasetSlug       string     `json:"dataset_slug"`
	Slug              string     `json:"slug"`
	URL               string     `json:"url"`
	RecordsPointer    string     `json:"records_pointer"`
	CursorParameter   *string    `json:"cursor_parameter,omitempty"`
	NextCursorPointer *string    `json:"next_cursor_pointer,omitempty"`
	AuthConfigured    bool       `json:"auth_configured"`
	Enabled           bool       `json:"enabled"`
	RefreshMinutes    int        `json:"refresh_minutes"`
	NextSyncAt        time.Time  `json:"next_sync_at"`
	LastStatus        *string    `json:"last_status,omitempty"`
	LastError         *string    `json:"last_error,omitempty"`
	LastFinishedAt    *time.Time `json:"last_finished_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type ConnectorInput struct {
	Slug              string
	DatasetSlug       string
	URL               string
	RecordsPointer    string
	CursorParameter   *string
	NextCursorPointer *string
	Enabled           bool
	RefreshMinutes    int
	Ciphertext        []byte
	Nonce             []byte
}

type ConnectorDetails struct {
	ImportJob
	ConnectorID       string
	DatasetID         string
	DatasetSlug       string
	URL               string
	RecordsPointer    string
	CursorParameter   *string
	NextCursorPointer *string
	Ciphertext        []byte
	Nonce             []byte
	ObjectKey         string
}

func CreateConnector(ctx context.Context, database *sql.DB, tenant Tenant, actor User, input ConnectorInput, requestID string) (Connector, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Connector{}, err
	}
	defer tx.Rollback()
	datasetID := newID()
	if err := tx.QueryRowContext(ctx, `INSERT INTO datasets (id, tenant_id, slug) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, slug) DO UPDATE SET slug = EXCLUDED.slug RETURNING id`, datasetID, tenant.ID, input.DatasetSlug).Scan(&datasetID); err != nil {
		return Connector{}, fmt.Errorf("create connector dataset: %w", err)
	}
	var secretID *string
	if len(input.Ciphertext) > 0 {
		id := newID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO secret_refs
			(id, tenant_id, kind, ciphertext, nonce, created_by_user_id) VALUES ($1, $2, 'bearer', $3, $4, $5)`,
			id, tenant.ID, input.Ciphertext, input.Nonce, actor.ID); err != nil {
			return Connector{}, fmt.Errorf("store connector secret: %w", err)
		}
		secretID = &id
	}
	connector := Connector{ID: newID(), TenantID: tenant.ID, DatasetID: datasetID, DatasetSlug: input.DatasetSlug,
		Slug: input.Slug, URL: input.URL, RecordsPointer: input.RecordsPointer, CursorParameter: input.CursorParameter,
		NextCursorPointer: input.NextCursorPointer, AuthConfigured: secretID != nil, Enabled: input.Enabled,
		RefreshMinutes: input.RefreshMinutes, NextSyncAt: time.Now().UTC(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	_, err = tx.ExecContext(ctx, `INSERT INTO connectors
		(id, tenant_id, dataset_id, secret_ref_id, slug, url, records_pointer, cursor_parameter,
		next_cursor_pointer, enabled, refresh_minutes, next_sync_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		connector.ID, tenant.ID, datasetID, secretID, input.Slug, input.URL, input.RecordsPointer,
		input.CursorParameter, input.NextCursorPointer, input.Enabled, input.RefreshMinutes,
		connector.NextSyncAt, connector.CreatedAt, connector.UpdatedAt)
	if err != nil {
		if sqlState(err) == "23505" {
			return Connector{}, ErrConflict
		}
		return Connector{}, fmt.Errorf("create connector: %w", err)
	}
	if err := audit(ctx, tx, tenant.ID, actor.ID, "connector.created", "connector", connector.ID, requestID); err != nil {
		return Connector{}, err
	}
	if err := tx.Commit(); err != nil {
		return Connector{}, err
	}
	return connector, nil
}

func ListConnectors(ctx context.Context, database *sql.DB, tenantID string, limit, offset int) ([]Connector, error) {
	rows, err := database.QueryContext(ctx, connectorSelect+` WHERE c.tenant_id = $1 ORDER BY c.updated_at DESC, c.slug LIMIT $2 OFFSET $3`, tenantID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list connectors: %w", err)
	}
	defer rows.Close()
	result := []Connector{}
	for rows.Next() {
		var item Connector
		if err := rows.Scan(connectorScan(&item)...); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func GetConnector(ctx context.Context, database *sql.DB, tenantID, slug string) (Connector, error) {
	var item Connector
	err := database.QueryRowContext(ctx, connectorSelect+` WHERE c.tenant_id = $1 AND c.slug = $2`, tenantID, slug).Scan(connectorScan(&item)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Connector{}, sql.ErrNoRows
	}
	return item, err
}

const connectorSelect = `SELECT c.id, c.tenant_id, c.dataset_id, d.slug, c.slug, c.url,
	c.records_pointer, c.cursor_parameter, c.next_cursor_pointer, c.secret_ref_id IS NOT NULL,
	c.enabled, c.refresh_minutes, c.next_sync_at, latest.status, latest.error, latest.finished_at,
	c.created_at, c.updated_at FROM connectors c
	JOIN datasets d ON (d.tenant_id, d.id) = (c.tenant_id, c.dataset_id)
	LEFT JOIN LATERAL (
		SELECT status, error, finished_at FROM sync_runs
		WHERE tenant_id = c.tenant_id AND connector_id = c.id ORDER BY created_at DESC LIMIT 1
	) latest ON true`

func connectorScan(item *Connector) []any {
	return []any{&item.ID, &item.TenantID, &item.DatasetID, &item.DatasetSlug, &item.Slug, &item.URL,
		&item.RecordsPointer, &item.CursorParameter, &item.NextCursorPointer, &item.AuthConfigured,
		&item.Enabled, &item.RefreshMinutes, &item.NextSyncAt, &item.LastStatus, &item.LastError,
		&item.LastFinishedAt, &item.CreatedAt, &item.UpdatedAt}
}

func UpdateConnector(ctx context.Context, database *sql.DB, tenant Tenant, actor User, slug string, input ConnectorInput, rotateSecret bool, requestID string) (Connector, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Connector{}, err
	}
	defer tx.Rollback()
	var id string
	var secretID *string
	err = tx.QueryRowContext(ctx, `SELECT id, secret_ref_id FROM connectors WHERE tenant_id = $1 AND slug = $2 FOR UPDATE`, tenant.ID, slug).Scan(&id, &secretID)
	if errors.Is(err, sql.ErrNoRows) {
		return Connector{}, sql.ErrNoRows
	}
	if err != nil {
		return Connector{}, err
	}
	if rotateSecret {
		if len(input.Ciphertext) == 0 {
			if secretID != nil {
				_, _ = tx.ExecContext(ctx, `DELETE FROM secret_refs WHERE tenant_id = $1 AND id = $2`, tenant.ID, *secretID)
			}
			secretID = nil
		} else if secretID == nil {
			newSecretID := newID()
			if _, err := tx.ExecContext(ctx, `INSERT INTO secret_refs
				(id, tenant_id, kind, ciphertext, nonce, created_by_user_id) VALUES ($1, $2, 'bearer', $3, $4, $5)`,
				newSecretID, tenant.ID, input.Ciphertext, input.Nonce, actor.ID); err != nil {
				return Connector{}, err
			}
			secretID = &newSecretID
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE secret_refs SET ciphertext = $1, nonce = $2, updated_at = now()
				WHERE tenant_id = $3 AND id = $4`, input.Ciphertext, input.Nonce, tenant.ID, *secretID); err != nil {
				return Connector{}, err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE connectors SET url = $1, records_pointer = $2,
		cursor_parameter = $3, next_cursor_pointer = $4, enabled = $5, refresh_minutes = $6,
		secret_ref_id = $7, next_sync_at = CASE WHEN $5 THEN LEAST(next_sync_at, now()) ELSE next_sync_at END,
		updated_at = now() WHERE tenant_id = $8 AND id = $9`, input.URL, input.RecordsPointer,
		input.CursorParameter, input.NextCursorPointer, input.Enabled, input.RefreshMinutes, secretID, tenant.ID, id)
	if err != nil {
		return Connector{}, fmt.Errorf("update connector: %w", err)
	}
	if err := audit(ctx, tx, tenant.ID, actor.ID, "connector.updated", "connector", id, requestID); err != nil {
		return Connector{}, err
	}
	if err := tx.Commit(); err != nil {
		return Connector{}, err
	}
	return GetConnector(ctx, database, tenant.ID, slug)
}

func DeleteConnector(ctx context.Context, database *sql.DB, tenant Tenant, actor User, slug, requestID string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	var secretID *string
	err = tx.QueryRowContext(ctx, `SELECT id, secret_ref_id FROM connectors WHERE tenant_id = $1 AND slug = $2 FOR UPDATE`, tenant.ID, slug).Scan(&id, &secretID)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sync_runs r JOIN jobs j
		ON (j.tenant_id, j.sync_run_id) = (r.tenant_id, r.id)
		WHERE r.tenant_id = $1 AND r.connector_id = $2 AND j.status IN ('queued', 'running'))`, tenant.ID, id).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM connectors WHERE tenant_id = $1 AND id = $2`, tenant.ID, id); err != nil {
		return err
	}
	if secretID != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM secret_refs WHERE tenant_id = $1 AND id = $2`, tenant.ID, *secretID); err != nil {
			return err
		}
	}
	if err := audit(ctx, tx, tenant.ID, actor.ID, "connector.deleted", "connector", id, requestID); err != nil {
		return err
	}
	return tx.Commit()
}

func CreateConnectorJob(ctx context.Context, database *sql.DB, tenant Tenant, actor *User, connectorID, idempotencyKey, requestID string) (Job, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	job, err := createConnectorJob(ctx, tx, tenant.ID, actor, connectorID, idempotencyKey)
	if err != nil {
		return Job{}, err
	}
	if actor != nil {
		if err := audit(ctx, tx, tenant.ID, actor.ID, "connector.sync_queued", "connector", connectorID, requestID); err != nil {
			return Job{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Job{}, err
	}
	return GetJob(ctx, database, tenant.ID, job.ID)
}

func createConnectorJob(ctx context.Context, tx *sql.Tx, tenantID string, actor *User, connectorID, idempotencyKey string) (Job, error) {
	var datasetID string
	err := tx.QueryRowContext(ctx, `SELECT dataset_id FROM connectors WHERE tenant_id = $1 AND id = $2`, tenantID, connectorID).Scan(&datasetID)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, sql.ErrNoRows
	}
	if err != nil {
		return Job{}, err
	}
	var queued int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE tenant_id = $1 AND status IN ('awaiting_upload', 'queued', 'running')`, tenantID).Scan(&queued); err != nil {
		return Job{}, err
	}
	if queued >= 100 {
		return Job{}, ErrConflict
	}
	syncID, jobID := newID(), newID()
	objectKey := fmt.Sprintf("tenants/%s/connectors/%s/source.json", tenantID, syncID)
	var actorID *string
	if actor != nil {
		actorID = &actor.ID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sync_runs
		(id, tenant_id, dataset_id, actor_user_id, status, format, object_key, byte_count, source, connector_id)
		VALUES ($1, $2, $3, $4, 'queued', 'json', $5, 0, 'connector', $6)`,
		syncID, tenantID, datasetID, actorID, objectKey, connectorID); err != nil {
		return Job{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs
		(id, tenant_id, kind, idempotency_key, sync_run_id, status) VALUES ($1, $2, 'connector_sync', $3, $4, 'queued')`,
		jobID, tenantID, idempotencyKey, syncID); err != nil {
		if sqlState(err) == "23505" {
			return Job{}, ErrConflict
		}
		return Job{}, err
	}
	return Job{ID: jobID, Kind: "connector_sync", Status: "queued", SyncID: &syncID}, nil
}

func EnqueueDueConnector(ctx context.Context, database *sql.DB) (bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var tenantID, connectorID string
	var scheduled time.Time
	err = tx.QueryRowContext(ctx, `SELECT tenant_id, id, next_sync_at FROM connectors
		WHERE enabled AND next_sync_at <= now() ORDER BY next_sync_at, created_at
		FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&tenantID, &connectorID, &scheduled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	key := "scheduled:" + connectorID + ":" + scheduled.UTC().Format(time.RFC3339Nano)
	if _, err := createConnectorJob(ctx, tx, tenantID, nil, connectorID, key); err != nil {
		if !errors.Is(err, ErrConflict) {
			return false, err
		}
		// The per-tenant queue is full. Leave the scheduled interval intact and
		// retry shortly without hot-looping on the same connector.
		if _, err := tx.ExecContext(ctx, `UPDATE connectors SET next_sync_at = now() + interval '1 minute'
			WHERE tenant_id = $1 AND id = $2`, tenantID, connectorID); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `UPDATE connectors SET next_sync_at = now() + refresh_minutes * interval '1 minute', updated_at = now()
		WHERE tenant_id = $1 AND id = $2`, tenantID, connectorID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func ClaimConnectorJob(ctx context.Context, database *sql.DB, workerID string, lease time.Duration) (ImportJob, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return ImportJob{}, err
	}
	defer tx.Rollback()
	var job ImportJob
	err = tx.QueryRowContext(ctx, `WITH candidate AS (
		SELECT j.id FROM jobs j WHERE j.kind = 'connector_sync' AND j.available_at <= now()
		AND (j.status = 'queued' OR (j.status = 'running' AND j.lease_expires_at < now()))
		AND NOT EXISTS (SELECT 1 FROM jobs active WHERE active.tenant_id = j.tenant_id AND active.id <> j.id
			AND active.kind IN ('dataset_import', 'connector_sync') AND active.status = 'running' AND active.lease_expires_at >= now())
		ORDER BY j.available_at, j.created_at FOR UPDATE SKIP LOCKED LIMIT 1)
		UPDATE jobs j SET status = 'running', leased_by = $1, lease_expires_at = now() + $2::interval,
		attempts = attempts + 1, updated_at = now() FROM candidate WHERE j.id = candidate.id
		RETURNING j.id, j.tenant_id, j.sync_run_id, j.attempts`, workerID, lease.String()).Scan(&job.ID, &job.TenantID, &job.SyncRunID, &job.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportJob{}, sql.ErrNoRows
	}
	if err != nil {
		return ImportJob{}, fmt.Errorf("claim connector job: %w", err)
	}
	job.WorkerID = workerID
	if _, err := tx.ExecContext(ctx, `UPDATE sync_runs SET status = 'running', started_at = COALESCE(started_at, now())
		WHERE tenant_id = $1 AND id = $2`, job.TenantID, job.SyncRunID); err != nil {
		return ImportJob{}, err
	}
	return job, tx.Commit()
}

func GetConnectorDetails(ctx context.Context, database *sql.DB, job ImportJob) (ConnectorDetails, error) {
	var details ConnectorDetails
	details.ImportJob = job
	err := database.QueryRowContext(ctx, `SELECT c.id, r.dataset_id, d.slug, c.url, c.records_pointer,
		c.cursor_parameter, c.next_cursor_pointer, COALESCE(s.ciphertext, ''::bytea), COALESCE(s.nonce, ''::bytea), r.object_key
		FROM jobs j JOIN sync_runs r ON (r.tenant_id, r.id) = (j.tenant_id, j.sync_run_id)
		JOIN connectors c ON (c.tenant_id, c.id) = (r.tenant_id, r.connector_id)
		JOIN datasets d ON (d.tenant_id, d.id) = (r.tenant_id, r.dataset_id)
		LEFT JOIN secret_refs s ON (s.tenant_id, s.id) = (c.tenant_id, c.secret_ref_id)
		WHERE j.id = $1 AND j.tenant_id = $2 AND j.status = 'running' AND j.leased_by = $3`,
		job.ID, job.TenantID, job.WorkerID).Scan(&details.ConnectorID, &details.DatasetID, &details.DatasetSlug,
		&details.URL, &details.RecordsPointer, &details.CursorParameter, &details.NextCursorPointer,
		&details.Ciphertext, &details.Nonce, &details.ObjectKey)
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectorDetails{}, ErrLeaseLost
	}
	return details, err
}

func SetConnectorRunBytes(ctx context.Context, database *sql.DB, job ImportJob, byteCount int64) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkStorageQuota(ctx, tx, job.TenantID, byteCount, &job.SyncRunID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sync_runs SET byte_count = $1 WHERE tenant_id = $2 AND id = $3
		AND EXISTS (SELECT 1 FROM jobs WHERE id = $4 AND status = 'running' AND leased_by = $5)`,
		byteCount, job.TenantID, job.SyncRunID, job.ID, job.WorkerID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrLeaseLost
	}
	return tx.Commit()
}

func RetryConnectorJob(ctx context.Context, database *sql.DB, job ImportJob, failure error, delay time.Duration) (bool, error) {
	message := failureMessage(failure)
	result, err := database.ExecContext(ctx, `UPDATE jobs SET status = 'queued', leased_by = NULL,
		lease_expires_at = NULL, available_at = now() + $1::interval, last_error = $2, updated_at = now()
		WHERE id = $3 AND tenant_id = $4 AND status = 'running' AND leased_by = $5 AND attempts < 3`,
		delay.String(), message, job.ID, job.TenantID, job.WorkerID)
	if err != nil {
		return false, err
	}
	if count, _ := result.RowsAffected(); count == 1 {
		_, _ = database.ExecContext(ctx, `UPDATE sync_runs SET status = 'queued', error = $1 WHERE tenant_id = $2 AND id = $3`, message, job.TenantID, job.SyncRunID)
		return true, nil
	}
	return false, nil
}
