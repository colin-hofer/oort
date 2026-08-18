package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type Dashboard struct {
	Tenant      Tenant            `json:"tenant"`
	Datasets    []DatasetSummary  `json:"datasets"`
	Queries     []QueryRevision   `json:"queries"`
	Apps        []AppSummary      `json:"apps"`
	Deployments []Deployment      `json:"deployments"`
	Syncs       []DatasetSync     `json:"syncs"`
	Activity    []ActivitySummary `json:"activity"`
}

type DatasetSummary struct {
	ID                string          `json:"id"`
	Slug              string          `json:"slug"`
	CurrentSnapshotID *int64          `json:"current_snapshot_id,omitempty"`
	Schema            json.RawMessage `json:"schema,omitempty"`
	RowCount          *int64          `json:"row_count,omitempty"`
	ByteCount         *int64          `json:"byte_count,omitempty"`
	LastSyncStatus    *string         `json:"last_sync_status,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

type AppSummary struct {
	ID                  string    `json:"id"`
	Slug                string    `json:"slug"`
	CurrentDeploymentID *string   `json:"current_deployment_id,omitempty"`
	CurrentVersion      *int      `json:"current_version,omitempty"`
	CurrentStatus       *string   `json:"current_status,omitempty"`
	URL                 string    `json:"url,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type DatasetSync struct {
	ID          string     `json:"id"`
	DatasetID   string     `json:"dataset_id"`
	DatasetSlug string     `json:"dataset_slug"`
	Status      string     `json:"status"`
	Format      string     `json:"format"`
	SnapshotID  *int64     `json:"snapshot_id,omitempty"`
	RowCount    *int64     `json:"row_count,omitempty"`
	ByteCount   int64      `json:"byte_count"`
	Error       *string    `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

type ActivitySummary struct {
	ID           string    `json:"id"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	RequestID    string    `json:"request_id"`
	CreatedAt    time.Time `json:"created_at"`
}

