package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var ErrConflict = errors.New("conflict")

type User struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

type Tenant struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func Open(ctx context.Context, databaseURL string) (*sql.DB, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, fmt.Errorf("database URL is required")
	}
	database, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	database.SetMaxOpenConns(10)
	database.SetMaxIdleConns(5)
	database.SetConnMaxIdleTime(5 * time.Minute)
	if err := database.PingContext(ctx); err != nil {
		database.Close()
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	return database, nil
}

func CreateLocalIdentity(ctx context.Context, database *sql.DB, email string, lifetime time.Duration) (User, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || lifetime <= 0 {
		return User{}, "", fmt.Errorf("email and positive token lifetime are required")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return User{}, "", fmt.Errorf("generate API token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return User{}, "", fmt.Errorf("start identity transaction: %w", err)
	}
	defer tx.Rollback()
	user := User{ID: newID(), Email: email}
	if err := tx.QueryRowContext(ctx, `INSERT INTO users (id, email) VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id, email`, user.ID, user.Email).Scan(&user.ID, &user.Email); err != nil {
		return User{}, "", fmt.Errorf("create identity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM api_tokens WHERE user_id = $1 AND tenant_id IS NULL", user.ID); err != nil {
		return User{}, "", fmt.Errorf("revoke prior identity token: %w", err)
	}
	hash := sha256.Sum256([]byte(token))
	if _, err := tx.ExecContext(ctx, `INSERT INTO api_tokens
		(id, user_id, token_hash, scopes, expires_at)
		VALUES ($1, $2, $3, ARRAY[
			'tenants:read', 'tenants:write', 'datasets:read', 'datasets:write',
			'queries:write', 'queries:run'
		], $4)`,
		newID(), user.ID, hash[:], time.Now().UTC().Add(lifetime)); err != nil {
		return User{}, "", fmt.Errorf("create API token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, "", fmt.Errorf("commit identity: %w", err)
	}
	return user, token, nil
}

func Authenticate(ctx context.Context, database *sql.DB, token, scope string) (User, error) {
	hash := sha256.Sum256([]byte(token))
	var user User
	err := database.QueryRowContext(ctx, `SELECT u.id, u.email
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1 AND t.expires_at > now() AND $2 = ANY(t.scopes)`, hash[:], scope).Scan(&user.ID, &user.Email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, sql.ErrNoRows
		}
		return User{}, fmt.Errorf("authenticate token: %w", err)
	}
	return user, nil
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

func newID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}

func sqlState(err error) string {
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		return state.SQLState()
	}
	return ""
}
