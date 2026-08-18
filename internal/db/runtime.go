package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type RuntimeAccess struct {
	TenantID     string
	TenantSlug   string
	AppID        string
	AppSlug      string
	UserID       string
	DeploymentID string
	ObjectKey    string
	ByteCount    int64
	Checksum     []byte
	Manifest     json.RawMessage
}

func CreateRuntimeLoginCode(ctx context.Context, database *sql.DB, tenant Tenant, actor User, appSlug string, lifetime time.Duration) (string, error) {
	code, hash := newSecret()
	result, err := database.ExecContext(ctx, `INSERT INTO runtime_login_codes
		(code_hash, tenant_id, app_id, user_id, expires_at)
		SELECT $1, a.tenant_id, a.id, $2, now() + $3::interval FROM apps a
		WHERE a.tenant_id = $4 AND a.slug = $5 AND a.current_deployment_id IS NOT NULL`,
		hash, actor.ID, lifetime.String(), tenant.ID, appSlug)
	if err != nil {
		return "", fmt.Errorf("create app login code: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return "", sql.ErrNoRows
	}
	return code, nil
}

func ExchangeRuntimeLoginCode(ctx context.Context, database *sql.DB, tenantSlug, appSlug, code string, lifetime time.Duration) (string, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("start app login exchange: %w", err)
	}
	defer tx.Rollback()
	var tenantID, appID, userID string
	err = tx.QueryRowContext(ctx, `DELETE FROM runtime_login_codes c USING apps a, tenants t
		WHERE c.code_hash = $1 AND c.expires_at > now() AND a.id = c.app_id AND a.tenant_id = c.tenant_id
		  AND t.id = c.tenant_id AND t.slug = $2 AND a.slug = $3
		RETURNING c.tenant_id, c.app_id, c.user_id`, secretHash(code), tenantSlug, appSlug).
		Scan(&tenantID, &appID, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", sql.ErrNoRows
	}
	if err != nil {
		return "", fmt.Errorf("exchange app login code: %w", err)
	}
	token, tokenHash := newSecret()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_sessions
		(token_hash, tenant_id, app_id, user_id, expires_at) VALUES ($1, $2, $3, $4, now() + $5::interval)`,
		tokenHash, tenantID, appID, userID, lifetime.String()); err != nil {
		return "", fmt.Errorf("create app session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit app login exchange: %w", err)
	}
	return token, nil
}

func RuntimeDeployment(ctx context.Context, database *sql.DB, tenantSlug, appSlug, token string) (RuntimeAccess, error) {
	var access RuntimeAccess
	err := database.QueryRowContext(ctx, `SELECT t.id, t.slug, a.id, a.slug, s.user_id, d.id,
		d.object_key, d.byte_count, d.checksum, d.manifest_json
		FROM runtime_sessions s
		JOIN tenants t ON t.id = s.tenant_id
		JOIN apps a ON (a.tenant_id, a.id) = (s.tenant_id, s.app_id)
		JOIN memberships m ON (m.tenant_id, m.user_id) = (s.tenant_id, s.user_id)
		JOIN deployments d ON (d.tenant_id, d.id) = (a.tenant_id, a.current_deployment_id)
		WHERE s.token_hash = $1 AND s.expires_at > now() AND t.slug = $2 AND a.slug = $3
		  AND d.status = 'succeeded'`, secretHash(token), tenantSlug, appSlug).Scan(&access.TenantID, &access.TenantSlug,
		&access.AppID, &access.AppSlug, &access.UserID, &access.DeploymentID, &access.ObjectKey,
		&access.ByteCount, &access.Checksum, &access.Manifest)
	if errors.Is(err, sql.ErrNoRows) {
		return RuntimeAccess{}, sql.ErrNoRows
	}
	if err != nil {
		return RuntimeAccess{}, fmt.Errorf("authenticate app session: %w", err)
	}
	return access, nil
}

func RuntimeQuery(ctx context.Context, database *sql.DB, access RuntimeAccess, name string) (QueryRevision, error) {
	var revision QueryRevision
	var typesJSON []byte
	err := database.QueryRowContext(ctx, `SELECT r.id, r.tenant_id, r.query_id, q.slug, r.version,
		r.sql_text, r.parameter_types, r.created_at
		FROM deployment_queries g
		JOIN query_revisions r ON (r.tenant_id, r.id) = (g.tenant_id, g.query_revision_id)
		JOIN queries q ON (q.tenant_id, q.id) = (r.tenant_id, r.query_id)
		WHERE g.tenant_id = $1 AND g.deployment_id = $2 AND q.slug = $3`,
		access.TenantID, access.DeploymentID, name).Scan(&revision.ID, &revision.TenantID,
		&revision.QueryID, &revision.Slug, &revision.Version, &revision.SQL, &typesJSON, &revision.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return QueryRevision{}, sql.ErrNoRows
	}
	if err != nil {
		return QueryRevision{}, fmt.Errorf("resolve deployment query: %w", err)
	}
	if err := json.Unmarshal(typesJSON, &revision.ParameterTypes); err != nil {
		return QueryRevision{}, fmt.Errorf("decode deployment query types: %w", err)
	}
	return revision, nil
}
