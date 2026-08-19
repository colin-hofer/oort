package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func DeleteDataset(ctx context.Context, database *sql.DB, tenant Tenant, actor User, slug, requestID string, beforeDelete func() error) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM datasets WHERE tenant_id = $1 AND slug = $2 FOR UPDATE`, tenant.ID, slug).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	} else if err != nil {
		return err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM sync_runs r JOIN jobs j ON (j.tenant_id, j.sync_run_id) = (r.tenant_id, r.id)
		WHERE r.tenant_id = $1 AND r.dataset_id = $2 AND j.status IN ('awaiting_upload', 'queued', 'running')
	)`, tenant.ID, id).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT secret_ref_id FROM connectors
		WHERE tenant_id = $1 AND dataset_id = $2 AND secret_ref_id IS NOT NULL`, tenant.ID, id)
	if err != nil {
		return err
	}
	var secretIDs []string
	for rows.Next() {
		var secretID string
		if err := rows.Scan(&secretID); err != nil {
			rows.Close()
			return err
		}
		secretIDs = append(secretIDs, secretID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if beforeDelete != nil {
		if err := beforeDelete(); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM datasets WHERE tenant_id = $1 AND id = $2`, tenant.ID, id); err != nil {
		return err
	}
	for _, secretID := range secretIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM secret_refs WHERE tenant_id = $1 AND id = $2`, tenant.ID, secretID); err != nil {
			return err
		}
	}
	if err := audit(ctx, tx, tenant.ID, actor.ID, "dataset.deleted", "dataset", id, requestID); err != nil {
		return err
	}
	return tx.Commit()
}

func ListDatasets(ctx context.Context, database *sql.DB, tenantID string, limit, offset int) ([]DatasetSummary, error) {
	rows, err := database.QueryContext(ctx, `SELECT d.id, d.slug, d.current_snapshot_id,
		COALESCE(d.schema_json, 'null'::jsonb), d.row_count, d.byte_count, latest.status,
		d.created_at, d.updated_at
		FROM datasets d LEFT JOIN LATERAL (
			SELECT status FROM sync_runs WHERE tenant_id = d.tenant_id AND dataset_id = d.id
			ORDER BY created_at DESC LIMIT 1
		) latest ON true
		WHERE d.tenant_id = $1 ORDER BY d.updated_at DESC, d.slug LIMIT $2 OFFSET $3`, tenantID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list datasets: %w", err)
	}
	defer rows.Close()
	result := []DatasetSummary{}
	for rows.Next() {
		var item DatasetSummary
		if err := rows.Scan(&item.ID, &item.Slug, &item.CurrentSnapshotID, &item.Schema, &item.RowCount,
			&item.ByteCount, &item.LastSyncStatus, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan dataset: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func DeleteApp(ctx context.Context, database *sql.DB, tenant Tenant, actor User, slug, requestID string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM apps WHERE tenant_id = $1 AND slug = $2 FOR UPDATE`, tenant.ID, slug).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	} else if err != nil {
		return err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM deployments d JOIN jobs j ON (j.tenant_id, j.deployment_id) = (d.tenant_id, d.id)
		WHERE d.tenant_id = $1 AND d.app_id = $2 AND j.status IN ('awaiting_upload', 'queued', 'running')
	)`, tenant.ID, id).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE apps SET current_deployment_id = NULL WHERE tenant_id = $1 AND id = $2`, tenant.ID, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deployments SET previous_deployment_id = NULL WHERE tenant_id = $1 AND app_id = $2`, tenant.ID, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM apps WHERE tenant_id = $1 AND id = $2`, tenant.ID, id); err != nil {
		return err
	}
	if err := audit(ctx, tx, tenant.ID, actor.ID, "app.deleted", "app", id, requestID); err != nil {
		return err
	}
	return tx.Commit()
}

func GetDataset(ctx context.Context, database *sql.DB, tenantID, slug string) (DatasetSummary, error) {
	var item DatasetSummary
	err := database.QueryRowContext(ctx, `SELECT d.id, d.slug, d.current_snapshot_id,
		COALESCE(d.schema_json, 'null'::jsonb), d.row_count, d.byte_count, latest.status,
		d.created_at, d.updated_at
		FROM datasets d LEFT JOIN LATERAL (
			SELECT status FROM sync_runs WHERE tenant_id = d.tenant_id AND dataset_id = d.id
			ORDER BY created_at DESC LIMIT 1
		) latest ON true WHERE d.tenant_id = $1 AND d.slug = $2`, tenantID, slug).
		Scan(&item.ID, &item.Slug, &item.CurrentSnapshotID, &item.Schema, &item.RowCount,
			&item.ByteCount, &item.LastSyncStatus, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DatasetSummary{}, sql.ErrNoRows
	}
	return item, err
}

