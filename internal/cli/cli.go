package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nebulous/internal/storage"
)

const composeProject = "nebulous"

var apiClient = &http.Client{Timeout: 15 * time.Second}

type localState struct {
	APIURL string `json:"api_url"`
	Token  string `json:"token"`
	User   struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

type tenant struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		printHelp(stdout)
		return nil
	}
	jsonOutput, args := takeFlag(args, "--json")
	switch args[0] {
	case "tenant":
		return runTenant(ctx, args[1:], jsonOutput, stdout)
	case "dataset":
		return runDataset(ctx, args[1:], jsonOutput, stdout, stderr)
	case "query":
		return runQuery(ctx, args[1:], jsonOutput, stdout, stderr)
	case "local":
		return runLocal(ctx, args[1:], jsonOutput, stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q; run neb --help", args[0])
	}
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, `neb manages Nebulous.

Usage:
  neb local up|run|status|logs|down|reset
  neb tenant create <slug>
  neb tenant list
  neb dataset upload <file.csv|file.parquet> [--name <slug>] [--tenant <slug>]
  neb query run <file.sql> [--param name=value] [--tenant <slug>]

Add --json to return machine-readable output.`)
}

func runDataset(ctx context.Context, args []string, jsonOutput bool, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "upload" {
		return fmt.Errorf("usage: neb dataset upload <file.csv|file.parquet> [--name <slug>] [--tenant <slug>]")
	}
	name, args, err := takeValueFlag(args, "--name")
	if err != nil {
		return err
	}
	tenantSlug, args, err := takeValueFlag(args, "--tenant")
	if err != nil {
		return err
	}
	idempotencyKey, args, err := takeValueFlag(args, "--idempotency-key")
	if err != nil {
		return err
	}
	if len(args) != 2 || args[0] != "upload" {
		return fmt.Errorf("usage: neb dataset upload <file.csv|file.parquet> [--name <slug>] [--tenant <slug>]")
	}
	file := args[1]
	info, err := os.Stat(file)
	if err != nil {
		return fmt.Errorf("inspect upload: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1<<30 {
		return fmt.Errorf("upload must be a regular file between 1 byte and 1 GiB")
	}
	format := strings.TrimPrefix(strings.ToLower(filepath.Ext(file)), ".")
	if format != "csv" && format != "parquet" {
		return fmt.Errorf("upload file must end in .csv or .parquet")
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	}
	if idempotencyKey == "" {
		idempotencyKey = randomKey()
	}
	state, err := loadState()
	if err != nil {
		return err
	}
	tenantSlug, err = resolveTenant(ctx, state, tenantSlug)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"slug": name, "format": format, "byte_count": info.Size(), "idempotency_key": idempotencyKey,
	})
	response, err := apiRequest(ctx, state, http.MethodPost,
		"/v1/tenants/"+url.PathEscape(tenantSlug)+"/dataset-uploads", body)
	if err != nil {
		return err
	}
	var created struct {
		Dataset struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"dataset"`
		Run    syncRun `json:"run"`
		Upload *struct {
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		} `json:"upload,omitempty"`
	}
	if err := json.Unmarshal(response, &created); err != nil {
		return fmt.Errorf("decode server response: %w", err)
	}
	if created.Upload != nil {
		fmt.Fprintf(stderr, "Uploading %s (%d bytes)...\n", file, info.Size())
		if err := putFile(ctx, created.Upload.URL, created.Upload.Headers, file, info.Size()); err != nil {
			return err
		}
		response, err = apiRequest(ctx, state, http.MethodPost,
			"/v1/tenants/"+url.PathEscape(tenantSlug)+"/dataset-uploads/"+url.PathEscape(created.Run.ID)+"/complete", []byte("{}"))
		if err != nil {
			return err
		}
	}
	run, err := waitForSync(ctx, state, tenantSlug, created.Run.ID)
	if err != nil {
		return err
	}
	result := map[string]any{"dataset": created.Dataset, "run": run}
	if jsonOutput {
		return json.NewEncoder(stdout).Encode(result)
	}
	fmt.Fprintf(stdout, "Uploaded %s to %s (%d rows, snapshot %d).\n", file, created.Dataset.Slug, valueOrZero(run.RowCount), valueOrZero(run.SnapshotID))
	fmt.Fprintf(stdout, "Query it with: neb query run <file.sql> --tenant %s\n", tenantSlug)
	return nil
}

type syncRun struct {
	ID         string  `json:"id"`
	Status     string  `json:"status"`
	SnapshotID *int64  `json:"snapshot_id,omitempty"`
	RowCount   *int64  `json:"row_count,omitempty"`
	Error      *string `json:"error,omitempty"`
}

func runQuery(ctx context.Context, args []string, jsonOutput bool, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "run" {
		return fmt.Errorf("usage: neb query run <file.sql> [--param name=value] [--tenant <slug>]")
	}
	name, args, err := takeValueFlag(args, "--name")
	if err != nil {
		return err
	}
	tenantSlug, args, err := takeValueFlag(args, "--tenant")
	if err != nil {
		return err
	}
	parameterValues, args, err := takeValueFlags(args, "--param")
	if err != nil {
		return err
	}
	if len(args) != 2 || args[0] != "run" {
		return fmt.Errorf("usage: neb query run <file.sql> [--param name=value] [--tenant <slug>]")
	}
	contents, err := os.ReadFile(args[1])
	if err != nil {
		return fmt.Errorf("read query file: %w", err)
	}
	if len(contents) > 1<<20 {
		return fmt.Errorf("query file exceeds 1 MiB")
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(args[1]), filepath.Ext(args[1]))
	}
	parameters := map[string]any{}
	for _, item := range parameterValues {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			return fmt.Errorf("--param must be name=value")
		}
		if _, exists := parameters[key]; exists {
			return fmt.Errorf("query parameter %q was provided twice", key)
		}
		parameters[key] = parseParameter(value)
	}
	state, err := loadState()
	if err != nil {
		return err
	}
	tenantSlug, err = resolveTenant(ctx, state, tenantSlug)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"name": name, "sql": string(contents), "parameters": parameters})
	response, err := apiRequest(ctx, state, http.MethodPost,
		"/v1/tenants/"+url.PathEscape(tenantSlug)+"/queries/run", body)
	if err != nil {
		return err
	}
	if jsonOutput {
		_, err = stdout.Write(append(response, '\n'))
		return err
	}
	var result struct {
		Query struct {
			Slug    string `json:"slug"`
			Version int    `json:"version"`
		} `json:"query"`
		Result struct {
			Columns []struct {
				Name string `json:"name"`
			} `json:"columns"`
			Rows      [][]any `json:"rows"`
			Truncated bool    `json:"truncated"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return fmt.Errorf("decode query result: %w", err)
	}
	for index, column := range result.Result.Columns {
		if index > 0 {
			fmt.Fprint(stdout, "\t")
		}
		fmt.Fprint(stdout, column.Name)
	}
	fmt.Fprintln(stdout)
	for _, row := range result.Result.Rows {
		for index, value := range row {
			if index > 0 {
				fmt.Fprint(stdout, "\t")
			}
			if value == nil {
				fmt.Fprint(stdout, "NULL")
			} else {
				fmt.Fprint(stdout, value)
			}
		}
		fmt.Fprintln(stdout)
	}
	if result.Result.Truncated {
		fmt.Fprintln(stderr, "Result truncated at 10,000 rows.")
	}
	return nil
}

