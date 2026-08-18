package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"time"

	"oort/internal/connector"
	"oort/internal/db"
	"oort/internal/manifest"
	"oort/internal/queryexec"
	"oort/internal/secretbox"
	"oort/internal/storage"
)

type Config struct {
	DatabaseURL            string
	CatalogSecret          string
	ExtensionDir           string
	Storage                storage.Config
	WorkerID               string
	PollInterval           time.Duration
	Lease                  time.Duration
	UploadLimit            int64
	SecretKey              string
	AllowPrivateConnectors bool
	Log                    io.Writer
	logger                 *slog.Logger
}

var errCancelled = errors.New("job cancellation requested")

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
	config.logger = slog.New(slog.NewJSONHandler(config.Log, nil))
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
		if _, err := db.EnqueueDueConnector(ctx, database); err != nil {
			config.logger.Error("enqueue due connector", "error", err)
		}
		job, err := db.ClaimImportJob(ctx, database, config.WorkerID, config.Lease)
		if err == nil {
			if err := runImport(ctx, database, objects, config, job); err != nil {
				config.logger.Error("dataset import failed", "job_id", job.ID, "tenant_id", job.TenantID, "error", err)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			config.logger.Error("claim import job", "error", err)
		}
		connectorJob, connectorErr := db.ClaimConnectorJob(ctx, database, config.WorkerID, config.Lease)
		if connectorErr == nil {
			if err := runConnector(ctx, database, objects, config, connectorJob); err != nil {
				config.logger.Error("connector sync failed", "job_id", connectorJob.ID, "tenant_id", connectorJob.TenantID, "error", err)
			}
			continue
		}
		if !errors.Is(connectorErr, sql.ErrNoRows) {
			config.logger.Error("claim connector job", "error", connectorErr)
		}
		publish, publishErr := db.ClaimPublishJob(ctx, database, config.WorkerID, config.Lease)
		if publishErr == nil {
			if err := runPublish(ctx, database, objects, config, publish); err != nil {
				config.logger.Error("app publish failed", "job_id", publish.ID, "tenant_id", publish.TenantID, "error", err)
			}
			continue
		}
		if !errors.Is(publishErr, sql.ErrNoRows) {
			config.logger.Error("claim publish job", "error", publishErr)
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

func runPublish(ctx context.Context, database *sql.DB, objects *storage.Client, config Config, job db.PublishJob) error {
	_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "info", "Publishing app bundle")
	jobContext, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan error, 1)
	go func() { heartbeatDone <- publishHeartbeat(jobContext, cancel, database, job, config.Lease) }()

	details, err := db.GetPublishDetails(jobContext, database, job)
	var bundle bytes.Buffer
	if err == nil {
		var downloaded int64
		downloaded, err = objects.Download(jobContext, details.Deployment.ObjectKey, &bundle, details.Deployment.ByteCount)
		if err == nil && downloaded != details.Deployment.ByteCount {
			err = fmt.Errorf("uploaded bundle is %d bytes; expected %d", downloaded, details.Deployment.ByteCount)
		}
	}
	if err == nil {
		checksum := sha256.Sum256(bundle.Bytes())
		if !bytes.Equal(checksum[:], details.Deployment.Checksum) {
			err = fmt.Errorf("uploaded bundle checksum does not match")
		}
	}
	var published []db.PublishedQuery
	if err == nil {
		var bundled manifest.Manifest
		var queryFiles map[string]string
		bundled, queryFiles, err = manifest.ReadBundle(bundle.Bytes())
		if err == nil {
			var expected manifest.Manifest
			expected, err = manifest.Parse(details.Deployment.Manifest)
			if err == nil && !reflect.DeepEqual(bundled, expected) {
				err = fmt.Errorf("uploaded bundle manifest does not match deployment")
			}
		}
		if err == nil {
			for _, query := range bundled.Queries {
				parameters := sampleParameters(query.Parameters)
				cleaned, types, validateErr := queryexec.Validate(queryFiles[query.Name], parameters)
				if validateErr != nil {
					err = fmt.Errorf("query %q: %w", query.Name, validateErr)
					break
				}
				if !reflect.DeepEqual(types, query.Parameters) {
					err = fmt.Errorf("query %q parameter types do not match its manifest", query.Name)
					break
				}
				published = append(published, db.PublishedQuery{Name: query.Name, SQL: cleaned, ParameterTypes: types})
			}
		}
	}
	if err == nil {
		err = db.CompletePublishJob(jobContext, database, job, published)
	}
	cancel()
	heartbeatErr := <-heartbeatDone
	if errors.Is(heartbeatErr, errCancelled) || cancellationRequested(ctx, database, job.TenantID, job.ID, job.WorkerID) {
		_ = db.CancelClaimedJob(ctx, database, job.TenantID, job.ID, job.WorkerID)
		_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "info", "Job cancelled")
		return nil
	}
	if err == nil {
		err = heartbeatErr
	}
	if err != nil {
		_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "error", err.Error())
		if failureErr := db.FailPublishJob(ctx, database, job, err); failureErr != nil && !errors.Is(failureErr, db.ErrLeaseLost) {
			return fmt.Errorf("%w (record failure: %v)", err, failureErr)
		}
	}
	if err == nil {
		_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "info", "App bundle published")
	}
	return err
}

