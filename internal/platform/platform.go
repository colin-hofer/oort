package platform

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nebulous/internal/db"
	"nebulous/internal/jobs"
	"nebulous/internal/secretbox"
	"nebulous/internal/server"
	"nebulous/internal/storage"
)

func Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing internal platform mode")
	}
	switch args[0] {
	case "server":
		flags := flag.NewFlagSet("server", flag.ContinueOnError)
		listen := flags.String("listen", env("NEB_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
		databaseURL := flags.String("database-url", env("NEB_DATABASE_URL", localDatabaseURL()), "PostgreSQL URL")
		localAuth := flags.Bool("local-auth", false, "create a loopback-only local identity")
		stateDir := flags.String("state-dir", defaultStateDir(), "local state directory")
		controlHost := flags.String("control-host", env("NEB_CONTROL_HOST", ""), "accepted control-plane host")
		appHostSuffix := flags.String("app-host-suffix", env("NEB_APP_HOST_SUFFIX", "apps.localhost"), "app runtime host suffix")
		appScheme := flags.String("app-scheme", env("NEB_APP_SCHEME", "http"), "app URL scheme")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("server takes no positional arguments")
		}
		secretKey := env("NEB_SECRET_KEY", "")
		if *localAuth && secretKey == "" {
			var err error
			secretKey, err = localSecretKey(*stateDir)
			if err != nil {
				return err
			}
		}
		return server.Run(ctx, server.Config{
			DatabaseURL:            *databaseURL,
			Listen:                 *listen,
			LocalAuth:              *localAuth,
			StateDir:               *stateDir,
			CatalogSecret:          env("NEB_CATALOG_SECRET", "nebulous-local-catalog-secret"),
			ExtensionDir:           extensionDir(*stateDir),
			Storage:                storageConfig(),
			QueryTimeout:           10 * time.Second,
			ControlHost:            *controlHost,
			AppHostSuffix:          *appHostSuffix,
			AppScheme:              *appScheme,
			SecureCookies:          *appScheme == "https",
			OIDCIssuer:             env("NEB_OIDC_ISSUER", ""),
			OIDCClientID:           env("NEB_OIDC_CLIENT_ID", ""),
			OIDCSecret:             env("NEB_OIDC_CLIENT_SECRET", ""),
			PublicURL:              env("NEB_PUBLIC_URL", ""),
			SecretKey:              secretKey,
			AllowPrivateConnectors: env("NEB_ALLOW_PRIVATE_CONNECTORS", "") == "true",
			Log:                    os.Stderr,
		})
	case "worker":
		flags := flag.NewFlagSet("worker", flag.ContinueOnError)
		databaseURL := flags.String("database-url", env("NEB_DATABASE_URL", localDatabaseURL()), "PostgreSQL URL")
		stateDir := flags.String("state-dir", defaultStateDir(), "local state directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("worker takes no positional arguments")
		}
		return jobs.Run(ctx, jobs.Config{
			DatabaseURL:            *databaseURL,
			CatalogSecret:          env("NEB_CATALOG_SECRET", "nebulous-local-catalog-secret"),
			ExtensionDir:           extensionDir(*stateDir),
			Storage:                storageConfig(),
			SecretKey:              env("NEB_SECRET_KEY", ""),
			AllowPrivateConnectors: env("NEB_ALLOW_PRIVATE_CONNECTORS", "") == "true",
			Log:                    os.Stderr,
		})
	case "local":
		flags := flag.NewFlagSet("local", flag.ContinueOnError)
		listen := flags.String("listen", env("NEB_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
		databaseURL := flags.String("database-url", env("NEB_DATABASE_URL", localDatabaseURL()), "PostgreSQL URL")
		stateDir := flags.String("state-dir", defaultStateDir(), "local state directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || !loopback(*listen) {
			return fmt.Errorf("local mode takes no positional arguments and requires a loopback listener")
		}
		key, err := localSecretKey(*stateDir)
		if err != nil {
			return err
		}
		localCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 2)
		go func() {
			done <- server.Run(localCtx, server.Config{
				DatabaseURL: *databaseURL, Listen: *listen, LocalAuth: true, StateDir: *stateDir,
				CatalogSecret: env("NEB_CATALOG_SECRET", "nebulous-local-catalog-secret"),
				ExtensionDir:  extensionDir(*stateDir), Storage: storageConfig(), QueryTimeout: 10 * time.Second,
				AppHostSuffix: env("NEB_APP_HOST_SUFFIX", "apps.localhost"), AppScheme: "http",
				SecretKey: key, AllowPrivateConnectors: env("NEB_ALLOW_PRIVATE_CONNECTORS", "") == "true", Log: os.Stderr,
			})
		}()
		go func() {
			done <- jobs.Run(localCtx, jobs.Config{
				DatabaseURL: *databaseURL, CatalogSecret: env("NEB_CATALOG_SECRET", "nebulous-local-catalog-secret"),
				ExtensionDir: extensionDir(*stateDir), Storage: storageConfig(), SecretKey: key,
				AllowPrivateConnectors: env("NEB_ALLOW_PRIVATE_CONNECTORS", "") == "true", Log: os.Stderr,
			})
		}()
		err = <-done
		cancel()
		<-done
		return err
	case "migrate":
		flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
		databaseURL := flags.String("database-url", env("NEB_DATABASE_URL", localDatabaseURL()), "PostgreSQL URL")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("migrate takes no positional arguments")
		}
		database, err := db.Open(ctx, *databaseURL)
		if err != nil {
			return err
		}
		defer database.Close()
		return db.Migrate(ctx, database)
	default:
		return fmt.Errorf("unknown internal platform mode %q", args[0])
	}
}

func localSecretKey(stateDir string) (string, error) {
	return secretbox.LoadOrCreate(filepath.Join(stateDir, "connector.key"))
}

func loopback(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
}

func extensionDir(stateDir string) string {
	return env("NEB_DUCKDB_EXTENSION_DIR", filepath.Join(stateDir, "duckdb", "extensions"))
}

func storageConfig() storage.Config {
	return storage.Config{
		Endpoint:  env("NEB_S3_ENDPOINT", "http://127.0.0.1:"+env("NEB_LOCAL_S3_PORT", "9000")),
		Region:    env("NEB_S3_REGION", "us-east-1"),
		AccessKey: env("NEB_S3_ACCESS_KEY", env("NEB_LOCAL_S3_ACCESS_KEY", "nebulous")),
		SecretKey: env("NEB_S3_SECRET_KEY", env("NEB_LOCAL_S3_SECRET_KEY", "nebulous-local-secret")),
		Bucket:    env("NEB_S3_BUCKET", env("NEB_LOCAL_S3_BUCKET", "nebulous")),
	}
}

func defaultStateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "nebulous")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "nebulous-state")
	}
	return filepath.Join(home, ".local", "state", "nebulous")
}

func localDatabaseURL() string {
	return "postgresql://nebulous:nebulous-local@127.0.0.1:" + env("NEB_LOCAL_POSTGRES_PORT", "55432") + "/nebulous?sslmode=disable"
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
