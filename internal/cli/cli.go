package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
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

func runDataset(ctx context.Context, args []string, jsonOutput bool, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "upload" {
		return fmt.Errorf("usage: neb dataset upload <file.csv|file.parquet> [--name <slug>] [--tenant <slug>] [--detach]")
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
	detach, args := takeFlag(args, "--detach")
	if len(args) != 2 || args[0] != "upload" {
		return fmt.Errorf("usage: neb dataset upload <file.csv|file.parquet> [--name <slug>] [--tenant <slug>] [--detach]")
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
		Upload struct {
			ID string `json:"id"`
		} `json:"upload"`
		Job     job `json:"job"`
		Content *struct {
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		} `json:"content,omitempty"`
	}
	if err := json.Unmarshal(response, &created); err != nil {
		return fmt.Errorf("decode server response: %w", err)
	}
	if created.Content != nil {
		fmt.Fprintf(stderr, "Uploading %s (%d bytes)...\n", file, info.Size())
		if err := putFile(ctx, created.Content.URL, created.Content.Headers, file, info.Size()); err != nil {
			return err
		}
		response, err = apiRequest(ctx, state, http.MethodPost,
			"/v1/tenants/"+url.PathEscape(tenantSlug)+"/dataset-uploads/"+url.PathEscape(created.Upload.ID)+"/complete", []byte("{}"))
		if err != nil {
			return err
		}
		var completedUpload struct {
			Job job `json:"job"`
		}
		if err := json.Unmarshal(response, &completedUpload); err != nil {
			return fmt.Errorf("decode queued upload: %w", err)
		}
		created.Job = completedUpload.Job
	}
	if detach {
		result := map[string]any{"dataset": created.Dataset, "job": created.Job}
		if jsonOutput {
			return emitJSON(stdout, result)
		}
		fmt.Fprintf(stdout, "Queued upload of %s to %s (job %s).\n", file, created.Dataset.Slug, created.Job.ID)
		return nil
	}
	completed, err := waitForJob(ctx, state, tenantSlug, created.Job.ID)
	if err != nil {
		return err
	}
	result := map[string]any{"dataset": created.Dataset, "job": completed}
	if jsonOutput {
		return emitJSON(stdout, result)
	}
	fmt.Fprintf(stdout, "Uploaded %s to %s (%d rows, snapshot %d).\n", file, created.Dataset.Slug, valueOrZero(completed.RowCount), valueOrZero(completed.SnapshotID))
	fmt.Fprintf(stdout, "Query it with: neb query run <file.sql> --tenant %s\n", tenantSlug)
	return nil
}

type job struct {
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
	body, _ := json.Marshal(map[string]any{"sql": string(contents), "parameters": parameters})
	response, err := apiRequest(ctx, state, http.MethodPost,
		"/v1/tenants/"+url.PathEscape(tenantSlug)+"/queries/execute", body)
	if err != nil {
		return err
	}
	if jsonOutput {
		return emitResponse(stdout, response, true)
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
		use, rest := takeFlag(args[1:], "--use")
		args = append(args[:1], rest...)
		if len(args) != 2 {
			return fmt.Errorf("usage: neb tenant create <slug> [--use]")
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
		if use {
			if err := runTenantUse([]string{result.Tenant.Slug}, false, io.Discard); err != nil {
				return fmt.Errorf("tenant was created but context could not be updated: %w", err)
			}
		}
		if jsonOutput {
			return emitJSON(stdout, map[string]any{"tenant": result.Tenant, "used": use})
		}
		fmt.Fprintf(stdout, "Created tenant %s (%s)\n", result.Tenant.Slug, result.Tenant.ID)
		if use {
			fmt.Fprintf(stdout, "Using tenant %s for this project.\n", result.Tenant.Slug)
		}
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
			return emitJSON(stdout, result)
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

func runPlatform(ctx context.Context, args []string, jsonOutput bool, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: neb platform run|dev|status|logs|stop|reset")
	}
	root, compose, err := findCompose()
	if err != nil {
		return err
	}
	switch args[0] {
	case "run":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb platform run")
		}
		if err := localUp(ctx, root, compose, stderr); err != nil {
			return err
		}
		return localRun(ctx, root, stdout, stderr)
	case "dev":
		if len(args) != 1 || jsonOutput {
			return fmt.Errorf("usage: neb platform dev")
		}
		if err := localUp(ctx, root, compose, stderr); err != nil {
			return err
		}
		return localDev(ctx, root, stdout, stderr)
	case "status":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb platform status [--json]")
		}
		return localStatus(ctx, root, compose, jsonOutput, stdout, stderr)
	case "logs":
		follow, rest := takeFlag(args[1:], "-f")
		if len(rest) > 1 {
			return fmt.Errorf("usage: neb platform logs [service] [-f]")
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
			return emitJSON(stdout, map[string]string{"logs": string(output)})
		}
		return composeCommand(ctx, root, compose, stdout, stderr, commandArgs...).Run()
	case "stop":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb platform stop [--json]")
		}
		if err := composeCommand(ctx, root, compose, stderr, stderr, "down").Run(); err != nil {
			return err
		}
		if jsonOutput {
			return emitJSON(stdout, map[string]bool{"stopped": true})
		}
		fmt.Fprintln(stdout, "Local dependencies stopped; data was preserved.")
	case "reset":
		yes, rest := takeFlag(args[1:], "--yes")
		if !yes || len(rest) != 0 {
			return fmt.Errorf("reset deletes local data; rerun as neb platform reset --yes")
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
			return emitJSON(stdout, map[string]bool{"reset": true})
		}
		fmt.Fprintln(stdout, "Local data and identity were deleted.")
	default:
		return fmt.Errorf("unknown platform command %q", args[0])
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
	var command *exec.Cmd
	if info, err := os.Stat(platform); err == nil && info.Mode().IsRegular() {
		command = exec.Command(platform, "local", "--state-dir", defaultStateDir())
	} else {
		command = exec.Command("go", "run", "./cmd/nebulous", "local", "--state-dir", defaultStateDir())
	}
	return runChildren(ctx, root, stdout, stderr, command)
}

func localDev(ctx context.Context, root string, stdout, stderr io.Writer) error {
	web := filepath.Join(root, "internal", "server", "web")
	if _, err := os.Stat(filepath.Join(web, "node_modules")); err != nil {
		return fmt.Errorf("dashboard packages are missing; run `npm --prefix internal/server/web install`")
	}
	air := exec.Command("go", "run", "github.com/air-verse/air@v1.67.4", "-c", ".air.toml")
	vite := exec.Command("npm", "--prefix", web, "run", "dev", "--", "--host", "127.0.0.1")
	vite.Env = append(os.Environ(), "NEB_LOCAL_STATE_FILE="+filepath.Join(defaultStateDir(), "local.json"))
	fmt.Fprintln(stdout, "Dashboard: http://127.0.0.1:5173")
	return runChildren(ctx, root, stdout, stderr, air, vite)
}

func runChildren(ctx context.Context, root string, stdout, stderr io.Writer, commands ...*exec.Cmd) error {
	for _, command := range commands {
		command.Dir = root
		if command.Env == nil {
			command.Env = os.Environ()
		}
		command.Stdout, command.Stderr = stdout, stderr
		if err := command.Start(); err != nil {
			for _, started := range commands {
				if started.Process != nil {
					_ = started.Process.Kill()
				}
			}
			return fmt.Errorf("start development process: %w", err)
		}
	}
	done := make(chan error, len(commands))
	for _, command := range commands {
		go func(command *exec.Cmd) { done <- command.Wait() }(command)
	}
	var first error
	cancelled := false
	select {
	case first = <-done:
	case <-ctx.Done():
		cancelled = true
	}
	for _, command := range commands {
		if command.ProcessState == nil {
			_ = command.Process.Signal(os.Interrupt)
		}
	}
	remaining := len(commands) - 1
	if cancelled {
		remaining = len(commands)
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for remaining > 0 {
		select {
		case err := <-done:
			remaining--
			if !cancelled && first == nil && err != nil {
				first = err
			}
		case <-timer.C:
			for _, command := range commands {
				if command.ProcessState == nil {
					_ = command.Process.Kill()
				}
			}
			if cancelled {
				return nil
			}
			return first
		}
	}
	if cancelled {
		return nil
	}
	return first
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
	services := []map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var service map[string]any
		if err := decoder.Decode(&service); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode platform status: %w", err)
		}
		services = append(services, map[string]any{
			"name": service["Name"], "service": service["Service"], "state": service["State"],
			"status": service["Status"], "health": service["Health"], "publishers": service["Publishers"],
		})
	}
	result := map[string]any{"services": services, "endpoints": localEndpoints()}
	if state, err := readLocalState(); err == nil {
		result["identity"] = state.User
	}
	return emitJSON(stdout, result)
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
		return nil, fmt.Errorf("contact %s: %w; start it with `neb platform run`", state.APIURL, err)
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
		_, selected, _, _, _, err := activeProfile()
		if err != nil {
			return localState{}, err
		}
		return localState{APIURL: first(currentOptions.Server, os.Getenv("NEB_SERVER"), os.Getenv("NEB_API_URL"), selected.Server, "http://127.0.0.1:8080"), Token: token}, nil
	}
	name, selected, _, secrets, _, err := activeProfile()
	if err != nil {
		return localState{}, err
	}
	apiURL := first(currentOptions.Server, os.Getenv("NEB_SERVER"), os.Getenv("NEB_API_URL"), selected.Server)
	if token := secrets.Tokens[name]; token != "" {
		return localState{APIURL: first(apiURL, "http://127.0.0.1:8080"), Token: token}, nil
	}
	local, localErr := readLocalState()
	if localErr == nil && (apiURL == "" || strings.TrimRight(apiURL, "/") == strings.TrimRight(local.APIURL, "/")) {
		return local, nil
	}
	return localState{}, fmt.Errorf("no credentials for profile %q; run `neb auth login --server %s`", name, first(apiURL, "<url>"))
}

