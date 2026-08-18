package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"nebulous/internal/manifest"
)

type Deployment struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenant_id"`
	AppID       string          `json:"app_id"`
	AppSlug     string          `json:"app_slug"`
	PreviousID  *string         `json:"previous_deployment_id,omitempty"`
	Version     int             `json:"version"`
	Status      string          `json:"status"`
	ObjectKey   string          `json:"-"`
	Checksum    []byte          `json:"-"`
	ByteCount   int64           `json:"byte_count"`
	Manifest    json.RawMessage `json:"manifest"`
	Error       *string         `json:"error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	PublishedAt *time.Time      `json:"published_at,omitempty"`
}

type PublishJob struct {
	ID           string
	TenantID     string
	DeploymentID string
	WorkerID     string
}

type PublishDetails struct {
	PublishJob
	Deployment Deployment
	ActorID    string
}

func CreateDeployment(ctx context.Context, database *sql.DB, tenant Tenant, actor User, m manifest.Manifest, checksum []byte, byteCount int64, idempotencyKey, requestID string) (Deployment, error) {
	manifestJSON, err := json.Marshal(m)
	if err != nil {
		return Deployment{}, fmt.Errorf("encode deployment manifest: %w", err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Deployment{}, fmt.Errorf("start deployment: %w", err)
	}
	defer tx.Rollback()
	var existing Deployment
	err = tx.QueryRowContext(ctx, `SELECT d.id, d.tenant_id, d.app_id, a.slug, d.previous_deployment_id, d.version, d.status,
		d.object_key, d.checksum, d.byte_count, d.manifest_json, d.error, d.created_at, d.published_at
		FROM jobs j
		JOIN deployments d ON (d.tenant_id, d.id) = (j.tenant_id, j.deployment_id)
		JOIN apps a ON (a.tenant_id, a.id) = (d.tenant_id, d.app_id)
		WHERE j.tenant_id = $1 AND j.kind = 'app_publish' AND j.idempotency_key = $2`,
		tenant.ID, idempotencyKey).Scan(deploymentScan(&existing)...)
	if err == nil {
		if existing.AppSlug != m.App.Slug || existing.ByteCount != byteCount ||
			!equalJSON(existing.Manifest, manifestJSON) || !bytes.Equal(existing.Checksum, checksum) {
			return Deployment{}, ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, fmt.Errorf("find idempotent deployment: %w", err)
	}
	if err := checkStorageQuota(ctx, tx, tenant.ID, byteCount, nil); err != nil {
		return Deployment{}, err
	}
	appID := newID()
	if err := tx.QueryRowContext(ctx, `INSERT INTO apps (id, tenant_id, slug) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, slug) DO UPDATE SET slug = EXCLUDED.slug
		RETURNING id`, appID, tenant.ID, m.App.Slug).Scan(&appID); err != nil {
		return Deployment{}, fmt.Errorf("create app: %w", err)
	}
	var previousID *string
	if err := tx.QueryRowContext(ctx, `SELECT current_deployment_id FROM apps
		WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenant.ID, appID).Scan(&previousID); err != nil {
		return Deployment{}, fmt.Errorf("read current deployment: %w", err)
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM deployments
		WHERE tenant_id = $1 AND app_id = $2`, tenant.ID, appID).Scan(&version); err != nil {
		return Deployment{}, fmt.Errorf("allocate deployment version: %w", err)
	}
	deployment := Deployment{ID: newID(), TenantID: tenant.ID, AppID: appID, AppSlug: m.App.Slug, PreviousID: previousID,
		Version: version, Status: "awaiting_upload", Checksum: checksum, ByteCount: byteCount,
		Manifest: manifestJSON, CreatedAt: time.Now().UTC()}
	deployment.ObjectKey = fmt.Sprintf("tenants/%s/apps/%s/deployments/%s/bundle.zip", tenant.ID, appID, deployment.ID)
	if _, err := tx.ExecContext(ctx, `INSERT INTO deployments
		(id, tenant_id, app_id, actor_user_id, previous_deployment_id, version, status, object_key, checksum, byte_count, manifest_json, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`, deployment.ID, tenant.ID,
		appID, actor.ID, previousID, version, deployment.Status, deployment.ObjectKey, checksum, byteCount,
		manifestJSON, deployment.CreatedAt); err != nil {
		return Deployment{}, fmt.Errorf("create deployment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs
		(id, tenant_id, kind, idempotency_key, deployment_id, status)
		VALUES ($1, $2, 'app_publish', $3, $4, 'awaiting_upload')`, newID(), tenant.ID,
		idempotencyKey, deployment.ID); err != nil {
		if sqlState(err) == "23505" {
			return Deployment{}, ErrConflict
		}
		return Deployment{}, fmt.Errorf("create publish job: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events
		(id, tenant_id, actor_user_id, action, resource_type, resource_id, request_id)
		VALUES ($1, $2, $3, 'deployment.upload_created', 'deployment', $4, $5)`, newID(),
		tenant.ID, actor.ID, deployment.ID, requestID); err != nil {
		return Deployment{}, fmt.Errorf("audit deployment creation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Deployment{}, fmt.Errorf("commit deployment: %w", err)
	}
	return deployment, nil
}

func CompleteDeploymentUpload(ctx context.Context, database *sql.DB, tenantID, deploymentID string) (Deployment, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Deployment{}, fmt.Errorf("start deployment upload completion: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE deployments SET status = 'queued'
		WHERE tenant_id = $1 AND id = $2 AND status = 'awaiting_upload'`, tenantID, deploymentID)
	if err != nil {
		return Deployment{}, fmt.Errorf("queue deployment: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'queued', available_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND deployment_id = $2 AND status = 'awaiting_upload'`, tenantID, deploymentID); err != nil {
			return Deployment{}, fmt.Errorf("queue publish job: %w", err)
		}
	}
	deployment, err := getDeployment(ctx, tx, tenantID, deploymentID)
	if err != nil {
		return Deployment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Deployment{}, fmt.Errorf("commit deployment upload completion: %w", err)
	}
	return deployment, nil
}

func GetDeployment(ctx context.Context, database *sql.DB, tenantID, deploymentID string) (Deployment, error) {
	return getDeployment(ctx, database, tenantID, deploymentID)
}

func getDeployment(ctx context.Context, query rowQuerier, tenantID, deploymentID string) (Deployment, error) {
	var deployment Deployment
	err := query.QueryRowContext(ctx, `SELECT d.id, d.tenant_id, d.app_id, a.slug, d.previous_deployment_id, d.version, d.status,
		d.object_key, d.checksum, d.byte_count, d.manifest_json, d.error, d.created_at, d.published_at
		FROM deployments d JOIN apps a ON (a.tenant_id, a.id) = (d.tenant_id, d.app_id)
		WHERE d.tenant_id = $1 AND d.id = $2`, tenantID, deploymentID).Scan(deploymentScan(&deployment)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, sql.ErrNoRows
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("get deployment: %w", err)
	}
	return deployment, nil
}

func ClaimPublishJob(ctx context.Context, database *sql.DB, workerID string, lease time.Duration) (PublishJob, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return PublishJob{}, fmt.Errorf("start publish job claim: %w", err)
	}
	defer tx.Rollback()
	var job PublishJob
	err = tx.QueryRowContext(ctx, `WITH candidate AS (
		SELECT j.id FROM jobs j
		WHERE j.kind = 'app_publish' AND j.available_at <= now()
		  AND (j.status = 'queued' OR (j.status = 'running' AND j.lease_expires_at < now()))
		ORDER BY j.available_at, j.created_at FOR UPDATE SKIP LOCKED LIMIT 1
	)
	UPDATE jobs j SET status = 'running', leased_by = $1,
		lease_expires_at = now() + $2::interval, attempts = attempts + 1, updated_at = now()
	FROM candidate WHERE j.id = candidate.id
	RETURNING j.id, j.tenant_id, j.deployment_id`, workerID, lease.String()).
		Scan(&job.ID, &job.TenantID, &job.DeploymentID)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishJob{}, sql.ErrNoRows
	}
	if err != nil {
		return PublishJob{}, fmt.Errorf("claim publish job: %w", err)
	}
	job.WorkerID = workerID
	if _, err := tx.ExecContext(ctx, `UPDATE deployments SET status = 'running'
		WHERE tenant_id = $1 AND id = $2`, job.TenantID, job.DeploymentID); err != nil {
		return PublishJob{}, fmt.Errorf("start deployment publish: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PublishJob{}, fmt.Errorf("commit publish job claim: %w", err)
	}
	return job, nil
}

