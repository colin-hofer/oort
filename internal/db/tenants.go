package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Tenant struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func CreateTenant(ctx context.Context, database *sql.DB, actor User, slug, requestID string) (Tenant, error) {
	tenant := Tenant{ID: newID(), Slug: slug, Role: "owner", CreatedAt: time.Now().UTC()}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Tenant{}, fmt.Errorf("start tenant transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO tenants (id, slug, created_at) VALUES ($1, $2, $3)", tenant.ID, tenant.Slug, tenant.CreatedAt); err != nil {
		if sqlState(err) == "23505" {
			return Tenant{}, ErrConflict
		}
		return Tenant{}, fmt.Errorf("create tenant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memberships (tenant_id, user_id, role)
		VALUES ($1, $2, 'owner')`, tenant.ID, actor.ID); err != nil {
		return Tenant{}, fmt.Errorf("create owner membership: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_events
		(id, tenant_id, actor_user_id, action, resource_type, resource_id, request_id)
		VALUES ($1, $2, $3, 'tenant.created', 'tenant', $2, $4)`, newID(), tenant.ID, actor.ID, requestID); err != nil {
		return Tenant{}, fmt.Errorf("audit tenant creation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Tenant{}, fmt.Errorf("commit tenant: %w", err)
	}
	return tenant, nil
}

func ListTenants(ctx context.Context, database *sql.DB, actor User) ([]Tenant, error) {
	rows, err := database.QueryContext(ctx, `SELECT t.id, t.slug, m.role, t.created_at
		FROM memberships m JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1 ORDER BY t.slug`, actor.ID)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	tenants := make([]Tenant, 0)
	for rows.Next() {
		var tenant Tenant
		if err := rows.Scan(&tenant.ID, &tenant.Slug, &tenant.Role, &tenant.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan tenant: %w", err)
		}
		tenant.CreatedAt = tenant.CreatedAt.UTC()
		tenants = append(tenants, tenant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	return tenants, nil
}

func ResolveTenant(ctx context.Context, database *sql.DB, actor User, slug string) (Tenant, error) {
	var tenant Tenant
	err := database.QueryRowContext(ctx, `SELECT t.id, t.slug, m.role, t.created_at
		FROM memberships m JOIN tenants t ON t.id = m.tenant_id
		WHERE m.user_id = $1 AND t.slug = $2`, actor.ID, slug).
		Scan(&tenant.ID, &tenant.Slug, &tenant.Role, &tenant.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, sql.ErrNoRows
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("resolve tenant: %w", err)
	}
	tenant.CreatedAt = tenant.CreatedAt.UTC()
	return tenant, nil
}
