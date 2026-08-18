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

	"oort/internal/db"
	"oort/internal/jobs"
	"oort/internal/secretbox"
	"oort/internal/server"
	"oort/internal/storage"
)

func Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing internal platform mode")
	}
	switch args[0] {
	case "server":
		flags := flag.NewFlagSet("server", flag.ContinueOnError)
		listen := flags.String("listen", env("OORT_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
		databaseURL := flags.String("database-url", env("OORT_DATABASE_URL", localDatabaseURL()), "PostgreSQL URL")
		localAuth := flags.Bool("local-auth", false, "create a loopback-only local identity")
		stateDir := flags.String("state-dir", defaultStateDir(), "local state directory")
		controlHost := flags.String("control-host", env("OORT_CONTROL_HOST", ""), "accepted control-plane host")
		appHostSuffix := flags.String("app-host-suffix", env("OORT_APP_HOST_SUFFIX", "apps.localhost"), "app runtime host suffix")
		appScheme := flags.String("app-scheme", env("OORT_APP_SCHEME", "http"), "app URL scheme")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("server takes no positional arguments")
		}
		secretKey := env("OORT_SECRET_KEY", "")
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
			CatalogSecret:          env("OORT_CATALOG_SECRET", "oort-local-catalog-secret"),
			ExtensionDir:           extensionDir(*stateDir),
			Storage:                storageConfig(),
			QueryTimeout:           10 * time.Second,
			ControlHost:            *controlHost,
			AppHostSuffix:          *appHostSuffix,
			AppScheme:              *appScheme,
			SecureCookies:          *appScheme == "https",
			OIDCIssuer:             env("OORT_OIDC_ISSUER", ""),
			OIDCClientID:           env("OORT_OIDC_CLIENT_ID", ""),
			OIDCSecret:             env("OORT_OIDC_CLIENT_SECRET", ""),
			PublicURL:              env("OORT_PUBLIC_URL", ""),
			SecretKey:              secretKey,
			AllowPrivateConnectors: env("OORT_ALLOW_PRIVATE_CONNECTORS", "") == "true",
			Log:                    os.Stderr,
		})
	case "worker":
		flags := flag.NewFlagSet("worker", flag.ContinueOnError)
		databaseURL := flags.String("database-url", env("OORT_DATABASE_URL", localDatabaseURL()), "PostgreSQL URL")
		stateDir := flags.String("state-dir", defaultStateDir(), "local state directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("worker takes no positional arguments")
		}
		return jobs.Run(ctx, jobs.Config{
			DatabaseURL:            *databaseURL,
			CatalogSecret:          env("OORT_CATALOG_SECRET", "oort-local-catalog-secret"),
			ExtensionDir:           extensionDir(*stateDir),
			Storage:                storageConfig(),
			SecretKey:              env("OORT_SECRET_KEY", ""),
			AllowPrivateConnectors: env("OORT_ALLOW_PRIVATE_CONNECTORS", "") == "true",
			Log:                    os.Stderr,
		})
	case "local":
		flags := flag.NewFlagSet("local", flag.ContinueOnError)
		listen := flags.String("listen", env("OORT_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
		databaseURL := flags.String("database-url", env("OORT_DATABASE_URL", localDatabaseURL()), "PostgreSQL URL")
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
				CatalogSecret: env("OORT_CATALOG_SECRET", "oort-local-catalog-secret"),
				ExtensionDir:  extensionDir(*stateDir), Storage: storageConfig(), QueryTimeout: 10 * time.Second,
				AppHostSuffix: env("OORT_APP_HOST_SUFFIX", "apps.localhost"), AppScheme: "http",
				SecretKey: key, AllowPrivateConnectors: env("OORT_ALLOW_PRIVATE_CONNECTORS", "") == "true", Log: os.Stderr,
			})
		}()
		go func() {
			done <- jobs.Run(localCtx, jobs.Config{
				DatabaseURL: *databaseURL, CatalogSecret: env("OORT_CATALOG_SECRET", "oort-local-catalog-secret"),
				ExtensionDir: extensionDir(*stateDir), Storage: storageConfig(), SecretKey: key,
				AllowPrivateConnectors: env("OORT_ALLOW_PRIVATE_CONNECTORS", "") == "true", Log: os.Stderr,
			})
		}()
		err = <-done
		cancel()
		<-done
		return err
	case "migrate":
		flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
		databaseURL := flags.String("database-url", env("OORT_DATABASE_URL", localDatabaseURL()), "PostgreSQL URL")
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
	return env("OORT_DUCKDB_EXTENSION_DIR", filepath.Join(stateDir, "duckdb", "extensions"))
}

func storageConfig() storage.Config {
	return storage.Config{
		Endpoint:  env("OORT_S3_ENDPOINT", "http://127.0.0.1:"+env("OORT_LOCAL_S3_PORT", "9000")),
		Region:    env("OORT_S3_REGION", "us-east-1"),
		AccessKey: env("OORT_S3_ACCESS_KEY", env("OORT_LOCAL_S3_ACCESS_KEY", "oort")),
		SecretKey: env("OORT_S3_SECRET_KEY", env("OORT_LOCAL_S3_SECRET_KEY", "oort-local-secret")),
		Bucket:    env("OORT_S3_BUCKET", env("OORT_LOCAL_S3_BUCKET", "oort")),
	}
}

func defaultStateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "oort")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "oort-state")
	}
	return filepath.Join(home, ".local", "state", "oort")
}

func localDatabaseURL() string {
	return "postgresql://oort:oort-local@127.0.0.1:" + env("OORT_LOCAL_POSTGRES_PORT", "55432") + "/oort?sslmode=disable"
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