func sampleParameters(types map[string]string) map[string]any {
	values := make(map[string]any, len(types))
	for name, kind := range types {
		switch kind {
		case "boolean":
			values[name] = false
		case "integer":
			values[name] = float64(0)
		case "number":
			values[name] = float64(0.5)
		case "string":
			values[name] = ""
		}
	}
	return values
}

func publishHeartbeat(ctx context.Context, cancel context.CancelFunc, database *sql.DB, job db.PublishJob, lease time.Duration) error {
	ticker := time.NewTicker(lease / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if ctx.Err() != nil {
				return nil
			}
			requested, err := db.JobCancellationRequested(ctx, database, job.TenantID, job.ID, job.WorkerID)
			if err != nil {
				cancel()
				return err
			}
			if requested {
				cancel()
				return errCancelled
			}
			if err := db.HeartbeatPublishJob(ctx, database, job, lease); err != nil {
				cancel()
				return err
			}
		}
	}
}

func runImport(ctx context.Context, database *sql.DB, objects *storage.Client, config Config, job db.ImportJob) error {
	_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "info", "Importing dataset")
	jobContext, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan error, 1)
	go func() { heartbeatDone <- heartbeat(jobContext, cancel, database, job, config.Lease) }()

	details, err := db.GetImportDetails(jobContext, database, job)
	if err == nil && details.ByteCount > config.UploadLimit {
		err = fmt.Errorf("upload exceeds %d-byte limit", config.UploadLimit)
	}
	var staged *os.File
	if err == nil {
		staged, err = os.CreateTemp("", "oort-import-*."+details.Format)
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
	heartbeatErr := <-heartbeatDone
	if errors.Is(heartbeatErr, errCancelled) || cancellationRequested(ctx, database, job.TenantID, job.ID, job.WorkerID) {
		_ = db.CancelClaimedJob(ctx, database, job.TenantID, job.ID, job.WorkerID)
		_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "info", "Job cancelled")
		return nil
	}
	if err == nil {
		err = heartbeatErr
	}
	if err != nil {
		_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "error", err.Error())
		if failureErr := db.FailImportJob(ctx, database, job, err); failureErr != nil && !errors.Is(failureErr, db.ErrLeaseLost) {
			return fmt.Errorf("%w (record failure: %v)", err, failureErr)
		}
	}
	if err == nil {
		_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "info", "Dataset snapshot published")
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
			if ctx.Err() != nil {
				return nil
			}
			requested, err := db.JobCancellationRequested(ctx, database, job.TenantID, job.ID, job.WorkerID)
			if err != nil {
				cancel()
				return err
			}
			if requested {
				cancel()
				return errCancelled
			}
			if err := db.HeartbeatImportJob(ctx, database, job, lease); err != nil {
				cancel()
				return err
			}
		}
	}
}