func GetPublishDetails(ctx context.Context, database *sql.DB, job PublishJob) (PublishDetails, error) {
	var details PublishDetails
	details.PublishJob = job
	err := database.QueryRowContext(ctx, `SELECT d.id, d.tenant_id, d.app_id, a.slug, d.previous_deployment_id, d.version, d.status,
		d.object_key, d.checksum, d.byte_count, d.manifest_json, d.error, d.created_at, d.published_at,
		d.actor_user_id
		FROM jobs j JOIN deployments d ON (d.tenant_id, d.id) = (j.tenant_id, j.deployment_id)
		JOIN apps a ON (a.tenant_id, a.id) = (d.tenant_id, d.app_id)
		WHERE j.id = $1 AND j.tenant_id = $2 AND j.status = 'running' AND j.leased_by = $3`,
		job.ID, job.TenantID, job.WorkerID).Scan(append(deploymentScan(&details.Deployment), &details.ActorID)...)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishDetails{}, ErrLeaseLost
	}
	if err != nil {
		return PublishDetails{}, fmt.Errorf("load publish job: %w", err)
	}
	return details, nil
}

func HeartbeatPublishJob(ctx context.Context, database *sql.DB, job PublishJob, lease time.Duration) error {
	result, err := database.ExecContext(ctx, `UPDATE jobs SET lease_expires_at = now() + $1::interval, updated_at = now()
		WHERE id = $2 AND tenant_id = $3 AND status = 'running' AND leased_by = $4`,
		lease.String(), job.ID, job.TenantID, job.WorkerID)
	if err != nil {
		return fmt.Errorf("heartbeat publish job: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrLeaseLost
	}
	return nil
}

func CompletePublishJob(ctx context.Context, database *sql.DB, job PublishJob, queries []PublishedQuery) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start publish completion: %w", err)
	}
	defer tx.Rollback()
	var appID, actorID string
	err = tx.QueryRowContext(ctx, `SELECT d.app_id, d.actor_user_id FROM jobs j
		JOIN deployments d ON (d.tenant_id, d.id) = (j.tenant_id, j.deployment_id)
		WHERE j.id = $1 AND j.tenant_id = $2 AND j.status = 'running' AND j.leased_by = $3
		  AND j.cancel_requested_at IS NULL
		FOR UPDATE`, job.ID, job.TenantID, job.WorkerID).Scan(&appID, &actorID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("lock publish job: %w", err)
	}
	for _, query := range queries {
		revisionID, err := savePublishedQuery(ctx, tx, job.TenantID, query)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO deployment_queries
			(tenant_id, deployment_id, query_revision_id) VALUES ($1, $2, $3)`,
			job.TenantID, job.DeploymentID, revisionID); err != nil {
			return fmt.Errorf("grant deployment query %q: %w", query.Name, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deployments SET status = 'succeeded', published_at = now(), error = NULL
		WHERE tenant_id = $1 AND id = $2`, job.TenantID, job.DeploymentID); err != nil {
		return fmt.Errorf("publish deployment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE apps SET current_deployment_id = $1, updated_at = now()
		WHERE tenant_id = $2 AND id = $3`, job.DeploymentID, job.TenantID, appID); err != nil {
		return fmt.Errorf("promote deployment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'succeeded', leased_by = NULL,
		lease_expires_at = NULL, updated_at = now(), last_error = NULL WHERE id = $1`, job.ID); err != nil {
		return fmt.Errorf("complete publish job: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events
		(id, tenant_id, actor_user_id, action, resource_type, resource_id, request_id)
		VALUES ($1, $2, $3, 'deployment.published', 'deployment', $4, $5)`, newID(), job.TenantID,
		actorID, job.DeploymentID, newID()); err != nil {
		return fmt.Errorf("audit deployment publish: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit publish completion: %w", err)
	}
	return nil
}

func FailPublishJob(ctx context.Context, database *sql.DB, job PublishJob, failure error) error {
	message := failureMessage(failure)
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start publish failure: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'failed', leased_by = NULL,
		lease_expires_at = NULL, last_error = $1, updated_at = now()
		WHERE id = $2 AND tenant_id = $3 AND status = 'running' AND leased_by = $4`,
		message, job.ID, job.TenantID, job.WorkerID)
	if err != nil {
		return fmt.Errorf("fail publish job: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrLeaseLost
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deployments SET status = 'failed', error = $1
		WHERE tenant_id = $2 AND id = $3`, message, job.TenantID, job.DeploymentID); err != nil {
		return fmt.Errorf("fail deployment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit publish failure: %w", err)
	}
	return nil
}

func RollbackDeployment(ctx context.Context, database *sql.DB, tenant Tenant, actor User, deploymentID, requestID string) (Deployment, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Deployment{}, fmt.Errorf("start deployment rollback: %w", err)
	}
	defer tx.Rollback()
	deployment, err := getDeployment(ctx, tx, tenant.ID, deploymentID)
	if err != nil || deployment.Status != "succeeded" {
		if err == nil {
			err = sql.ErrNoRows
		}
		return Deployment{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE apps SET current_deployment_id = $1, updated_at = now()
		WHERE tenant_id = $2 AND id = $3`, deployment.ID, tenant.ID, deployment.AppID)
	if err != nil {
		return Deployment{}, fmt.Errorf("rollback app: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return Deployment{}, sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events
		(id, tenant_id, actor_user_id, action, resource_type, resource_id, request_id)
		VALUES ($1, $2, $3, 'deployment.rolled_back', 'deployment', $4, $5)`, newID(), tenant.ID,
		actor.ID, deployment.ID, requestID); err != nil {
		return Deployment{}, fmt.Errorf("audit deployment rollback: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Deployment{}, fmt.Errorf("commit deployment rollback: %w", err)
	}
	return deployment, nil
}

func deploymentScan(deployment *Deployment) []any {
	return []any{&deployment.ID, &deployment.TenantID, &deployment.AppID, &deployment.AppSlug, &deployment.PreviousID,
		&deployment.Version, &deployment.Status, &deployment.ObjectKey, &deployment.Checksum,
		&deployment.ByteCount, &deployment.Manifest, &deployment.Error, &deployment.CreatedAt,
		&deployment.PublishedAt}
}
