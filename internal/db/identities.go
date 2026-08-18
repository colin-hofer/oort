package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type User struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	DisplayName *string `json:"display_name,omitempty"`
}

type Principal struct {
	User
	Kind     string   `json:"kind"`
	TokenID  *string  `json:"token_id,omitempty"`
	TenantID *string  `json:"tenant_id,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
}

type OIDCAttempt struct {
	Nonce               string
	CodeVerifier        string
	CLIReturnURL        *string
	InvitationID        *string
	InvitationTokenHash []byte
}

type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	TenantID   *string    `json:"tenant_id,omitempty"`
	UserID     string     `json:"user_id"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  time.Time  `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

var allScopes = []string{
	"tenants:read", "tenants:write", "dashboard:read", "datasets:read", "datasets:write",
	"queries:read", "queries:write", "queries:run", "apps:read", "apps:write",
	"jobs:read", "jobs:write", "connectors:read", "connectors:write", "members:read", "members:write", "tokens:read", "tokens:write",
}

func FullPrincipal(user User) Principal {
	return Principal{User: user, Kind: "session", Scopes: append([]string(nil), allScopes...)}
}

func CreateLocalIdentity(ctx context.Context, database *sql.DB, email string, lifetime time.Duration) (User, string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || lifetime <= 0 {
		return User{}, "", fmt.Errorf("email and positive token lifetime are required")
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return User{}, "", fmt.Errorf("start identity transaction: %w", err)
	}
	defer tx.Rollback()
	user := User{ID: newID(), Email: email}
	if err := tx.QueryRowContext(ctx, `INSERT INTO users (id, email) VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id, email, display_name`, user.ID, user.Email).Scan(&user.ID, &user.Email, &user.DisplayName); err != nil {
		return User{}, "", fmt.Errorf("create identity: %w", err)
	}
	token, _, err := createAPIToken(ctx, tx, user, nil, "local", allScopes, time.Now().UTC().Add(lifetime), nil)
	if err != nil {
		return User{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return User{}, "", fmt.Errorf("commit identity: %w", err)
	}
	return user, token, nil
}

func AuthenticatePrincipal(ctx context.Context, database *sql.DB, token, scope string) (Principal, error) {
	var principal Principal
	var scopesJSON string
	err := database.QueryRowContext(ctx, `SELECT u.id, u.email, u.display_name, t.id, t.tenant_id, to_json(t.scopes)::text
		FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1 AND t.expires_at > now() AND t.revoked_at IS NULL AND $2 = ANY(t.scopes)`, secretHash(token), scope).
		Scan(&principal.ID, &principal.Email, &principal.DisplayName, &principal.TokenID, &principal.TenantID, &scopesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, sql.ErrNoRows
	}
	if err != nil {
		return Principal{}, fmt.Errorf("authenticate token: %w", err)
	}
	if err := json.Unmarshal([]byte(scopesJSON), &principal.Scopes); err != nil {
		return Principal{}, fmt.Errorf("decode token scopes: %w", err)
	}
	principal.Kind = "token"
	_, _ = database.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = now() WHERE id = $1`, *principal.TokenID)
	return principal, nil
}