func runConnector(ctx context.Context, database *sql.DB, objects *storage.Client, config Config, job db.ImportJob) error {
	_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "info", "Fetching connector data")
	jobContext, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan error, 1)
	go func() { heartbeatDone <- heartbeat(jobContext, cancel, database, job, config.Lease) }()

	details, err := db.GetConnectorDetails(jobContext, database, job)
	var bearer string
	if err == nil && len(details.Ciphertext) > 0 {
		box, boxErr := secretbox.New(config.SecretKey)
		if boxErr != nil {
			err = boxErr
		} else {
			bearer, err = box.Open(details.Ciphertext, details.Nonce)
		}
	}
	var staged *os.File
	if err == nil {
		staged, err = os.CreateTemp("", "oort-connector-*.json")
	}
	if staged != nil {
		path := staged.Name()
		defer os.Remove(path)
		defer staged.Close()
		var fetched connector.Result
		if err == nil {
			cursor, next := "", ""
			if details.CursorParameter != nil {
				cursor = *details.CursorParameter
			}
			if details.NextCursorPointer != nil {
				next = *details.NextCursorPointer
			}
			fetched, err = connector.Fetch(jobContext, connector.Config{
				URL: details.URL, BearerToken: bearer, RecordsPointer: details.RecordsPointer,
				CursorParameter: cursor, NextCursorPointer: next, AllowPrivate: config.AllowPrivateConnectors,
			}, staged)
			if err == nil && fetched.Rows == 0 {
				err = fmt.Errorf("connector returned no records; keeping the previous snapshot")
			}
		}
		if err == nil {
			err = staged.Sync()
		}
		if err == nil {
			_, err = staged.Seek(0, io.SeekStart)
		}
		if err == nil {
			err = db.SetConnectorRunBytes(jobContext, database, job, fetched.Bytes)
		}
		if err == nil {
			err = objects.Upload(jobContext, details.ObjectKey, staged, fetched.Bytes)
		}
		if err == nil {
			var catalogURL string
			catalogURL, err = db.EnsureTenantCatalog(jobContext, database, config.DatabaseURL, config.CatalogSecret, job.TenantID)
			if err == nil {
				var result db.ImportResult
				result, err = queryexec.ImportDataset(jobContext, queryexec.DatasetImport{
					CatalogURL: catalogURL, DataPath: objects.DataPath(job.TenantID), ExtensionDir: config.ExtensionDir,
					Storage: config.Storage, DatasetSlug: details.DatasetSlug, Format: "json", File: filepath.Clean(path),
				})
				if err == nil {
					err = db.CompleteImportJob(jobContext, database, job, result)
				}
			}
		}
	}
	cancel()
	heartbeatErr := <-heartbeatDone
	if errors.Is(heartbeatErr, errCancelled) || cancellationRequested(ctx, database, job.TenantID, job.ID, job.WorkerID) {
		_ = db.CancelClaimedJob(ctx, database, job.TenantID, job.ID, job.WorkerID)
		_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "info", "Job cancelled")
		return nil
	}
	if err == nil {
		err = heartbeatErr
	}
	if err == nil {
		_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "info", "Connector snapshot published")
		return nil
	}
	if delay, retryable := connector.Retryable(err); retryable {
		if delay <= 0 {
			delay = time.Duration(job.Attempts*job.Attempts) * time.Second
		}
		delay = min(delay, 30*time.Second)
		if retried, retryErr := db.RetryConnectorJob(ctx, database, job, err, delay); retryErr != nil {
			return fmt.Errorf("%w (schedule retry: %v)", err, retryErr)
		} else if retried {
			_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "warn", fmt.Sprintf("Fetch failed; retrying in %s", delay))
			return nil
		}
	}
	_ = db.AppendJobLog(ctx, database, job.TenantID, job.ID, "error", err.Error())
	if failureErr := db.FailImportJob(ctx, database, job, err); failureErr != nil && !errors.Is(failureErr, db.ErrLeaseLost) {
		return fmt.Errorf("%w (record failure: %v)", err, failureErr)
	}
	return err
}

func cancellationRequested(ctx context.Context, database *sql.DB, tenantID, jobID, workerID string) bool {
	requested, err := db.JobCancellationRequested(ctx, database, tenantID, jobID, workerID)
	return err == nil && requested
}
