package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"nebulous/internal/db"
	"nebulous/internal/queryexec"
	"nebulous/internal/storage"
)

type Config struct {
	DatabaseURL   string
	CatalogSecret string
	ExtensionDir  string
	Storage       storage.Config
	WorkerID      string
	PollInterval  time.Duration
	Lease         time.Duration
	UploadLimit   int64
	Log           io.Writer
}

func Run(ctx context.Context, config Config) error {
	if config.PollInterval <= 0 {
		config.PollInterval = 250 * time.Millisecond
	}
	if config.Lease <= 0 {
		config.Lease = 30 * time.Second
	}
	if config.UploadLimit <= 0 {
		config.UploadLimit = 1 << 30
	}
	if config.WorkerID == "" {
		hostname, _ := os.Hostname()
		config.WorkerID = hostname + "-" + strconv.Itoa(os.Getpid())
	}
	if config.Log == nil {
		config.Log = io.Discard
	}
	database, err := db.Open(ctx, config.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := db.Migrate(ctx, database); err != nil {
		return err
	}
	objects, err := storage.New(config.Storage)
	if err != nil {
		return err
	}
	if err := queryexec.EnsureExtensions(ctx, config.ExtensionDir); err != nil {
		return err
	}
	for {
		job, err := db.ClaimImportJob(ctx, database, config.WorkerID, config.Lease)
		if err == nil {
			if err := runImport(ctx, database, objects, config, job); err != nil {
				fmt.Fprintf(config.Log, "dataset import %s failed: %v\n", job.ID, err)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			fmt.Fprintf(config.Log, "claim import job: %v\n", err)
		}
		delay := config.PollInterval + time.Duration(rand.Int64N(int64(config.PollInterval/5+1)))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func runImport(ctx context.Context, database *sql.DB, objects *storage.Client, config Config, job db.ImportJob) error {
	jobContext, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan error, 1)
	go func() { heartbeatDone <- heartbeat(jobContext, cancel, database, job, config.Lease) }()

	details, err := db.GetImportDetails(jobContext, database, job)
	if err == nil && details.ByteCount > config.UploadLimit {
		err = fmt.Errorf("upload exceeds %d-byte limit", config.UploadLimit)
	}
	var staged *os.File
	if err == nil {
		staged, err = os.CreateTemp("", "nebulous-import-*."+details.Format)
		if err != nil {
			err = fmt.Errorf("create import staging file: %w", err)
		}
	}
	if staged != nil {
		path := staged.Name()
		defer os.Remove(path)
		defer staged.Close()
		var downloaded int64
		if err == nil {
			downloaded, err = objects.Download(jobContext, details.ObjectKey, staged, details.ByteCount)
		}
		if err == nil && downloaded != details.ByteCount {
			err = fmt.Errorf("uploaded object is %d bytes; expected %d", downloaded, details.ByteCount)
		}
		if err == nil {
			err = staged.Close()
		}
		if err == nil {
			var catalogURL string
			catalogURL, err = db.EnsureTenantCatalog(jobContext, database, config.DatabaseURL, config.CatalogSecret, job.TenantID)
			if err == nil {
				var result db.ImportResult
				result, err = queryexec.ImportDataset(jobContext, queryexec.DatasetImport{
					CatalogURL: catalogURL, DataPath: objects.DataPath(job.TenantID),
					ExtensionDir: config.ExtensionDir, Storage: config.Storage,
					DatasetSlug: details.DatasetSlug, Format: details.Format, File: filepath.Clean(path),
				})
				if err == nil {
					err = db.CompleteImportJob(jobContext, database, job, result)
				}
			}
		}
	}
	cancel()
	if heartbeatErr := <-heartbeatDone; err == nil {
		err = heartbeatErr
	}
	if err != nil {
		if failureErr := db.FailImportJob(ctx, database, job, err); failureErr != nil && !errors.Is(failureErr, db.ErrLeaseLost) {
			return fmt.Errorf("%w (record failure: %v)", err, failureErr)
		}
	}
	return err
}

func heartbeat(ctx context.Context, cancel context.CancelFunc, database *sql.DB, job db.ImportJob, lease time.Duration) error {
	ticker := time.NewTicker(lease / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := db.HeartbeatImportJob(ctx, database, job, lease); err != nil {
				cancel()
				return err
			}
		}
	}
}