func CreateControlSession(ctx context.Context, database *sql.DB, principal Principal, lifetime time.Duration) (string, error) {
	if lifetime <= 0 {
		return "", fmt.Errorf("positive session lifetime is required")
	}
	token, hash := newSecret()
	_, err := database.ExecContext(ctx, `INSERT INTO control_sessions (token_hash, user_id, tenant_id, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, hash, principal.ID, principal.TenantID, principal.Scopes, time.Now().UTC().Add(lifetime))
	if err != nil {
		return "", fmt.Errorf("create control session: %w", err)
	}
	return token, nil
}

func AuthenticateControlSession(ctx context.Context, database *sql.DB, token, scope string) (Principal, error) {
	var principal Principal
	var scopesJSON string
	err := database.QueryRowContext(ctx, `SELECT u.id, u.email, u.display_name, s.tenant_id, to_json(s.scopes)::text
		FROM control_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now() AND $2 = ANY(s.scopes)`, secretHash(token), scope).
		Scan(&principal.ID, &principal.Email, &principal.DisplayName, &principal.TenantID, &scopesJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, sql.ErrNoRows
	}
	if err != nil {
		return Principal{}, fmt.Errorf("authenticate control session: %w", err)
	}
	if err := json.Unmarshal([]byte(scopesJSON), &principal.Scopes); err != nil {
		return Principal{}, fmt.Errorf("decode control session scopes: %w", err)
	}
	principal.Kind = "session"
	return principal, nil
}

func DeleteControlSession(ctx context.Context, database *sql.DB, token string) error {
	_, err := database.ExecContext(ctx, `DELETE FROM control_sessions WHERE token_hash = $1`, secretHash(token))
	return err
}

func CreateOIDCAttempt(ctx context.Context, database *sql.DB, nonce, verifier string, cliReturnURL *string, lifetime time.Duration) (string, error) {
	state, hash := newSecret()
	_, err := database.ExecContext(ctx, `INSERT INTO oidc_auth_attempts
		(state_hash, nonce, code_verifier, cli_return_url, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, hash, nonce, verifier, cliReturnURL, time.Now().UTC().Add(lifetime))
	if err != nil {
		return "", fmt.Errorf("create OIDC attempt: %w", err)
	}
	return state, nil
}

func ConsumeOIDCAttempt(ctx context.Context, database *sql.DB, state string) (OIDCAttempt, error) {
	var attempt OIDCAttempt
	err := database.QueryRowContext(ctx, `DELETE FROM oidc_auth_attempts
		WHERE state_hash = $1 AND expires_at > now()
		RETURNING nonce, code_verifier, cli_return_url, invitation_id, invitation_token_hash`, secretHash(state)).
		Scan(&attempt.Nonce, &attempt.CodeVerifier, &attempt.CLIReturnURL, &attempt.InvitationID, &attempt.InvitationTokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return OIDCAttempt{}, sql.ErrNoRows
	}
	if err != nil {
		return OIDCAttempt{}, fmt.Errorf("consume OIDC attempt: %w", err)
	}
	return attempt, nil
}

func UpsertOIDCUser(ctx context.Context, database *sql.DB, issuer, subject, email string, displayName *string) (User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if issuer == "" || subject == "" || email == "" {
		return User{}, fmt.Errorf("issuer, subject, and verified email are required")
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var user User
	err = tx.QueryRowContext(ctx, `SELECT u.id, u.email, u.display_name
		FROM user_identities i JOIN users u ON u.id = i.user_id
		WHERE i.issuer = $1 AND i.subject = $2 FOR UPDATE`, issuer, subject).
		Scan(&user.ID, &user.Email, &user.DisplayName)
	if err == nil {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET email = $1, display_name = $2 WHERE id = $3`, email, displayName, user.ID); err != nil {
			if sqlState(err) == "23505" {
				return User{}, ErrConflict
			}
			return User{}, fmt.Errorf("update OIDC user: %w", err)
		}
		user.Email, user.DisplayName = email, displayName
	} else if errors.Is(err, sql.ErrNoRows) {
		user = User{ID: newID(), Email: email, DisplayName: displayName}
		if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, email, display_name) VALUES ($1, $2, $3)`, user.ID, email, displayName); err != nil {
			if sqlState(err) == "23505" {
				return User{}, ErrConflict
			}
			return User{}, fmt.Errorf("create OIDC user: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_identities (issuer, subject, user_id) VALUES ($1, $2, $3)`, issuer, subject, user.ID); err != nil {
			return User{}, fmt.Errorf("create OIDC identity: %w", err)
		}
	} else {
		return User{}, fmt.Errorf("look up OIDC identity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return user, nil
}

func CreateCLILoginCode(ctx context.Context, database *sql.DB, user User, lifetime time.Duration) (string, error) {
	code, hash := newSecret()
	_, err := database.ExecContext(ctx, `INSERT INTO cli_login_codes (code_hash, user_id, expires_at)
		VALUES ($1, $2, $3)`, hash, user.ID, time.Now().UTC().Add(lifetime))
	if err != nil {
		return "", fmt.Errorf("create CLI login code: %w", err)
	}
	return code, nil
}

func ExchangeCLILoginCode(ctx context.Context, database *sql.DB, code string, lifetime time.Duration) (User, string, APIToken, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return User{}, "", APIToken{}, err
	}
	defer tx.Rollback()
	var user User
	err = tx.QueryRowContext(ctx, `DELETE FROM cli_login_codes c USING users u
		WHERE c.code_hash = $1 AND c.expires_at > now() AND u.id = c.user_id
		RETURNING u.id, u.email, u.display_name`, secretHash(code)).Scan(&user.ID, &user.Email, &user.DisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", APIToken{}, sql.ErrNoRows
	}
	if err != nil {
		return User{}, "", APIToken{}, fmt.Errorf("consume CLI login code: %w", err)
	}
	secret, token, err := createAPIToken(ctx, tx, user, nil, "cli", allScopes, time.Now().UTC().Add(lifetime), &user.ID)
	if err != nil {
		return User{}, "", APIToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, "", APIToken{}, err
	}
	return user, secret, token, nil
}

func CreateAPIToken(ctx context.Context, database *sql.DB, actor User, tenantID *string, name string, scopes []string, expiresAt time.Time, requestID string) (string, APIToken, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return "", APIToken{}, err
	}
	defer tx.Rollback()
	secret, token, err := createAPIToken(ctx, tx, actor, tenantID, name, scopes, expiresAt, &actor.ID)
	if err != nil {
		return "", APIToken{}, err
	}
	if tenantID != nil {
		if err := audit(ctx, tx, *tenantID, actor.ID, "token.created", "api_token", token.ID, requestID); err != nil {
			return "", APIToken{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", APIToken{}, err
	}
	return secret, token, nil
}

func createAPIToken(ctx context.Context, tx *sql.Tx, user User, tenantID *string, name string, scopes []string, expiresAt time.Time, createdBy *string) (string, APIToken, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 || len(scopes) == 0 || !expiresAt.After(time.Now()) {
		return "", APIToken{}, fmt.Errorf("valid token name, scopes, and future expiry are required")
	}
	allowed := map[string]bool{}
	for _, scope := range allScopes {
		allowed[scope] = true
	}
	for _, scope := range scopes {
		if !allowed[scope] {
			return "", APIToken{}, fmt.Errorf("unknown token scope %q", scope)
		}
	}
	secret, hash := newSecret()
	token := APIToken{ID: newID(), Name: name, TenantID: tenantID, UserID: user.ID, Scopes: scopes, ExpiresAt: expiresAt.UTC(), CreatedAt: time.Now().UTC()}
	_, err := tx.ExecContext(ctx, `INSERT INTO api_tokens
		(id, tenant_id, user_id, token_hash, scopes, expires_at, name, created_by_user_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, token.ID, tenantID, user.ID, hash, scopes,
		token.ExpiresAt, name, createdBy, token.CreatedAt)
	if err != nil {
		return "", APIToken{}, fmt.Errorf("create API token: %w", err)
	}
	return secret, token, nil
}

func ListAPITokens(ctx context.Context, database *sql.DB, actor User, tenantID string) ([]APIToken, error) {
	rows, err := database.QueryContext(ctx, `SELECT id, name, tenant_id, user_id, to_json(scopes)::text, expires_at, created_at, last_used_at, revoked_at
		FROM api_tokens WHERE user_id = $1 AND tenant_id = $2 ORDER BY created_at DESC`, actor.ID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list API tokens: %w", err)
	}
	defer rows.Close()
	result := []APIToken{}
	for rows.Next() {
		var token APIToken
		var scopesJSON string
		if err := rows.Scan(&token.ID, &token.Name, &token.TenantID, &token.UserID, &scopesJSON, &token.ExpiresAt, &token.CreatedAt, &token.LastUsedAt, &token.RevokedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(scopesJSON), &token.Scopes); err != nil {
			return nil, err
		}
		result = append(result, token)
	}
	return result, rows.Err()
}

func RevokeAPIToken(ctx context.Context, database *sql.DB, actor User, tenantID, tokenID, requestID string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = now()
		WHERE id = $1 AND tenant_id = $2 AND user_id = $3 AND revoked_at IS NULL`, tokenID, tenantID, actor.ID)
	if err != nil {
		return fmt.Errorf("revoke API token: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	if err := audit(ctx, tx, tenantID, actor.ID, "token.revoked", "api_token", tokenID, requestID); err != nil {
		return err
	}
	return tx.Commit()
}

func RevokeCurrentAPIToken(ctx context.Context, database *sql.DB, actor User, tokenID string) error {
	result, err := database.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, tokenID, actor.ID)
	if err != nil {
		return fmt.Errorf("revoke current API token: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