func runTenant(ctx context.Context, args []string, jsonOutput bool, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: neb tenant create <slug>|list")
	}
	state, err := loadState()
	if err != nil {
		return err
	}
	switch args[0] {
	case "create":
		if len(args) != 2 {
			return fmt.Errorf("usage: neb tenant create <slug> [--json]")
		}
		body, _ := json.Marshal(map[string]string{"slug": args[1]})
		response, err := apiRequest(ctx, state, http.MethodPost, "/v1/tenants", body)
		if err != nil {
			return err
		}
		var result struct {
			Tenant tenant `json:"tenant"`
		}
		if err := json.Unmarshal(response, &result); err != nil {
			return fmt.Errorf("decode server response: %w", err)
		}
		if jsonOutput {
			return json.NewEncoder(stdout).Encode(result)
		}
		fmt.Fprintf(stdout, "Created tenant %s (%s)\n", result.Tenant.Slug, result.Tenant.ID)
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb tenant list [--json]")
		}
		response, err := apiRequest(ctx, state, http.MethodGet, "/v1/tenants", nil)
		if err != nil {
			return err
		}
		var result struct {
			Tenants []tenant `json:"tenants"`
		}
		if err := json.Unmarshal(response, &result); err != nil {
			return fmt.Errorf("decode server response: %w", err)
		}
		if jsonOutput {
			return json.NewEncoder(stdout).Encode(result)
		}
		if len(result.Tenants) == 0 {
			fmt.Fprintln(stdout, "No tenants. Create one with: neb tenant create <slug>")
			return nil
		}
		for _, tenant := range result.Tenants {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", tenant.Slug, tenant.Role, tenant.ID)
		}
	default:
		return fmt.Errorf("unknown tenant command %q; use create or list", args[0])
	}
	return nil
}

