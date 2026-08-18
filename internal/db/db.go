package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	ErrConflict  = errors.New("conflict")
	ErrLeaseLost = errors.New("job lease lost")
	ErrQuota     = errors.New("tenant storage quota exceeded")
)

const MaxTenantStoredBytes int64 = 10 << 30

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
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

func newID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}

func newSecret() (string, []byte) {
	secret := rand.Text()
	return secret, secretHash(secret)
}

func secretHash(secret string) []byte {
	hash := sha256.Sum256([]byte(secret))
	return hash[:]
}

func failureMessage(err error) string {
	message := err.Error()
	return message[:min(len(message), 2000)]
}

func sqlState(err error) string {
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		return state.SQLState()
	}
	return ""
}

func checkStorageQuota(ctx context.Context, tx *sql.Tx, tenantID string, additional int64, excludedRunID *string) error {
	var locked string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM tenants WHERE id = $1 FOR UPDATE`, tenantID).Scan(&locked); err != nil {
		return err
	}
	var stored int64
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT sum(byte_count) FROM sync_runs WHERE tenant_id = $1 AND ($2::uuid IS NULL OR id <> $2)), 0) +
		COALESCE((SELECT sum(byte_count) FROM deployments WHERE tenant_id = $1), 0)`, tenantID, excludedRunID).Scan(&stored); err != nil {
		return err
	}
	if additional < 0 || stored > MaxTenantStoredBytes-additional {
		return ErrQuota
	}
	return nil
}
