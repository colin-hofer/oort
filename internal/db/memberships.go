package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Member struct {
	UserID      string    `json:"user_id"`
	Email       string    `json:"email"`
	DisplayName *string   `json:"display_name,omitempty"`
	Role        string    `json:"role"`
	CreatedAt   time.Time `json:"created_at"`
}

func ListMembers(ctx context.Context, database *sql.DB, tenantID string) ([]Member, error) {
	rows, err := database.QueryContext(ctx, `SELECT u.id, u.email, u.display_name, m.role, m.created_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.tenant_id = $1 ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 WHEN 'developer' THEN 2 ELSE 3 END, u.email`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	defer rows.Close()
	members := []Member{}
	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.UserID, &member.Email, &member.DisplayName, &member.Role, &member.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

func AddMember(ctx context.Context, database *sql.DB, tenant Tenant, actor User, email, role, requestID string) (Member, error) {
	if !validRole(role) || !canManageRole(tenant.Role, "", role) {
		return Member{}, ErrConflict
	}
	email = strings.ToLower(strings.TrimSpace(email))
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback()
	var member Member
	err = tx.QueryRowContext(ctx, `SELECT id, email, display_name FROM users WHERE email = $1`, email).
		Scan(&member.UserID, &member.Email, &member.DisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, sql.ErrNoRows
	}
	if err != nil {
		return Member{}, fmt.Errorf("find member identity: %w", err)
	}
	member.Role, member.CreatedAt = role, time.Now().UTC()
	_, err = tx.ExecContext(ctx, `INSERT INTO memberships (tenant_id, user_id, role, created_at)
		VALUES ($1, $2, $3, $4)`, tenant.ID, member.UserID, role, member.CreatedAt)
	if err != nil {
		if sqlState(err) == "23505" {
			return Member{}, ErrConflict
		}
		return Member{}, fmt.Errorf("add member: %w", err)
	}
	if err := audit(ctx, tx, tenant.ID, actor.ID, "member.added", "user", member.UserID, requestID); err != nil {
		return Member{}, err
	}
	if err := tx.Commit(); err != nil {
		return Member{}, err
	}
	return member, nil
}

func ChangeMemberRole(ctx context.Context, database *sql.DB, tenant Tenant, actor User, userID, role, requestID string) (Member, error) {
	if !validRole(role) {
		return Member{}, ErrConflict
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return Member{}, err
	}
	defer tx.Rollback()
	var member Member
	err = tx.QueryRowContext(ctx, `SELECT u.id, u.email, u.display_name, m.role, m.created_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.tenant_id = $1 AND m.user_id = $2 FOR UPDATE`, tenant.ID, userID).
		Scan(&member.UserID, &member.Email, &member.DisplayName, &member.Role, &member.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, sql.ErrNoRows
	}
	if err != nil {
		return Member{}, err
	}
	if !canManageRole(tenant.Role, member.Role, role) {
		return Member{}, ErrConflict
	}
	if member.Role == "owner" && role != "owner" {
		var owners int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'owner'`, tenant.ID).Scan(&owners); err != nil {
			return Member{}, err
		}
		if owners == 1 {
			return Member{}, ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memberships SET role = $1 WHERE tenant_id = $2 AND user_id = $3`, role, tenant.ID, userID); err != nil {
		return Member{}, err
	}
	member.Role = role
	if err := audit(ctx, tx, tenant.ID, actor.ID, "member.role_changed", "user", userID, requestID); err != nil {
		return Member{}, err
	}
	if err := tx.Commit(); err != nil {
		return Member{}, err
	}
	return member, nil
}

func RemoveMember(ctx context.Context, database *sql.DB, tenant Tenant, actor User, userID, requestID string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role string
	err = tx.QueryRowContext(ctx, `SELECT role FROM memberships WHERE tenant_id = $1 AND user_id = $2 FOR UPDATE`, tenant.ID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if !canManageRole(tenant.Role, role, "") {
		return ErrConflict
	}
	if role == "owner" {
		var owners int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memberships WHERE tenant_id = $1 AND role = 'owner'`, tenant.ID).Scan(&owners); err != nil {
			return err
		}
		if owners == 1 {
			return ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenant.ID, userID); err != nil {
		return err
	}
	if err := audit(ctx, tx, tenant.ID, actor.ID, "member.removed", "user", userID, requestID); err != nil {
		return err
	}
	return tx.Commit()
}

func validRole(role string) bool {
	return role == "owner" || role == "admin" || role == "developer" || role == "viewer"
}

func canManageRole(actorRole, currentRole, newRole string) bool {
	if actorRole == "owner" {
		return true
	}
	return actorRole == "admin" && currentRole != "owner" && newRole != "owner"
}

func audit(ctx context.Context, tx *sql.Tx, tenantID, actorID, action, resourceType, resourceID, requestID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_events
		(id, tenant_id, actor_user_id, action, resource_type, resource_id, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, newID(), tenantID, actorID, action, resourceType, resourceID, requestID)
	if err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}