func readLocalState() (localState, error) {
	path := filepath.Join(defaultStateDir(), "local.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		return localState{}, fmt.Errorf("read local identity: %w; run `neb platform run`", err)
	}
	var state localState
	if err := json.Unmarshal(contents, &state); err != nil {
		return state, fmt.Errorf("decode local identity: %w", err)
	}
	if state.APIURL == "" || state.Token == "" {
		return state, fmt.Errorf("local identity is incomplete; rerun `neb platform run`")
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
		requested = first(currentOptions.Tenant, os.Getenv("NEB_TENANT"))
	}
	if requested == "" {
		_, selected, _, _, project, err := activeProfile()
		if err != nil {
			return "", err
		}
		requested = first(project.Tenant, selected.DefaultTenant)
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

func waitForJob(ctx context.Context, state localState, tenantSlug, jobID string) (job, error) {
	path := "/v1/tenants/" + url.PathEscape(tenantSlug) + "/jobs/" + url.PathEscape(jobID)
	for {
		response, err := apiRequest(ctx, state, http.MethodGet, path, nil)
		if err != nil {
			return job{}, err
		}
		var body struct {
			Job job `json:"job"`
		}
		if err := json.Unmarshal(response, &body); err != nil {
			return job{}, fmt.Errorf("decode job: %w", err)
		}
		switch body.Job.Status {
		case "succeeded":
			return body.Job, nil
		case "failed":
			if body.Job.Error != nil {
				return job{}, fmt.Errorf("job failed: %s", *body.Job.Error)
			}
			return job{}, fmt.Errorf("job failed")
		case "cancelled":
			return job{}, fmt.Errorf("job was cancelled")
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return job{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func parseParameter(value string) any {
	if boolean, err := strconv.ParseBool(value); err == nil && (value == "true" || value == "false") {
		return boolean
	}
	if integer, err := strconv.ParseInt(value, 10, 64); err == nil {
		return json.Number(strconv.FormatInt(integer, 10))
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return json.Number(value)
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
