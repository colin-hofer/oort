package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Job struct {
	ID                string     `json:"id"`
	Kind              string     `json:"kind"`
	Status            string     `json:"status"`
	DatasetSlug       *string    `json:"dataset_slug,omitempty"`
	AppSlug           *string    `json:"app_slug,omitempty"`
	SyncID            *string    `json:"sync_id,omitempty"`
	DeploymentID      *string    `json:"deployment_id,omitempty"`
	Attempts          int        `json:"attempts"`
	SnapshotID        *int64     `json:"snapshot_id,omitempty"`
	RowCount          *int64     `json:"row_count,omitempty"`
	ByteCount         *int64     `json:"byte_count,omitempty"`
	Error             *string    `json:"error,omitempty"`
	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type JobLog struct {
	Sequence  int64     `json:"sequence"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

func ListJobs(ctx context.Context, database *sql.DB, tenantID string, limit, offset int) ([]Job, error) {
	rows, err := database.QueryContext(ctx, jobSelect+` WHERE j.tenant_id = $1 ORDER BY j.created_at DESC LIMIT $2 OFFSET $3`, tenantID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	result := []Job{}
	for rows.Next() {
		var job Job
		if err := rows.Scan(jobScan(&job)...); err != nil {
			return nil, err
		}
		result = append(result, job)
	}
	return result, rows.Err()
}

func GetJob(ctx context.Context, database *sql.DB, tenantID, jobID string) (Job, error) {
	var job Job
	err := database.QueryRowContext(ctx, jobSelect+` WHERE j.tenant_id = $1 AND j.id = $2`, tenantID, jobID).Scan(jobScan(&job)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, sql.ErrNoRows
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}
	return job, nil
}

func GetJobBySync(ctx context.Context, database *sql.DB, tenantID, syncRunID string) (Job, error) {
	var job Job
	err := database.QueryRowContext(ctx, jobSelect+` WHERE j.tenant_id = $1 AND j.sync_run_id = $2`, tenantID, syncRunID).Scan(jobScan(&job)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, sql.ErrNoRows
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job by upload: %w", err)
	}
	return job, nil
}

func GetJobByDeployment(ctx context.Context, database *sql.DB, tenantID, deploymentID string) (Job, error) {
	var job Job
	err := database.QueryRowContext(ctx, jobSelect+` WHERE j.tenant_id = $1 AND j.deployment_id = $2`, tenantID, deploymentID).Scan(jobScan(&job)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, sql.ErrNoRows
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job by deployment: %w", err)
	}
	return job, nil
}

const jobSelect = `SELECT j.id, j.kind, j.status, d.slug, a.slug, j.sync_run_id, j.deployment_id,
	j.attempts, sr.snapshot_id, sr.row_count, COALESCE(sr.byte_count, dep.byte_count),
	j.last_error, j.cancel_requested_at, j.created_at, j.updated_at
	FROM jobs j
	LEFT JOIN sync_runs sr ON (sr.tenant_id, sr.id) = (j.tenant_id, j.sync_run_id)
	LEFT JOIN datasets d ON (d.tenant_id, d.id) = (sr.tenant_id, sr.dataset_id)
	LEFT JOIN deployments dep ON (dep.tenant_id, dep.id) = (j.tenant_id, j.deployment_id)
	LEFT JOIN apps a ON (a.tenant_id, a.id) = (dep.tenant_id, dep.app_id)`

func jobScan(job *Job) []any {
	return []any{&job.ID, &job.Kind, &job.Status, &job.DatasetSlug, &job.AppSlug, &job.SyncID,
		&job.DeploymentID, &job.Attempts, &job.SnapshotID, &job.RowCount, &job.ByteCount,
		&job.Error, &job.CancelRequestedAt, &job.CreatedAt, &job.UpdatedAt}
}

func CancelJob(ctx context.Context, database *sql.DB, tenant Tenant, actor User, jobID, requestID string) (Job, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	var status string
	var syncID, deploymentID *string
	err = tx.QueryRowContext(ctx, `SELECT status, sync_run_id, deployment_id FROM jobs
		WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenant.ID, jobID).Scan(&status, &syncID, &deploymentID)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, sql.ErrNoRows
	}
	if err != nil {
		return Job{}, err
	}
	switch status {
	case "awaiting_upload", "queued":
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'cancelled', cancel_requested_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND id = $2`, tenant.ID, jobID); err != nil {
			return Job{}, err
		}
		if syncID != nil {
			_, _ = tx.ExecContext(ctx, `UPDATE sync_runs SET status = 'cancelled', cancel_requested_at = now(), finished_at = now()
				WHERE tenant_id = $1 AND id = $2`, tenant.ID, *syncID)
		}
		if deploymentID != nil {
			_, _ = tx.ExecContext(ctx, `UPDATE deployments SET status = 'cancelled', error = 'cancelled'
				WHERE tenant_id = $1 AND id = $2`, tenant.ID, *deploymentID)
		}
	case "running":
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET cancel_requested_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND id = $2`, tenant.ID, jobID); err != nil {
			return Job{}, err
		}
		if syncID != nil {
			_, _ = tx.ExecContext(ctx, `UPDATE sync_runs SET cancel_requested_at = now() WHERE tenant_id = $1 AND id = $2`, tenant.ID, *syncID)
		}
	case "succeeded", "failed", "cancelled":
		return Job{}, ErrConflict
	}
	if err := audit(ctx, tx, tenant.ID, actor.ID, "job.cancel_requested", "job", jobID, requestID); err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, err
	}
	return GetJob(ctx, database, tenant.ID, jobID)
}