func runLocal(ctx context.Context, args []string, jsonOutput bool, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: neb local up|run|status|logs|down|reset")
	}
	root, compose, err := findCompose()
	if err != nil {
		return err
	}
	switch args[0] {
	case "up":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb local up [--json]")
		}
		if err := localUp(ctx, root, compose, stderr); err != nil {
			return err
		}
		endpoints := localEndpoints()
		if jsonOutput {
			return json.NewEncoder(stdout).Encode(endpoints)
		}
		fmt.Fprintf(stdout, "Local dependencies are ready.\nPostgreSQL: %s\nS3: %s\nConsole: %s\n",
			endpoints["database"], endpoints["s3"], endpoints["console"])
	case "run":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb local run")
		}
		return localRun(ctx, root, stdout, stderr)
	case "status":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb local status [--json]")
		}
		return localStatus(ctx, root, compose, jsonOutput, stdout, stderr)
	case "logs":
		follow, rest := takeFlag(args[1:], "-f")
		if len(rest) > 1 {
			return fmt.Errorf("usage: neb local logs [service] [-f]")
		}
		commandArgs := []string{"logs"}
		if follow {
			commandArgs = append(commandArgs, "--follow")
		}
		commandArgs = append(commandArgs, rest...)
		if jsonOutput && !follow {
			output, err := composeCommand(ctx, root, compose, nil, nil, commandArgs...).Output()
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(map[string]string{"logs": string(output)})
		}
		return composeCommand(ctx, root, compose, stdout, stderr, commandArgs...).Run()
	case "down":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb local down [--json]")
		}
		if err := composeCommand(ctx, root, compose, stderr, stderr, "down").Run(); err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(stdout).Encode(map[string]bool{"stopped": true})
		}
		fmt.Fprintln(stdout, "Local dependencies stopped; data was preserved.")
	case "reset":
		yes, rest := takeFlag(args[1:], "--yes")
		if !yes || len(rest) != 0 {
			return fmt.Errorf("reset deletes local data; rerun as neb local reset --yes")
		}
		stateDir := defaultStateDir()
		fmt.Fprintf(stderr, "Resetting Compose project %s, volumes %s_postgres-data and %s_objectstore-data, and %s\n",
			composeProject, composeProject, composeProject, stateDir)
		if err := composeCommand(ctx, root, compose, stderr, stderr, "down", "--volumes").Run(); err != nil {
			return err
		}
		if filepath.Base(stateDir) != "nebulous" {
			return fmt.Errorf("refusing unexpected state directory %q", stateDir)
		}
		if err := os.RemoveAll(stateDir); err != nil {
			return fmt.Errorf("remove local state: %w", err)
		}
		if jsonOutput {
			return json.NewEncoder(stdout).Encode(map[string]bool{"reset": true})
		}
		fmt.Fprintln(stdout, "Local data and identity were deleted.")
	default:
		return fmt.Errorf("unknown local command %q", args[0])
	}
	return nil
}

