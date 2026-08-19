package queryexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"

	"oort/internal/db"
	"oort/internal/storage"
)

type DatasetImport struct {
	CatalogURL   string
	DataPath     string
	ExtensionDir string
	Storage      storage.Config
	DatasetSlug  string
	Format       string
	File         string
}

type DatasetCatalog struct {
	CatalogURL   string
	DataPath     string
	ExtensionDir string
	Storage      storage.Config
	DatasetSlug  string
}

type schemaColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func EnsureExtensions(ctx context.Context, extensionDir string) error {
	if extensionDir == "" {
		return fmt.Errorf("DuckDB extension directory is required")
	}
	if err := os.MkdirAll(extensionDir, 0o700); err != nil {
		return fmt.Errorf("create DuckDB extension directory: %w", err)
	}
	database, err := sql.Open("duckdb", "")
	if err != nil {
		return fmt.Errorf("open DuckDB for extension installation: %w", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	connection, err := database.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect DuckDB for extension installation: %w", err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, "SET extension_directory = "+sqlString(extensionDir)); err != nil {
		return fmt.Errorf("configure DuckDB extension directory: %w", err)
	}
	for _, name := range []string{"httpfs", "postgres", "ducklake"} {
		if _, err := connection.ExecContext(ctx, "INSTALL "+name); err != nil {
			return fmt.Errorf("install DuckDB %s extension: %w", name, err)
		}
	}
	return nil
}

func ImportDataset(ctx context.Context, input DatasetImport) (db.ImportResult, error) {
	connection, closeConnection, err := openLake(ctx, input.CatalogURL, input.DataPath, input.ExtensionDir, "", input.Storage, false, false)
	if err != nil {
		return db.ImportResult{}, err
	}
	defer closeConnection()
	file, err := filepath.Abs(input.File)
	if err != nil {
		return db.ImportResult{}, fmt.Errorf("resolve staged file: %w", err)
	}
	var source string
	switch input.Format {
	case "csv":
		source = "read_csv_auto(" + sqlString(file) + ", header = true)"
	case "parquet":
		source = "read_parquet(" + sqlString(file) + ")"
	case "json":
		source = "read_json_auto(" + sqlString(file) + ", format = 'newline_delimited')"
	default:
		return db.ImportResult{}, fmt.Errorf("unsupported dataset format %q", input.Format)
	}
	if _, err := connection.ExecContext(ctx, "CREATE TEMP VIEW incoming AS SELECT * FROM "+source); err != nil {
		return db.ImportResult{}, fmt.Errorf("inspect uploaded %s: %w", input.Format, err)
	}
	incoming, err := describe(ctx, connection, "incoming")
	if err != nil {
		return db.ImportResult{}, err
	}
	table := quoteIdentifier(input.DatasetSlug)
	var exists bool
	if err := connection.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_catalog = 'lake' AND table_schema = 'main' AND table_name = ?
	)`, input.DatasetSlug).Scan(&exists); err != nil {
		return db.ImportResult{}, fmt.Errorf("check dataset table: %w", err)
	}
	if exists {
		current, err := describe(ctx, connection, "lake.main."+table)
		if err != nil {
			return db.ImportResult{}, err
		}
		if !reflect.DeepEqual(current, incoming) {
			return db.ImportResult{}, fmt.Errorf("dataset schema changed from %v to %v", current, incoming)
		}
	}
	transaction, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return db.ImportResult{}, fmt.Errorf("start DuckLake import: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, "CREATE OR REPLACE TABLE lake.main."+table+" AS SELECT * FROM incoming"); err != nil {
		return db.ImportResult{}, fmt.Errorf("write DuckLake dataset: %w", err)
	}
	var rowCount int64
	if err := transaction.QueryRowContext(ctx, "SELECT count(*) FROM lake.main."+table).Scan(&rowCount); err != nil {
		return db.ImportResult{}, fmt.Errorf("count imported rows: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return db.ImportResult{}, fmt.Errorf("commit DuckLake import: %w", err)
	}
	var snapshotID int64
	if err := connection.QueryRowContext(ctx, "SELECT id FROM lake.current_snapshot()").Scan(&snapshotID); err != nil {
		return db.ImportResult{}, fmt.Errorf("read DuckLake snapshot: %w", err)
	}
	schemaJSON, _ := json.Marshal(incoming)
	return db.ImportResult{SnapshotID: snapshotID, RowCount: rowCount, Schema: schemaJSON}, nil
}

func DropDataset(ctx context.Context, input DatasetCatalog) error {
	connection, closeConnection, err := openLake(ctx, input.CatalogURL, input.DataPath, input.ExtensionDir, "", input.Storage, false, false)
	if err != nil {
		return err
	}
	defer closeConnection()
	if _, err := connection.ExecContext(ctx, "DROP TABLE IF EXISTS lake.main."+quoteIdentifier(input.DatasetSlug)); err != nil {
		return fmt.Errorf("drop DuckLake dataset: %w", err)
	}
	return nil
}

func Child(input io.Reader, output io.Writer) error {
	request, err := DecodeRequest(input)
	if err != nil {
		return err
	}
	if request.MaxRows < 1 || request.MaxRows > 10_000 || request.MaxBytes < 1 || request.MaxBytes > 10<<20 {
		return fmt.Errorf("invalid query result limits")
	}
	cleaned, parameterTypes, err := Validate(request.SQL, request.Parameters)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(parameterTypes, request.ParameterTypes) {
		return fmt.Errorf("query parameter types changed")
	}
	connection, closeConnection, err := openLake(context.Background(), request.CatalogURL, request.DataPath,
		request.ExtensionDir, request.TempDir, request.Storage, true, true)
	if err != nil {
		return err
	}
	defer closeConnection()
	return streamQuery(context.Background(), connection, output, cleaned, request.Parameters, request.MaxRows, request.MaxBytes)
}

func openLake(ctx context.Context, catalogURL, dataPath, extensionDir, tempDir string, s3 storage.Config, readOnly, secure bool) (*sql.Conn, func(), error) {
	if catalogURL == "" || dataPath == "" || extensionDir == "" {
		return nil, nil, fmt.Errorf("DuckLake catalog, data path, and extension directory are required")
	}
	database, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, nil, fmt.Errorf("open DuckDB: %w", err)
	}
	database.SetMaxOpenConns(1)
	connection, err := database.Conn(ctx)
	if err != nil {
		database.Close()
		return nil, nil, fmt.Errorf("connect DuckDB: %w", err)
	}
	closeConnection := func() { connection.Close(); database.Close() }
	settings := []string{
		"SET extension_directory = " + sqlString(extensionDir),
		"SET autoinstall_known_extensions = false",
		"SET autoload_known_extensions = false",
		"SET allow_community_extensions = false",
	}
	for _, statement := range settings {
		if _, err := connection.ExecContext(ctx, statement); err != nil {
			closeConnection()
			return nil, nil, fmt.Errorf("secure DuckDB configuration: %w", err)
		}
	}
	for _, name := range []string{"httpfs", "postgres", "ducklake"} {
		if _, err := connection.ExecContext(ctx, "LOAD "+name); err != nil {
			closeConnection()
			return nil, nil, fmt.Errorf("load preinstalled DuckDB %s extension: %w", name, err)
		}
	}
	if _, err := connection.ExecContext(ctx, "SET enable_global_s3_configuration = false"); err != nil {
		closeConnection()
		return nil, nil, fmt.Errorf("disable ambient S3 credentials: %w", err)
	}
	endpoint, err := url.Parse(s3.Endpoint)
	if err != nil || endpoint.Host == "" {
		closeConnection()
		return nil, nil, fmt.Errorf("invalid S3 endpoint")
	}
	useSSL := endpoint.Scheme == "https"
	secret := fmt.Sprintf(`CREATE OR REPLACE SECRET oort_s3 (
		TYPE s3, PROVIDER config, KEY_ID %s, SECRET %s, REGION %s,
		ENDPOINT %s, URL_STYLE 'path', USE_SSL %t, SCOPE %s)`,
		sqlString(s3.AccessKey), sqlString(s3.SecretKey), sqlString(s3.Region), sqlString(endpoint.Host), useSSL,
		sqlString(dataPath))
	if _, err := connection.ExecContext(ctx, secret); err != nil {
		closeConnection()
		return nil, nil, fmt.Errorf("configure DuckDB S3 access: %w", err)
	}
	options := "DATA_PATH " + sqlString(dataPath)
	if readOnly {
		options += ", READ_ONLY"
	}
	metadataPath, err := duckLakeMetadataPath(catalogURL)
	if err != nil {
		closeConnection()
		return nil, nil, err
	}
	attach := "ATTACH " + sqlString("ducklake:"+metadataPath) + " AS lake (" + options + ")"
	if _, err := connection.ExecContext(ctx, attach); err != nil {
		closeConnection()
		return nil, nil, fmt.Errorf("attach DuckLake catalog: %w", sanitizedCatalogError(err, catalogURL))
	}
	if _, err := connection.ExecContext(ctx, "USE lake"); err != nil {
		closeConnection()
		return nil, nil, fmt.Errorf("select DuckLake catalog: %w", err)
	}
	if secure {
		secureSettings := []string{
			"SET memory_limit = '512MB'",
			"SET max_temp_directory_size = '1GB'",
			"SET threads = 1",
		}
		if tempDir != "" {
			secureSettings = append(secureSettings, "SET temp_directory = "+sqlString(tempDir))
		}
		secureSettings = append(secureSettings,
			"SET allowed_directories = ["+sqlString(dataPath)+"]",
			"SET enable_external_access = false",
			"SET lock_configuration = true",
		)
		for _, statement := range secureSettings {
			if _, err := connection.ExecContext(ctx, statement); err != nil {
				closeConnection()
				return nil, nil, fmt.Errorf("lock DuckDB query configuration at %q: %w", statement, err)
			}
		}
	}
	return connection, closeConnection, nil
}

func streamQuery(ctx context.Context, connection *sql.Conn, output io.Writer, sqlText string, parameters map[string]any, maxRows int, maxBytes int64) error {
	var snapshotID int64
	if err := connection.QueryRowContext(ctx, "SELECT id FROM lake.current_snapshot()").Scan(&snapshotID); err != nil {
		return fmt.Errorf("read query snapshot: %w", err)
	}
	rows, err := connection.QueryContext(ctx, sqlText, normalizedParameters(parameters)...)
	if err != nil {
		return fmt.Errorf("execute saved query: %w", err)
	}
	defer rows.Close()
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return fmt.Errorf("read result columns: %w", err)
	}
	columns := make([]Column, len(columnTypes))
	for index, column := range columnTypes {
		columns[index] = Column{Name: column.Name(), Type: column.DatabaseTypeName()}
	}
	limited := &limitWriter{writer: output, remaining: maxBytes}
	columnsJSON, _ := json.Marshal(columns)
	if _, err := fmt.Fprintf(limited, `{"columns":%s,"rows":[`, columnsJSON); err != nil {
		return err
	}
	count := 0
	for count < maxRows && rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return fmt.Errorf("scan query row: %w", err)
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return fmt.Errorf("encode query row: %w", err)
		}
		if count > 0 {
			if _, err := limited.Write([]byte{','}); err != nil {
				return err
			}
		}
		if _, err := limited.Write(encoded); err != nil {
			return err
		}
		count++
	}
	truncated := false
	if count == maxRows && rows.Next() {
		truncated = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("stream query rows: %w", err)
	}
	_, err = fmt.Fprintf(limited, `],"snapshot_id":%d,"truncated":%t}`, snapshotID, truncated)
	return err
}

func describe(ctx context.Context, connection *sql.Conn, relation string) ([]schemaColumn, error) {
	rows, err := connection.QueryContext(ctx, "DESCRIBE SELECT * FROM "+relation)
	if err != nil {
		return nil, fmt.Errorf("describe dataset: %w", err)
	}
	defer rows.Close()
	var columns []schemaColumn
	for rows.Next() {
		var column schemaColumn
		var nullable, key, defaultValue, extra any
		if err := rows.Scan(&column.Name, &column.Type, &nullable, &key, &defaultValue, &extra); err != nil {
			return nil, fmt.Errorf("scan dataset schema: %w", err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("describe dataset: %w", err)
	}
	return columns, nil
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }
func sqlString(value string) string       { return `'` + strings.ReplaceAll(value, `'`, `''`) + `'` }

func duckLakeMetadataPath(catalogURL string) (string, error) {
	parsed, err := url.Parse(catalogURL)
	if err != nil || parsed.Hostname() == "" || parsed.User == nil {
		return "", fmt.Errorf("invalid DuckLake PostgreSQL catalog URL")
	}
	password, ok := parsed.User.Password()
	if !ok {
		return "", fmt.Errorf("DuckLake PostgreSQL catalog password is required")
	}
	parts := []string{
		"host=" + postgresValue(parsed.Hostname()),
		"dbname=" + postgresValue(strings.TrimPrefix(parsed.Path, "/")),
		"user=" + postgresValue(parsed.User.Username()),
		"password=" + postgresValue(password),
	}
	if port := parsed.Port(); port != "" {
		parts = append(parts, "port="+postgresValue(port))
	}
	if sslmode := parsed.Query().Get("sslmode"); sslmode != "" {
		parts = append(parts, "sslmode="+postgresValue(sslmode))
	}
	return "postgres:" + strings.Join(parts, " "), nil
}

func postgresValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return `'` + strings.ReplaceAll(value, `'`, `\'`) + `'`
}

func sanitizedCatalogError(failure error, catalogURL string) error {
	message := failure.Error()
	if parsed, err := url.Parse(catalogURL); err == nil && parsed.User != nil {
		if password, ok := parsed.User.Password(); ok && password != "" {
			message = strings.ReplaceAll(message, password, "[redacted]")
		}
	}
	return fmt.Errorf("%s", message)
}