func JobCancellationRequested(ctx context.Context, database *sql.DB, tenantID, jobID, workerID string) (bool, error) {
	var requested bool
	err := database.QueryRowContext(ctx, `SELECT cancel_requested_at IS NOT NULL FROM jobs
		WHERE tenant_id = $1 AND id = $2 AND status = 'running' AND leased_by = $3`, tenantID, jobID, workerID).Scan(&requested)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrLeaseLost
	}
	return requested, err
}

func CancelClaimedJob(ctx context.Context, database *sql.DB, tenantID, jobID, workerID string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var syncID, deploymentID *string
	err = tx.QueryRowContext(ctx, `UPDATE jobs SET status = 'cancelled', leased_by = NULL, lease_expires_at = NULL,
		updated_at = now() WHERE tenant_id = $1 AND id = $2 AND status = 'running' AND leased_by = $3
		RETURNING sync_run_id, deployment_id`, tenantID, jobID, workerID).Scan(&syncID, &deploymentID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if syncID != nil {
		_, _ = tx.ExecContext(ctx, `UPDATE sync_runs SET status = 'cancelled', finished_at = now() WHERE tenant_id = $1 AND id = $2`, tenantID, *syncID)
	}
	if deploymentID != nil {
		_, _ = tx.ExecContext(ctx, `UPDATE deployments SET status = 'cancelled', error = 'cancelled' WHERE tenant_id = $1 AND id = $2`, tenantID, *deploymentID)
	}
	return tx.Commit()
}

func AppendJobLog(ctx context.Context, database *sql.DB, tenantID, jobID, level, message string) error {
	message = failureMessage(errors.New(message))
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var locked string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, jobID).Scan(&locked); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO job_logs (tenant_id, job_id, sequence, level, message)
		SELECT $1, $2, COALESCE(max(sequence), 0) + 1, $3, $4 FROM job_logs
		WHERE tenant_id = $1 AND job_id = $2`, tenantID, jobID, level, message)
	if err != nil {
		return fmt.Errorf("append job log: %w", err)
	}
	return tx.Commit()
}

func ListJobLogs(ctx context.Context, database *sql.DB, tenantID, jobID string, after int64, limit int) ([]JobLog, error) {
	rows, err := database.QueryContext(ctx, `SELECT sequence, level, message, created_at FROM job_logs
		WHERE tenant_id = $1 AND job_id = $2 AND sequence > $3 ORDER BY sequence LIMIT $4`, tenantID, jobID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []JobLog{}
	for rows.Next() {
		var item JobLog
		if err := rows.Scan(&item.Sequence, &item.Level, &item.Message, &item.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