func localUp(ctx context.Context, root, compose string, stderr io.Writer) error {
	if err := composeCommand(ctx, root, compose, io.Discard, io.Discard, "version").Run(); err != nil {
		return fmt.Errorf("Docker Compose is unavailable; install Docker and ensure `docker compose version` succeeds: %w", err)
	}
	running := composeCommand(ctx, root, compose, nil, nil, "ps", "--status", "running", "--quiet")
	output, _ := running.Output()
	if len(bytes.TrimSpace(output)) == 0 {
		for name, setting := range map[string][2]string{
			"PostgreSQL": {"NEB_LOCAL_POSTGRES_PORT", "55432"},
			"S3":         {"NEB_LOCAL_S3_PORT", "9000"},
			"S3 console": {"NEB_LOCAL_S3_CONSOLE_PORT", "9001"},
		} {
			value, err := parsePort(setting[0], setting[1])
			if err != nil {
				return err
			}
			port := strconv.Itoa(value)
			listener, err := net.Listen("tcp", "127.0.0.1:"+port)
			if err != nil {
				return fmt.Errorf("%s port %s is busy; stop its process or override the matching NEB_LOCAL_*_PORT", name, port)
			}
			listener.Close()
		}
	}
	if err := composeCommand(ctx, root, compose, stderr, stderr, "up", "-d", "--wait").Run(); err != nil {
		return fmt.Errorf("start local dependencies: %w", err)
	}
	pgUser, pgDatabase := env("NEB_LOCAL_POSTGRES_USER", "nebulous"), env("NEB_LOCAL_POSTGRES_DB", "nebulous")
	if err := composeCommand(ctx, root, compose, io.Discard, stderr, "exec", "-T", "postgres", "psql",
		"--username", pgUser, "--dbname", pgDatabase, "--tuples-only", "--command", "SELECT 1").Run(); err != nil {
		return fmt.Errorf("PostgreSQL health query failed: %w", err)
	}
	if err := headBucket(ctx); err != nil {
		return fmt.Errorf("authenticated S3 HeadBucket failed: %w", err)
	}
	return nil
}

func localRun(ctx context.Context, root string, stdout, stderr io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate neb executable: %w", err)
	}
	platform := filepath.Join(filepath.Dir(executable), "nebulous")
	var commands []*exec.Cmd
	if info, err := os.Stat(platform); err == nil && info.Mode().IsRegular() {
		commands = []*exec.Cmd{
			exec.Command(platform, "server", "--local-auth", "--state-dir", defaultStateDir()),
			exec.Command(platform, "worker", "--state-dir", defaultStateDir()),
		}
	} else {
		commands = []*exec.Cmd{
			exec.Command("go", "run", "./cmd/nebulous", "server", "--local-auth", "--state-dir", defaultStateDir()),
			exec.Command("go", "run", "./cmd/nebulous", "worker", "--state-dir", defaultStateDir()),
		}
	}
	for _, command := range commands {
		command.Dir, command.Env, command.Stdout, command.Stderr = root, os.Environ(), stdout, stderr
		if err := command.Start(); err != nil {
			for _, started := range commands {
				if started.Process != nil {
					_ = started.Process.Kill()
				}
			}
			return fmt.Errorf("start local platform: %w", err)
		}
	}
	done := make(chan error, len(commands))
	for _, command := range commands {
		go func() { done <- command.Wait() }()
	}
	var firstError error
	select {
	case firstError = <-done:
	case <-ctx.Done():
	}
	for _, command := range commands {
		if command.ProcessState == nil {
			_ = command.Process.Signal(os.Interrupt)
		}
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	remaining := len(commands) - 1
	if firstError == nil && ctx.Err() != nil {
		remaining = len(commands)
	}
	for remaining > 0 {
		select {
		case err := <-done:
			remaining--
			if firstError == nil && err != nil {
				firstError = err
			}
		case <-timer.C:
			for _, command := range commands {
				if command.ProcessState == nil {
					_ = command.Process.Kill()
				}
			}
			return firstError
		}
	}
	return firstError
}