func ListDatasetSyncs(ctx context.Context, database *sql.DB, tenantID, datasetID string, limit, offset int) ([]DatasetSync, error) {
	rows, err := database.QueryContext(ctx, `SELECT r.id, r.dataset_id, d.slug, r.status, r.format, r.snapshot_id,
		r.row_count, r.byte_count, r.error, r.created_at, r.started_at, r.finished_at
		FROM sync_runs r JOIN datasets d ON (d.tenant_id, d.id) = (r.tenant_id, r.dataset_id)
		WHERE r.tenant_id = $1 AND r.dataset_id = $2 ORDER BY r.created_at DESC LIMIT $3 OFFSET $4`, tenantID, datasetID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list dataset syncs: %w", err)
	}
	defer rows.Close()
	result := []DatasetSync{}
	for rows.Next() {
		var item DatasetSync
		if err := rows.Scan(&item.ID, &item.DatasetID, &item.DatasetSlug, &item.Status, &item.Format,
			&item.SnapshotID, &item.RowCount, &item.ByteCount, &item.Error, &item.CreatedAt,
			&item.StartedAt, &item.FinishedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func ListQueries(ctx context.Context, database *sql.DB, tenantID string, limit, offset int) ([]QueryRevision, error) {
	return queryRows(ctx, database, `SELECT r.id, r.tenant_id, r.query_id, q.slug, r.version,
		r.sql_text, r.parameter_types, r.created_at FROM queries q
		JOIN query_revisions r ON (r.tenant_id, r.id) = (q.tenant_id, q.current_revision_id)
		WHERE q.tenant_id = $1 ORDER BY q.updated_at DESC, q.slug LIMIT $2 OFFSET $3`, tenantID, limit, offset)
}

func ListQueryRevisions(ctx context.Context, database *sql.DB, tenantID, slug string, limit, offset int) ([]QueryRevision, error) {
	return queryRows(ctx, database, `SELECT r.id, r.tenant_id, r.query_id, q.slug, r.version,
		r.sql_text, r.parameter_types, r.created_at FROM queries q
		JOIN query_revisions r ON (r.tenant_id, r.query_id) = (q.tenant_id, q.id)
		WHERE q.tenant_id = $1 AND q.slug = $2 ORDER BY r.version DESC LIMIT $3 OFFSET $4`, tenantID, slug, limit, offset)
}

func GetCurrentQuery(ctx context.Context, database *sql.DB, tenantID, slug string) (QueryRevision, error) {
	rows, err := queryRows(ctx, database, `SELECT r.id, r.tenant_id, r.query_id, q.slug, r.version,
		r.sql_text, r.parameter_types, r.created_at FROM queries q
		JOIN query_revisions r ON (r.tenant_id, r.id) = (q.tenant_id, q.current_revision_id)
		WHERE q.tenant_id = $1 AND q.slug = $2`, tenantID, slug)
	if err != nil {
		return QueryRevision{}, err
	}
	if len(rows) == 0 {
		return QueryRevision{}, sql.ErrNoRows
	}
	return rows[0], nil
}

func queryRows(ctx context.Context, database *sql.DB, statement string, args ...any) ([]QueryRevision, error) {
	rows, err := database.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list queries: %w", err)
	}
	defer rows.Close()
	result := []QueryRevision{}
	for rows.Next() {
		var item QueryRevision
		var types []byte
		if err := rows.Scan(&item.ID, &item.TenantID, &item.QueryID, &item.Slug, &item.Version,
			&item.SQL, &types, &item.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(types, &item.ParameterTypes); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func ListApps(ctx context.Context, database *sql.DB, tenantID string, limit, offset int) ([]AppSummary, error) {
	rows, err := database.QueryContext(ctx, `SELECT a.id, a.slug, a.current_deployment_id,
		d.version, d.status, a.created_at, a.updated_at FROM apps a
		LEFT JOIN deployments d ON (d.tenant_id, d.id) = (a.tenant_id, a.current_deployment_id)
		WHERE a.tenant_id = $1 ORDER BY a.updated_at DESC, a.slug LIMIT $2 OFFSET $3`, tenantID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	defer rows.Close()
	result := []AppSummary{}
	for rows.Next() {
		var item AppSummary
		if err := rows.Scan(&item.ID, &item.Slug, &item.CurrentDeploymentID, &item.CurrentVersion,
			&item.CurrentStatus, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func ListDeployments(ctx context.Context, database *sql.DB, tenantID, appSlug string, limit, offset int) ([]Deployment, error) {
	rows, err := database.QueryContext(ctx, `SELECT d.id, d.tenant_id, d.app_id, a.slug,
		d.previous_deployment_id, d.version, d.status, d.object_key, d.checksum, d.byte_count,
		d.manifest_json, d.error, d.created_at, d.published_at
		FROM deployments d JOIN apps a ON (a.tenant_id, a.id) = (d.tenant_id, d.app_id)
		WHERE d.tenant_id = $1 AND ($2 = '' OR a.slug = $2)
		ORDER BY d.created_at DESC LIMIT $3 OFFSET $4`, tenantID, appSlug, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()
	result := []Deployment{}
	for rows.Next() {
		var item Deployment
		if err := rows.Scan(deploymentScan(&item)...); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
