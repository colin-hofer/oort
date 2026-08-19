package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"
)

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

type PublishedQuery struct {
	Name           string
	SQL            string
	ParameterTypes map[string]string
}

func SaveQueryRevision(ctx context.Context, database *sql.DB, tenant Tenant, actor User, slug, sqlText string, parameterTypes map[string]string, requestID string) (QueryRevision, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return QueryRevision{}, fmt.Errorf("start query revision: %w", err)
	}
	defer tx.Rollback()
	revision, created, err := saveQueryRevision(ctx, tx, tenant.ID, slug, sqlText, parameterTypes)
	if err != nil {
		return QueryRevision{}, err
	}
	if !created {
		return revision, nil
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

func DeleteQuery(ctx context.Context, database *sql.DB, tenant Tenant, actor User, slug, requestID string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM queries WHERE tenant_id = $1 AND slug = $2 FOR UPDATE`, tenant.ID, slug).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	} else if err != nil {
		return err
	}
	var deployed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM query_revisions r JOIN deployment_queries q
		ON (q.tenant_id, q.query_revision_id) = (r.tenant_id, r.id)
		WHERE r.tenant_id = $1 AND r.query_id = $2
		UNION ALL
		SELECT 1 FROM deployments d
		JOIN jobs j ON (j.tenant_id, j.deployment_id) = (d.tenant_id, d.id)
		CROSS JOIN LATERAL jsonb_array_elements(d.manifest_json->'queries') published
		WHERE d.tenant_id = $1 AND j.status IN ('awaiting_upload', 'queued', 'running')
		AND published->>'name' = $3
	)`, tenant.ID, id, slug).Scan(&deployed); err != nil {
		return err
	}
	if deployed {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE queries SET current_revision_id = NULL WHERE tenant_id = $1 AND id = $2`, tenant.ID, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM queries WHERE tenant_id = $1 AND id = $2`, tenant.ID, id); err != nil {
		return err
	}
	if err := audit(ctx, tx, tenant.ID, actor.ID, "query.deleted", "query", id, requestID); err != nil {
		return err
	}
	return tx.Commit()
}

func savePublishedQuery(ctx context.Context, tx *sql.Tx, tenantID string, published PublishedQuery) (string, error) {
	revision, _, err := saveQueryRevision(ctx, tx, tenantID, published.Name, published.SQL, published.ParameterTypes)
	if err != nil {
		return "", err
	}
	return revision.ID, nil
}

func saveQueryRevision(ctx context.Context, tx *sql.Tx, tenantID, slug, sqlText string, parameterTypes map[string]string) (QueryRevision, bool, error) {
	typesJSON, err := json.Marshal(parameterTypes)
	if err != nil {
		return QueryRevision{}, false, fmt.Errorf("encode query %q parameter types: %w", slug, err)
	}
	queryID := newID()
	if err := tx.QueryRowContext(ctx, `INSERT INTO queries (id, tenant_id, slug) VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, slug) DO UPDATE SET slug = EXCLUDED.slug
		RETURNING id`, queryID, tenantID, slug).Scan(&queryID); err != nil {
		return QueryRevision{}, false, fmt.Errorf("save query %q: %w", slug, err)
	}

	var current QueryRevision
	var currentTypes []byte
	err = tx.QueryRowContext(ctx, `SELECT r.id, r.version, r.sql_text, r.parameter_types, r.created_at
		FROM queries q JOIN query_revisions r ON (r.tenant_id, r.id) = (q.tenant_id, q.current_revision_id)
		WHERE q.tenant_id = $1 AND q.id = $2`, tenantID, queryID).
		Scan(&current.ID, &current.Version, &current.SQL, &currentTypes, &current.CreatedAt)
	if err == nil && current.SQL == sqlText && equalJSON(currentTypes, typesJSON) {
		current.TenantID = tenantID
		current.QueryID = queryID
		current.Slug = slug
		current.ParameterTypes = parameterTypes
		return current, false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return QueryRevision{}, false, fmt.Errorf("read current query revision %q: %w", slug, err)
	}

	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM query_revisions
		WHERE tenant_id = $1 AND query_id = $2`, tenantID, queryID).Scan(&version); err != nil {
		return QueryRevision{}, false, fmt.Errorf("allocate query revision %q: %w", slug, err)
	}
	revision := QueryRevision{
		ID:             newID(),
		TenantID:       tenantID,
		QueryID:        queryID,
		Slug:           slug,
		Version:        version,
		SQL:            sqlText,
		ParameterTypes: parameterTypes,
		CreatedAt:      time.Now().UTC(),
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO query_revisions
		(id, tenant_id, query_id, version, sql_text, parameter_types, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, revision.ID, tenantID, queryID, version,
		sqlText, typesJSON, revision.CreatedAt); err != nil {
		return QueryRevision{}, false, fmt.Errorf("create query revision %q: %w", slug, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE queries SET current_revision_id = $1, updated_at = now()
		WHERE tenant_id = $2 AND id = $3`, revision.ID, tenantID, queryID); err != nil {
		return QueryRevision{}, false, fmt.Errorf("publish query revision %q: %w", slug, err)
	}
	return revision, true, nil
}

func equalJSON(left, right []byte) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && reflect.DeepEqual(a, b)
}