func localStatus(ctx context.Context, root, compose string, jsonOutput bool, stdout, stderr io.Writer) error {
	if !jsonOutput {
		if err := composeCommand(ctx, root, compose, stdout, stderr, "ps").Run(); err != nil {
			return err
		}
		if state, err := readLocalState(); err == nil {
			fmt.Fprintf(stdout, "Local identity: %s\n", state.User.Email)
		}
		return nil
	}
	output, err := composeCommand(ctx, root, compose, nil, nil, "ps", "--format", "json").Output()
	if err != nil {
		return err
	}
	var services any
	if err := json.Unmarshal(output, &services); err != nil {
		services = string(output)
	}
	result := map[string]any{"services": services, "endpoints": localEndpoints()}
	if state, err := readLocalState(); err == nil {
		result["identity"] = state.User
	}
	return json.NewEncoder(stdout).Encode(result)
}

func apiRequest(ctx context.Context, state localState, method, path string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(state.APIURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+state.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := apiClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("contact %s: %w; start it with `neb local run`", state.APIURL, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read server response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				RequestID string `json:"request_id"`
			} `json:"error"`
		}
		if json.Unmarshal(payload, &failure) == nil && failure.Error.Message != "" {
			return nil, fmt.Errorf("%s: %s (request %s)", failure.Error.Code, failure.Error.Message, failure.Error.RequestID)
		}
		return nil, fmt.Errorf("server returned HTTP %d", response.StatusCode)
	}
	return payload, nil
}

func loadState() (localState, error) {
	if token := os.Getenv("NEB_TOKEN"); token != "" {
		return localState{APIURL: env("NEB_API_URL", "http://127.0.0.1:8080"), Token: token}, nil
	}
	return readLocalState()
}

func readLocalState() (localState, error) {
	path := filepath.Join(defaultStateDir(), "local.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		return localState{}, fmt.Errorf("read local identity: %w; run `neb local run`", err)
	}
	var state localState
	if err := json.Unmarshal(contents, &state); err != nil {
		return state, fmt.Errorf("decode local identity: %w", err)
	}
	if state.APIURL == "" || state.Token == "" {
		return state, fmt.Errorf("local identity is incomplete; rerun `neb local run`")
	}
	return state, nil
}

func findCompose() (string, string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	for {
		compose := filepath.Join(dir, "compose.yaml")
		if _, err := os.Stat(compose); err == nil {
			return dir, compose, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", fmt.Errorf("compose.yaml was not found; run this command from a Nebulous source checkout")
		}
		dir = parent
	}
}

func composeCommand(ctx context.Context, root, compose string, stdout, stderr io.Writer, args ...string) *exec.Cmd {
	base := []string{"compose", "-f", compose, "-p", composeProject}
	command := exec.CommandContext(ctx, "docker", append(base, args...)...)
	command.Dir, command.Env, command.Stdout, command.Stderr = root, os.Environ(), stdout, stderr
	return command
}

