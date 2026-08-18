package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"nebulous/internal/db"
	"nebulous/internal/jobs"
	"nebulous/internal/queryexec"
	"nebulous/internal/server"
	"nebulous/internal/storage"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "nebulous:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: nebulous server|worker|query-exec|migrate")
	}
	switch args[0] {
	case "server":
		flags := flag.NewFlagSet("server", flag.ContinueOnError)
		listen := flags.String("listen", env("NEB_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
		databaseURL := flags.String("database-url", env("NEB_DATABASE_URL", localDatabaseURL()), "PostgreSQL URL")
		localAuth := flags.Bool("local-auth", false, "create a loopback-only local identity")
		stateDir := flags.String("state-dir", defaultStateDir(), "local state directory")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("server takes no positional arguments")
		}
		return server.Run(ctx, server.Config{
			DatabaseURL:   *databaseURL,
			Listen:        *listen,
			LocalAuth:     *localAuth,
			StateDir:      *stateDir,
			CatalogSecret: env("NEB_CATALOG_SECRET", "nebulous-local-catalog-secret"),
			ExtensionDir:  extensionDir(*stateDir),
			Storage:       storageConfig(),
			QueryTimeout:  10 * time.Second,
			Log:           os.Stderr,
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
			DatabaseURL:   *databaseURL,
			CatalogSecret: env("NEB_CATALOG_SECRET", "nebulous-local-catalog-secret"),
			ExtensionDir:  extensionDir(*stateDir),
			Storage:       storageConfig(),
			Log:           os.Stderr,
		})
	case "query-exec":
		if len(args) != 1 {
			return fmt.Errorf("query-exec takes no arguments")
		}
		return queryexec.Child(os.Stdin, os.Stdout)
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
		return fmt.Errorf("unknown mode %q; use server, worker, query-exec, or migrate", args[0])
	}
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
