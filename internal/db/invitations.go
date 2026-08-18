package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

const InvitationLifetime = 7 * 24 * time.Hour

var (
	ErrInvitationAccepted = errors.New("invitation already accepted")
	ErrInvitationExpired  = errors.New("invitation expired")
	ErrInvitationRevoked  = errors.New("invitation revoked")
	ErrEmailMismatch      = errors.New("invitation email mismatch")
)

type Invitation struct {
	ID              string     `json:"id"`
	TenantID        string     `json:"-"`
	TenantSlug      string     `json:"tenant"`
	Email           string     `json:"email"`
	Role            string     `json:"role"`
	InvitedByUserID *string    `json:"invited_by_user_id,omitempty"`
	ExpiresAt       time.Time  `json:"expires_at"`
	AcceptedAt      *time.Time `json:"accepted_at,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	Status          string     `json:"status"`
}

func NormalizeEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 320 {
		return "", fmt.Errorf("a valid email address is required")
	}
	return email, nil
}

func AddMemberOrInvite(ctx context.Context, database *sql.DB, tenant Tenant, actor User, email, role, requestID string) (*Member, Invitation, string, error) {
	if !validRole(role) || !canManageRole(tenant.Role, "", role) {
		return nil, Invitation{}, "", ErrConflict
	}
	email, err := NormalizeEmail(email)
	if err != nil {
		return nil, Invitation{}, "", err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, Invitation{}, "", err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT id FROM tenants WHERE id = $1 FOR UPDATE`, tenant.ID).Scan(new(string)); err != nil {
		return nil, Invitation{}, "", err
	}
	var member Member
	err = tx.QueryRowContext(ctx, `SELECT id, email, display_name FROM users WHERE email = $1`, email).
		Scan(&member.UserID, &member.Email, &member.DisplayName)
	if err == nil {
		member.Role, member.CreatedAt = role, time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `INSERT INTO memberships (tenant_id, user_id, role, created_at)
			VALUES ($1, $2, $3, $4)`, tenant.ID, member.UserID, role, member.CreatedAt); err != nil {
			if sqlState(err) == "23505" {
				return nil, Invitation{}, "", ErrConflict
			}
			return nil, Invitation{}, "", fmt.Errorf("add member: %w", err)
		}
		// A user may have signed in after being invited. Their direct addition wins.
		var revokedInvitationID string
		revokeErr := tx.QueryRowContext(ctx, `UPDATE membership_invitations SET revoked_at = now()
			WHERE tenant_id = $1 AND email = $2 AND accepted_at IS NULL AND revoked_at IS NULL
			RETURNING id`, tenant.ID, email).Scan(&revokedInvitationID)
		if revokeErr == nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM oidc_auth_attempts WHERE invitation_id = $1`, revokedInvitationID); err != nil {
				return nil, Invitation{}, "", err
			}
			if err := audit(ctx, tx, tenant.ID, actor.ID, "invitation.revoked", "membership_invitation", revokedInvitationID, requestID); err != nil {
				return nil, Invitation{}, "", err
			}
		} else if !errors.Is(revokeErr, sql.ErrNoRows) {
			return nil, Invitation{}, "", fmt.Errorf("revoke superseded invitation: %w", revokeErr)
		}
		if err := audit(ctx, tx, tenant.ID, actor.ID, "member.added", "user", member.UserID, requestID); err != nil {
			return nil, Invitation{}, "", err
		}
		if err := tx.Commit(); err != nil {
			return nil, Invitation{}, "", err
		}
		return &member, Invitation{}, "", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, Invitation{}, "", fmt.Errorf("find member identity: %w", err)
	}

	secret, hash := newSecret()
	now := time.Now().UTC()
	invitation := Invitation{ID: newID(), TenantID: tenant.ID, TenantSlug: tenant.Slug, Email: email, Role: role,
		InvitedByUserID: &actor.ID, ExpiresAt: now.Add(InvitationLifetime), CreatedAt: now, Status: "pending"}
	var currentID string
	var currentExpires time.Time
	var acceptedAt, revokedAt *time.Time
	err = tx.QueryRowContext(ctx, `SELECT id, expires_at, accepted_at, revoked_at FROM membership_invitations
		WHERE tenant_id = $1 AND email = $2 FOR UPDATE`, tenant.ID, email).
		Scan(&currentID, &currentExpires, &acceptedAt, &revokedAt)
	if err == nil {
		if acceptedAt != nil || (revokedAt == nil && currentExpires.After(now)) {
			return nil, Invitation{}, "", ErrConflict
		}
		invitation.ID = currentID
		_, err = tx.ExecContext(ctx, `UPDATE membership_invitations SET role = $1, token_hash = $2,
			invited_by_user_id = $3, expires_at = $4, accepted_at = NULL, revoked_at = NULL, created_at = $5
			WHERE id = $6`, role, hash, actor.ID, invitation.ExpiresAt, now, invitation.ID)
	} else if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO membership_invitations
			(id, tenant_id, email, role, token_hash, invited_by_user_id, expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, invitation.ID, tenant.ID, email, role, hash,
			actor.ID, invitation.ExpiresAt, invitation.CreatedAt)
	}
	if err != nil {
		return nil, Invitation{}, "", fmt.Errorf("create invitation: %w", err)
	}
	if err := audit(ctx, tx, tenant.ID, actor.ID, "invitation.created", "membership_invitation", invitation.ID, requestID); err != nil {
		return nil, Invitation{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, Invitation{}, "", err
	}
	return nil, invitation, secret, nil
}

func ListInvitations(ctx context.Context, database *sql.DB, tenantID string) ([]Invitation, error) {
	rows, err := database.QueryContext(ctx, `SELECT i.id, i.tenant_id, t.slug, i.email, i.role,
		i.invited_by_user_id, i.expires_at, i.accepted_at, i.revoked_at, i.created_at,
		CASE WHEN i.expires_at > now() THEN 'pending' ELSE 'expired' END
		FROM membership_invitations i JOIN tenants t ON t.id = i.tenant_id
		WHERE i.tenant_id = $1 AND i.accepted_at IS NULL AND i.revoked_at IS NULL
		ORDER BY i.expires_at DESC, i.email`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	defer rows.Close()
	result := []Invitation{}
	for rows.Next() {
		var invitation Invitation
		if err := rows.Scan(invitationScan(&invitation)...); err != nil {
			return nil, fmt.Errorf("scan invitation: %w", err)
		}
		result = append(result, invitation)
	}
	return result, rows.Err()
}

func GetInvitationByToken(ctx context.Context, database *sql.DB, token string) (Invitation, error) {
	var invitation Invitation
	err := database.QueryRowContext(ctx, `SELECT i.id, i.tenant_id, t.slug, i.email, i.role,
		i.invited_by_user_id, i.expires_at, i.accepted_at, i.revoked_at, i.created_at,
		CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.revoked_at IS NOT NULL THEN 'revoked'
			WHEN i.expires_at <= now() THEN 'expired' ELSE 'pending' END
		FROM membership_invitations i JOIN tenants t ON t.id = i.tenant_id
		WHERE i.token_hash = $1`, secretHash(token)).Scan(invitationScan(&invitation)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, sql.ErrNoRows
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("get invitation: %w", err)
	}
	return invitation, invitationStatusError(invitation)
}

func CreateInvitationOIDCAttempt(ctx context.Context, database *sql.DB, token, nonce, verifier string, lifetime time.Duration) (string, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	invitation, err := lockInvitationByToken(ctx, tx, token)
	if err != nil {
		return "", err
	}
	state, stateHash := newSecret()
	tokenHash := secretHash(token)
	if _, err := tx.ExecContext(ctx, `INSERT INTO oidc_auth_attempts
		(state_hash, nonce, code_verifier, invitation_id, invitation_token_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, stateHash, nonce, verifier, invitation.ID, tokenHash,
		time.Now().UTC().Add(lifetime)); err != nil {
		return "", fmt.Errorf("create invitation OIDC attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return state, nil
}

func RenewInvitation(ctx context.Context, database *sql.DB, tenant Tenant, actor User, invitationID, requestID string) (Invitation, string, error) {
	secret, hash := newSecret()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Invitation{}, "", err
	}
	defer tx.Rollback()
	invitation, err := lockInvitation(ctx, tx, tenant.ID, invitationID)
	if err != nil {
		return Invitation{}, "", err
	}
	if invitation.AcceptedAt != nil || invitation.RevokedAt != nil || !canManageRole(tenant.Role, invitation.Role, invitation.Role) {
		return Invitation{}, "", ErrConflict
	}
	invitation.ExpiresAt, invitation.Status = time.Now().UTC().Add(InvitationLifetime), "pending"
	if _, err := tx.ExecContext(ctx, `UPDATE membership_invitations SET token_hash = $1, expires_at = $2 WHERE id = $3`, hash, invitation.ExpiresAt, invitation.ID); err != nil {
		return Invitation{}, "", fmt.Errorf("renew invitation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oidc_auth_attempts WHERE invitation_id = $1`, invitation.ID); err != nil {
		return Invitation{}, "", err
	}
	if err := audit(ctx, tx, tenant.ID, actor.ID, "invitation.renewed", "membership_invitation", invitation.ID, requestID); err != nil {
		return Invitation{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Invitation{}, "", err
	}
	return invitation, secret, nil
}

func RevokeInvitation(ctx context.Context, database *sql.DB, tenant Tenant, actor User, invitationID, requestID string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	invitation, err := lockInvitation(ctx, tx, tenant.ID, invitationID)
	if err != nil {
		return err
	}
	if invitation.AcceptedAt != nil || invitation.RevokedAt != nil || !canManageRole(tenant.Role, invitation.Role, "") {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE membership_invitations SET revoked_at = now() WHERE id = $1`, invitation.ID); err != nil {
		return fmt.Errorf("revoke invitation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oidc_auth_attempts WHERE invitation_id = $1`, invitation.ID); err != nil {
		return err
	}
	if err := audit(ctx, tx, tenant.ID, actor.ID, "invitation.revoked", "membership_invitation", invitation.ID, requestID); err != nil {
		return err
	}
	return tx.Commit()
}

func AcceptLocalInvitation(ctx context.Context, database *sql.DB, token, requestID string) (User, Tenant, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return User{}, Tenant{}, err
	}
	defer tx.Rollback()
	invitation, err := lockInvitationByToken(ctx, tx, token)
	if err != nil {
		return User{}, Tenant{}, err
	}
	user := User{ID: newID(), Email: invitation.Email}
	if err := tx.QueryRowContext(ctx, `INSERT INTO users (id, email) VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id, email, display_name`, user.ID, user.Email).Scan(&user.ID, &user.Email, &user.DisplayName); err != nil {
		return User{}, Tenant{}, fmt.Errorf("create invited identity: %w", err)
	}
	tenant, err := acceptInvitation(ctx, tx, invitation, user, requestID)
	if err != nil {
		return User{}, Tenant{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, Tenant{}, err
	}
	return user, tenant, nil
}

func AcceptOIDCInvitation(ctx context.Context, database *sql.DB, invitationID string, invitationTokenHash []byte, issuer, subject, email string, displayName *string, requestID string) (User, Tenant, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return User{}, Tenant{}, err
	}
	defer tx.Rollback()
	invitation, err := lockInvitationWithHash(ctx, tx, invitationID, invitationTokenHash)
	if err != nil {
		return User{}, Tenant{}, err
	}
	normalized, err := NormalizeEmail(email)
	if err != nil || normalized != invitation.Email {
		return User{}, Tenant{}, ErrEmailMismatch
	}
	if issuer == "" || subject == "" {
		return User{}, Tenant{}, fmt.Errorf("issuer and subject are required")
	}
	var user User
	err = tx.QueryRowContext(ctx, `SELECT u.id, u.email, u.display_name
		FROM user_identities i JOIN users u ON u.id = i.user_id
		WHERE i.issuer = $1 AND i.subject = $2 FOR UPDATE`, issuer, subject).
		Scan(&user.ID, &user.Email, &user.DisplayName)
	if err == nil {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET email = $1, display_name = $2 WHERE id = $3`, normalized, displayName, user.ID); err != nil {
			if sqlState(err) == "23505" {
				return User{}, Tenant{}, ErrConflict
			}
			return User{}, Tenant{}, fmt.Errorf("update OIDC user: %w", err)
		}
		user.Email, user.DisplayName = normalized, displayName
	} else if errors.Is(err, sql.ErrNoRows) {
		user = User{ID: newID(), Email: normalized, DisplayName: displayName}
		if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, email, display_name) VALUES ($1, $2, $3)`, user.ID, normalized, displayName); err != nil {
			if sqlState(err) == "23505" {
				return User{}, Tenant{}, ErrConflict
			}
			return User{}, Tenant{}, fmt.Errorf("create OIDC user: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_identities (issuer, subject, user_id) VALUES ($1, $2, $3)`, issuer, subject, user.ID); err != nil {
			return User{}, Tenant{}, fmt.Errorf("create OIDC identity: %w", err)
		}
	} else {
		return User{}, Tenant{}, fmt.Errorf("look up OIDC identity: %w", err)
	}
	tenant, err := acceptInvitation(ctx, tx, invitation, user, requestID)
	if err != nil {
		return User{}, Tenant{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, Tenant{}, err
	}
	return user, tenant, nil
}

func lockInvitationWithHash(ctx context.Context, tx *sql.Tx, invitationID string, tokenHash []byte) (Invitation, error) {
	if len(tokenHash) != 32 {
		return Invitation{}, sql.ErrNoRows
	}
	var invitation Invitation
	err := tx.QueryRowContext(ctx, `SELECT i.id, i.tenant_id, t.slug, i.email, i.role,
		i.invited_by_user_id, i.expires_at, i.accepted_at, i.revoked_at, i.created_at,
		CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.revoked_at IS NOT NULL THEN 'revoked'
			WHEN i.expires_at <= now() THEN 'expired' ELSE 'pending' END
		FROM membership_invitations i JOIN tenants t ON t.id = i.tenant_id
		WHERE i.id = $1 AND i.token_hash = $2 FOR UPDATE`, invitationID, tokenHash).Scan(invitationScan(&invitation)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, sql.ErrNoRows
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("lock invitation version: %w", err)
	}
	return invitation, nil
}

func acceptInvitation(ctx context.Context, tx *sql.Tx, invitation Invitation, user User, requestID string) (Tenant, error) {
	if err := invitationStatusError(invitation); err != nil {
		return Tenant{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, $3)`, invitation.TenantID, user.ID, invitation.Role); err != nil {
		if sqlState(err) == "23505" {
			return Tenant{}, ErrConflict
		}
		return Tenant{}, fmt.Errorf("accept invitation membership: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE membership_invitations SET accepted_at = now() WHERE id = $1`, invitation.ID); err != nil {
		return Tenant{}, fmt.Errorf("mark invitation accepted: %w", err)
	}
	if err := audit(ctx, tx, invitation.TenantID, user.ID, "invitation.accepted", "membership_invitation", invitation.ID, requestID); err != nil {
		return Tenant{}, err
	}
	return Tenant{ID: invitation.TenantID, Slug: invitation.TenantSlug, Role: invitation.Role}, nil
}

func lockInvitation(ctx context.Context, tx *sql.Tx, tenantID, invitationID string) (Invitation, error) {
	var invitation Invitation
	err := tx.QueryRowContext(ctx, `SELECT i.id, i.tenant_id, t.slug, i.email, i.role,
		i.invited_by_user_id, i.expires_at, i.accepted_at, i.revoked_at, i.created_at,
		CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.revoked_at IS NOT NULL THEN 'revoked'
			WHEN i.expires_at <= now() THEN 'expired' ELSE 'pending' END
		FROM membership_invitations i JOIN tenants t ON t.id = i.tenant_id
		WHERE i.id = $1 AND ($2::text = '' OR i.tenant_id::text = $2) FOR UPDATE`, invitationID, tenantID).
		Scan(invitationScan(&invitation)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, sql.ErrNoRows
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("lock invitation: %w", err)
	}
	return invitation, nil
}

func lockInvitationByToken(ctx context.Context, tx *sql.Tx, token string) (Invitation, error) {
	var invitation Invitation
	err := tx.QueryRowContext(ctx, `SELECT i.id, i.tenant_id, t.slug, i.email, i.role,
		i.invited_by_user_id, i.expires_at, i.accepted_at, i.revoked_at, i.created_at,
		CASE WHEN i.accepted_at IS NOT NULL THEN 'accepted' WHEN i.revoked_at IS NOT NULL THEN 'revoked'
			WHEN i.expires_at <= now() THEN 'expired' ELSE 'pending' END
		FROM membership_invitations i JOIN tenants t ON t.id = i.tenant_id
		WHERE i.token_hash = $1 FOR UPDATE`, secretHash(token)).Scan(invitationScan(&invitation)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Invitation{}, sql.ErrNoRows
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("lock invitation: %w", err)
	}
	return invitation, invitationStatusError(invitation)
}

func invitationScan(invitation *Invitation) []any {
	return []any{&invitation.ID, &invitation.TenantID, &invitation.TenantSlug, &invitation.Email, &invitation.Role,
		&invitation.InvitedByUserID, &invitation.ExpiresAt, &invitation.AcceptedAt, &invitation.RevokedAt,
		&invitation.CreatedAt, &invitation.Status}
}

func invitationStatusError(invitation Invitation) error {
	switch invitation.Status {
	case "accepted":
		return ErrInvitationAccepted
	case "revoked":
		return ErrInvitationRevoked
	case "expired":
		return ErrInvitationExpired
	default:
		return nil
	}
}