func localEndpoints() map[string]string {
	return map[string]string{
		"database": "postgresql://127.0.0.1:" + env("NEB_LOCAL_POSTGRES_PORT", "55432") + "/" + env("NEB_LOCAL_POSTGRES_DB", "nebulous"),
		"s3":       "http://127.0.0.1:" + env("NEB_LOCAL_S3_PORT", "9000"),
		"console":  "http://127.0.0.1:" + env("NEB_LOCAL_S3_CONSOLE_PORT", "9001"),
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

func takeFlag(args []string, flag string) (bool, []string) {
	found := false
	result := make([]string, 0, len(args))
	for _, argument := range args {
		if argument == flag {
			found = true
			continue
		}
		result = append(result, argument)
	}
	return found, result
}

func takeValueFlag(args []string, flag string) (string, []string, error) {
	values, rest, err := takeValueFlags(args, flag)
	if err != nil {
		return "", nil, err
	}
	if len(values) > 1 {
		return "", nil, fmt.Errorf("%s may be specified once", flag)
	}
	if len(values) == 0 {
		return "", rest, nil
	}
	return values[0], rest, nil
}

func takeValueFlags(args []string, flag string) ([]string, []string, error) {
	var values []string
	rest := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] != flag {
			rest = append(rest, args[index])
			continue
		}
		if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
			return nil, nil, fmt.Errorf("%s requires a value", flag)
		}
		values = append(values, args[index+1])
		index++
	}
	return values, rest, nil
}

func resolveTenant(ctx context.Context, state localState, requested string) (string, error) {
	if requested == "" {
		requested = os.Getenv("NEB_TENANT")
	}
	if requested != "" {
		return requested, nil
	}
	response, err := apiRequest(ctx, state, http.MethodGet, "/v1/tenants", nil)
	if err != nil {
		return "", err
	}
	var result struct {
		Tenants []tenant `json:"tenants"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return "", fmt.Errorf("decode tenant list: %w", err)
	}
	if len(result.Tenants) != 1 {
		return "", fmt.Errorf("select a tenant with --tenant or NEB_TENANT")
	}
	return result.Tenants[0].Slug, nil
}

func putFile(ctx context.Context, target string, headers http.Header, path string, size int64) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open upload: %w", err)
	}
	defer file.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, target, file)
	if err != nil {
		return fmt.Errorf("create upload request: %w", err)
	}
	request.ContentLength = size
	request.Header = headers.Clone()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("upload dataset: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("upload dataset: object storage returned HTTP %d", response.StatusCode)
	}
	return nil
}

func waitForSync(ctx context.Context, state localState, tenantSlug, runID string) (syncRun, error) {
	path := "/v1/tenants/" + url.PathEscape(tenantSlug) + "/sync-runs/" + url.PathEscape(runID)
	for {
		response, err := apiRequest(ctx, state, http.MethodGet, path, nil)
		if err != nil {
			return syncRun{}, err
		}
		var body struct {
			Run syncRun `json:"run"`
		}
		if err := json.Unmarshal(response, &body); err != nil {
			return syncRun{}, fmt.Errorf("decode sync run: %w", err)
		}
		switch body.Run.Status {
		case "succeeded":
			return body.Run, nil
		case "failed":
			if body.Run.Error != nil {
				return syncRun{}, fmt.Errorf("dataset import failed: %s", *body.Run.Error)
			}
			return syncRun{}, fmt.Errorf("dataset import failed")
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return syncRun{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func parseParameter(value string) any {
	if boolean, err := strconv.ParseBool(value); err == nil && (value == "true" || value == "false") {
		return boolean
	}
	if integer, err := strconv.ParseInt(value, 10, 64); err == nil {
		return integer
	}
	if number, err := strconv.ParseFloat(value, 64); err == nil {
		return number
	}
	return value
}

func randomKey() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(value[:])
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func headBucket(ctx context.Context) error {
	client, err := storage.New(storage.Config{
		Endpoint: "http://127.0.0.1:" + env("NEB_LOCAL_S3_PORT", "9000"),
		Region:   "us-east-1", AccessKey: env("NEB_LOCAL_S3_ACCESS_KEY", "nebulous"),
		SecretKey: env("NEB_LOCAL_S3_SECRET_KEY", "nebulous-local-secret"),
		Bucket:    env("NEB_LOCAL_S3_BUCKET", "nebulous"),
	})
	if err != nil {
		return err
	}
	return client.HeadBucket(ctx)
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func parsePort(name, fallback string) (int, error) {
	port, err := strconv.Atoi(env(name, fallback))
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s must be a port from 1 to 65535", name)
	}
	return port, nil
}