func LoadDashboard(ctx context.Context, database *sql.DB, tenant Tenant) (Dashboard, error) {
	dashboard := Dashboard{
		Tenant: tenant, Datasets: []DatasetSummary{}, Queries: []QueryRevision{}, Apps: []AppSummary{},
		Deployments: []Deployment{}, Syncs: []DatasetSync{}, Activity: []ActivitySummary{},
	}

	rows, err := database.QueryContext(ctx, `SELECT d.id, d.slug, d.current_snapshot_id,
		COALESCE(d.schema_json, 'null'::jsonb), d.row_count, d.byte_count, latest.status,
		d.created_at, d.updated_at
		FROM datasets d
		LEFT JOIN LATERAL (
			SELECT status FROM sync_runs WHERE tenant_id = d.tenant_id AND dataset_id = d.id
			ORDER BY created_at DESC LIMIT 1
		) latest ON true
		WHERE d.tenant_id = $1 ORDER BY d.updated_at DESC, d.slug`, tenant.ID)
	if err != nil {
		return Dashboard{}, fmt.Errorf("list dashboard datasets: %w", err)
	}
	for rows.Next() {
		var dataset DatasetSummary
		if err := rows.Scan(&dataset.ID, &dataset.Slug, &dataset.CurrentSnapshotID, &dataset.Schema,
			&dataset.RowCount, &dataset.ByteCount, &dataset.LastSyncStatus, &dataset.CreatedAt, &dataset.UpdatedAt); err != nil {
			rows.Close()
			return Dashboard{}, fmt.Errorf("scan dashboard dataset: %w", err)
		}
		dashboard.Datasets = append(dashboard.Datasets, dataset)
	}
	if err := rows.Close(); err != nil {
		return Dashboard{}, fmt.Errorf("close dashboard datasets: %w", err)
	}
	if err := rows.Err(); err != nil {
		return Dashboard{}, fmt.Errorf("list dashboard datasets: %w", err)
	}

	rows, err = database.QueryContext(ctx, `SELECT r.id, r.tenant_id, r.query_id, q.slug, r.version,
		r.sql_text, r.parameter_types, r.created_at
		FROM queries q
		JOIN query_revisions r ON (r.tenant_id, r.id) = (q.tenant_id, q.current_revision_id)
		WHERE q.tenant_id = $1 ORDER BY q.updated_at DESC, q.slug`, tenant.ID)
	if err != nil {
		return Dashboard{}, fmt.Errorf("list dashboard queries: %w", err)
	}
	for rows.Next() {
		var query QueryRevision
		var types []byte
		if err := rows.Scan(&query.ID, &query.TenantID, &query.QueryID, &query.Slug, &query.Version,
			&query.SQL, &types, &query.CreatedAt); err != nil {
			rows.Close()
			return Dashboard{}, fmt.Errorf("scan dashboard query: %w", err)
		}
		if err := json.Unmarshal(types, &query.ParameterTypes); err != nil {
			rows.Close()
			return Dashboard{}, fmt.Errorf("decode dashboard query parameters: %w", err)
		}
		dashboard.Queries = append(dashboard.Queries, query)
	}
	if err := rows.Close(); err != nil {
		return Dashboard{}, fmt.Errorf("close dashboard queries: %w", err)
	}
	if err := rows.Err(); err != nil {
		return Dashboard{}, fmt.Errorf("list dashboard queries: %w", err)
	}

	rows, err = database.QueryContext(ctx, `SELECT a.id, a.slug, a.current_deployment_id,
		d.version, d.status, a.created_at, a.updated_at
		FROM apps a
		LEFT JOIN deployments d ON (d.tenant_id, d.id) = (a.tenant_id, a.current_deployment_id)
		WHERE a.tenant_id = $1 ORDER BY a.updated_at DESC, a.slug`, tenant.ID)
	if err != nil {
		return Dashboard{}, fmt.Errorf("list dashboard apps: %w", err)
	}
	for rows.Next() {
		var app AppSummary
		if err := rows.Scan(&app.ID, &app.Slug, &app.CurrentDeploymentID, &app.CurrentVersion,
			&app.CurrentStatus, &app.CreatedAt, &app.UpdatedAt); err != nil {
			rows.Close()
			return Dashboard{}, fmt.Errorf("scan dashboard app: %w", err)
		}
		dashboard.Apps = append(dashboard.Apps, app)
	}
	if err := rows.Close(); err != nil {
		return Dashboard{}, fmt.Errorf("close dashboard apps: %w", err)
	}
	if err := rows.Err(); err != nil {
		return Dashboard{}, fmt.Errorf("list dashboard apps: %w", err)
	}

	rows, err = database.QueryContext(ctx, `SELECT d.id, d.tenant_id, d.app_id, a.slug,
		d.previous_deployment_id, d.version, d.status, d.object_key, d.checksum, d.byte_count,
		d.manifest_json, d.error, d.created_at, d.published_at
		FROM deployments d
		JOIN apps a ON (a.tenant_id, a.id) = (d.tenant_id, d.app_id)
		WHERE d.tenant_id = $1 ORDER BY d.created_at DESC LIMIT 50`, tenant.ID)
	if err != nil {
		return Dashboard{}, fmt.Errorf("list dashboard deployments: %w", err)
	}
	for rows.Next() {
		var deployment Deployment
		if err := rows.Scan(deploymentScan(&deployment)...); err != nil {
			rows.Close()
			return Dashboard{}, fmt.Errorf("scan dashboard deployment: %w", err)
		}
		dashboard.Deployments = append(dashboard.Deployments, deployment)
	}
	if err := rows.Close(); err != nil {
		return Dashboard{}, fmt.Errorf("close dashboard deployments: %w", err)
	}
	if err := rows.Err(); err != nil {
		return Dashboard{}, fmt.Errorf("list dashboard deployments: %w", err)
	}

	rows, err = database.QueryContext(ctx, `SELECT r.id, r.dataset_id, d.slug, r.status, r.format,
		r.snapshot_id, r.row_count, r.byte_count, r.error, r.created_at, r.started_at, r.finished_at
		FROM sync_runs r
		JOIN datasets d ON (d.tenant_id, d.id) = (r.tenant_id, r.dataset_id)
		WHERE r.tenant_id = $1 ORDER BY r.created_at DESC LIMIT 50`, tenant.ID)
	if err != nil {
		return Dashboard{}, fmt.Errorf("list dashboard syncs: %w", err)
	}
	for rows.Next() {
		var sync DatasetSync
		if err := rows.Scan(&sync.ID, &sync.DatasetID, &sync.DatasetSlug, &sync.Status, &sync.Format,
			&sync.SnapshotID, &sync.RowCount, &sync.ByteCount, &sync.Error, &sync.CreatedAt,
			&sync.StartedAt, &sync.FinishedAt); err != nil {
			rows.Close()
			return Dashboard{}, fmt.Errorf("scan dashboard sync: %w", err)
		}
		dashboard.Syncs = append(dashboard.Syncs, sync)
	}
	if err := rows.Close(); err != nil {
		return Dashboard{}, fmt.Errorf("close dashboard syncs: %w", err)
	}
	if err := rows.Err(); err != nil {
		return Dashboard{}, fmt.Errorf("list dashboard syncs: %w", err)
	}

	rows, err = database.QueryContext(ctx, `SELECT id, action, resource_type, resource_id, request_id, created_at
		FROM audit_events WHERE tenant_id = $1 ORDER BY created_at DESC LIMIT 50`, tenant.ID)
	if err != nil {
		return Dashboard{}, fmt.Errorf("list dashboard activity: %w", err)
	}
	for rows.Next() {
		var activity ActivitySummary
		if err := rows.Scan(&activity.ID, &activity.Action, &activity.ResourceType, &activity.ResourceID,
			&activity.RequestID, &activity.CreatedAt); err != nil {
			rows.Close()
			return Dashboard{}, fmt.Errorf("scan dashboard activity: %w", err)
		}
		dashboard.Activity = append(dashboard.Activity, activity)
	}
	if err := rows.Close(); err != nil {
		return Dashboard{}, fmt.Errorf("close dashboard activity: %w", err)
	}
	if err := rows.Err(); err != nil {
		return Dashboard{}, fmt.Errorf("list dashboard activity: %w", err)
	}

	return dashboard, nil
}
