package db

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

func TenantCatalog(databaseURL, secret, tenantID string) (catalogURL, role, name string, err error) {
	if secret == "" {
		return "", "", "", fmt.Errorf("catalog secret is required")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", "", "", fmt.Errorf("invalid PostgreSQL URL")
	}
	compactID := strings.ReplaceAll(tenantID, "-", "")
	if len(compactID) != 32 {
		return "", "", "", fmt.Errorf("invalid tenant ID")
	}
	name = "oortcat_" + compactID
	role = "oorttenant_" + compactID
	parsed.User = url.UserPassword(role, catalogPassword(secret, tenantID))
	parsed.Path = "/" + name
	return parsed.String(), role, name, nil
}

func EnsureTenantCatalog(ctx context.Context, database *sql.DB, databaseURL, secret, tenantID string) (string, error) {
	catalogURL, role, name, err := TenantCatalog(databaseURL, secret, tenantID)
	if err != nil {
		return "", err
	}
	var roleExists bool
	if err := database.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&roleExists); err != nil {
		return "", fmt.Errorf("check catalog role: %w", err)
	}
	roleSQL := quoteIdentifier(role)
	password := quoteLiteral(catalogPassword(secret, tenantID))
	if !roleExists {
		if _, err := database.ExecContext(ctx, "CREATE ROLE "+roleSQL+" LOGIN PASSWORD "+password); err != nil && sqlState(err) != "42710" {
			return "", fmt.Errorf("create catalog role: %w", err)
		}
	} else if _, err := database.ExecContext(ctx, "ALTER ROLE "+roleSQL+" PASSWORD "+password); err != nil {
		return "", fmt.Errorf("refresh catalog role: %w", err)
	}
	var databaseExists bool
	if err := database.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&databaseExists); err != nil {
		return "", fmt.Errorf("check catalog database: %w", err)
	}
	if !databaseExists {
		if _, err := database.ExecContext(ctx, "CREATE DATABASE "+quoteIdentifier(name)+" OWNER "+roleSQL); err != nil && sqlState(err) != "42P04" {
			return "", fmt.Errorf("create catalog database: %w", err)
		}
	}
	return catalogURL, nil
}

func catalogPassword(secret, tenantID string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(tenantID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
func quoteLiteral(value string) string    { return `'` + strings.ReplaceAll(value, `'`, `''`) + `'` }
