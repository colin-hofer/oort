package cli

import (
	"bytes"
	"context"
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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/browser"

	"oort/internal/manifest"
)

var projectSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

func runCommand(ctx context.Context, path string, args []string, jsonOutput bool, stdout, stderr io.Writer) error {
	switch path {
	case "app init":
		return runInit(args, jsonOutput, stdout)
	case "app dev":
		return runAppDev(ctx, args, jsonOutput, stdout, stderr)
	case "app codegen":
		return runGenerate(args, jsonOutput, stdout)
	case "app deploy":
		return runDeploy(ctx, args, jsonOutput, stdout, stderr)
	case "app open":
		return runOpen(ctx, args, jsonOutput, stdout)
	case "app deployment rollback":
		return runDeployment(ctx, append([]string{"rollback"}, args...), jsonOutput, stdout)
	case "tenant create":
		return runTenant(ctx, append([]string{"create"}, args...), jsonOutput, stdout)
	case "tenant list":
		return runTenant(ctx, append([]string{"list"}, args...), jsonOutput, stdout)
	case "dataset upload":
		return runDataset(ctx, append([]string{"upload"}, args...), jsonOutput, stdout, stderr)
	case "dataset replace":
		if len(args) < 2 {
			return fmt.Errorf("usage: oort dataset replace <slug> <file> [--detach]")
		}
		return runDataset(ctx, append([]string{"upload", args[1], "--name", args[0]}, args[2:]...), jsonOutput, stdout, stderr)
	case "query run":
		return runQuery(ctx, append([]string{"run"}, args...), jsonOutput, stdout, stderr)
	case "platform run", "platform dev", "platform status", "platform logs", "platform stop", "platform reset":
		return runPlatform(ctx, append([]string{strings.TrimPrefix(path, "platform ")}, args...), jsonOutput, stdout, stderr)
	case "auth login":
		return runLogin(ctx, args, jsonOutput, stdout, stderr)
	case "auth logout":
		return runLogout(ctx, args, jsonOutput, stdout)
	case "auth token":
		return runRevealToken(args, jsonOutput, stdout)
	case "auth whoami":
		return runSimpleGET(ctx, args, "/v1/me", jsonOutput, stdout)
	case "context show":
		return runContext(args, jsonOutput, stdout)
	case "doctor":
		return runDoctor(ctx, args, jsonOutput, stdout)
	case "tenant use":
		return runTenantUse(args, jsonOutput, stdout)
	case "dataset list", "dataset show", "dataset sample", "dataset delete":
		return runDatasetResource(ctx, path, args, jsonOutput, stdout)
	case "query validate", "query save", "query list", "query show", "query delete":
		return runQueryResource(ctx, path, args, jsonOutput, stdout)
	case "app list", "app show", "app delete", "app deployment list", "app deployment show":
		return runAppResource(ctx, path, args, jsonOutput, stdout)
	case "job list", "job show", "job wait", "job logs", "job cancel":
		return runJobResource(ctx, path, args, jsonOutput, stdout)
	case "connector create", "connector list", "connector show", "connector update", "connector sync", "connector delete":
		return runConnectorResource(ctx, path, args, jsonOutput, stdout, stderr)
	case "member list", "member add", "member update", "member remove":
		return runMemberResource(ctx, path, args, jsonOutput, stdout)
	case "member invitation list", "member invitation renew", "member invitation revoke":
		return runInvitationResource(ctx, path, args, jsonOutput, stdout)
	case "token list", "token create", "token revoke":
		return runTokenResource(ctx, path, args, jsonOutput, stdout, stderr)
	default:
		return fmt.Errorf("command %q is not implemented", path)
	}
}

func runRevealToken(args []string, jsonOutput bool, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: oort auth token")
	}
	state, err := loadState()
	if err != nil {
		return err
	}
	if jsonOutput {
		return emitJSON(stdout, map[string]string{"server": state.APIURL, "token": state.Token})
	}
	_, err = fmt.Fprintln(stdout, state.Token)
	return err
}

func runLogin(ctx context.Context, args []string, jsonOutput bool, stdout, stderr io.Writer) error {
	noOpen, args := takeFlag(args, "--no-open")
	if len(args) != 0 {
		return fmt.Errorf("usage: oort auth login [--server <url>] [--profile <name>] [--no-open]")
	}
	name, selected, config, secrets, _, err := activeProfile()
	if err != nil {
		return err
	}
	serverURL := first(currentOptions.Server, os.Getenv("OORT_SERVER"), selected.Server)
	target, err := url.Parse(serverURL)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return fmt.Errorf("select a server with --server <url>")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start login callback: %w", err)
	}
	defer listener.Close()
	nonce := randomKey()
	callback := "http://" + listener.Addr().String() + "/callback/" + nonce
	loginURL := strings.TrimRight(serverURL, "/") + "/auth/login?" + url.Values{"cli_return": {callback}}.Encode()
	codes := make(chan string, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback/"+nonce || r.URL.Query().Get("code") == "" {
			http.Error(w, "invalid login callback", http.StatusBadRequest)
			return
		}
		select {
		case codes <- r.URL.Query().Get("code"):
		default:
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "Oort login complete. You can close this window.\n")
	})
	callbackServer := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go callbackServer.Serve(listener)
	defer callbackServer.Close()
	if noOpen {
		fmt.Fprintln(stderr, loginURL)
	} else if err := browser.OpenURL(loginURL); err != nil {
		fmt.Fprintf(stderr, "Open this URL to log in:\n%s\n", loginURL)
	}
	loginContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var code string
	select {
	case code = <-codes:
	case <-loginContext.Done():
		return fmt.Errorf("login timed out: %w", loginContext.Err())
	}
	body, _ := json.Marshal(map[string]string{"code": code})
	payload, err := unauthenticatedRequest(loginContext, serverURL, http.MethodPost, "/v1/auth/cli-exchange", body)
	if err != nil {
		return err
	}
	var response struct {
		User  any    `json:"user"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(payload, &response); err != nil || response.Token == "" {
		return fmt.Errorf("decode login response")
	}
	config.ActiveProfile = name
	config.Profiles[name] = profile{Server: strings.TrimRight(serverURL, "/"), DefaultTenant: selected.DefaultTenant}
	secrets.Tokens[name] = response.Token
	if err := saveUserConfig(config, secrets); err != nil {
		return fmt.Errorf("save login: %w", err)
	}
	if jsonOutput {
		return emitJSON(stdout, map[string]any{"profile": name, "server": serverURL, "user": response.User})
	}
	fmt.Fprintf(stdout, "Logged in to %s as profile %s.\n", serverURL, name)
	return nil
}

func runLogout(ctx context.Context, args []string, jsonOutput bool, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: oort auth logout")
	}
	name, selected, config, secrets, _, err := activeProfile()
	if err != nil {
		return err
	}
	if token := secrets.Tokens[name]; token != "" {
		state := localState{APIURL: first(currentOptions.Server, os.Getenv("OORT_SERVER"), selected.Server), Token: token}
		if state.APIURL == "" {
			return fmt.Errorf("profile %s does not have a server URL", name)
		}
		if _, err := apiRequest(ctx, state, http.MethodDelete, "/v1/tokens/current", nil); err != nil {
			return fmt.Errorf("revoke remote token: %w", err)
		}
	}
	delete(secrets.Tokens, name)
	if err := saveUserConfig(config, secrets); err != nil {
		return err
	}
	if jsonOutput {
		return emitJSON(stdout, map[string]any{"profile": name, "logged_out": true})
	}
	fmt.Fprintf(stdout, "Removed credentials for profile %s.\n", name)
	return nil
}

func runContext(args []string, jsonOutput bool, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: oort context show")
	}
	name, selected, _, secrets, project, err := activeProfile()
	if err != nil {
		return err
	}
	result := map[string]any{
		"profile": name, "server": first(currentOptions.Server, os.Getenv("OORT_SERVER"), selected.Server, "http://127.0.0.1:8080"),
		"tenant":        first(currentOptions.Tenant, os.Getenv("OORT_TENANT"), project.Tenant, selected.DefaultTenant),
		"authenticated": os.Getenv("OORT_TOKEN") != "" || secrets.Tokens[name] != "",
		"sources":       map[string]string{"flags": "highest", "environment": "second", "project": "third", "profile": "fourth"},
	}
	if jsonOutput {
		return emitJSON(stdout, result)
	}
	fmt.Fprintf(stdout, "Profile: %s\nServer: %s\nTenant: %s\nAuthenticated: %t\n", result["profile"], result["server"], result["tenant"], result["authenticated"])
	return nil
}

func runTenantUse(args []string, jsonOutput bool, stdout io.Writer) error {
	global, args := takeFlag(args, "--global")
	if len(args) != 1 || !projectSlug.MatchString(args[0]) {
		return fmt.Errorf("usage: oort tenant use <slug> [--global]")
	}
	name, selected, config, secrets, project, err := activeProfile()
	if err != nil {
		return err
	}
	var destination string
	if global {
		selected.DefaultTenant = args[0]
		config.Profiles[name] = selected
		destination = filepath.Join(configDir(), "config.json")
		err = saveUserConfig(config, secrets)
	} else {
		project.Tenant, project.Profile = args[0], name
		destination, err = writeProjectContext(project)
	}
	if err != nil {
		return err
	}
	if jsonOutput {
		return emitJSON(stdout, map[string]any{"tenant": args[0], "profile": name, "file": destination})
	}
	fmt.Fprintf(stdout, "Using tenant %s (%s).\n", args[0], destination)
	return nil
}

func runInit(args []string, jsonOutput bool, stdout io.Writer) error {
	force, args := takeFlag(args, "--force")
	name, args, err := takeValueFlag(args, "--name")
	if err != nil || len(args) != 0 {
		return fmt.Errorf("usage: oort app init [--name <slug>] [--force]")
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	if name == "" {
		name = strings.ToLower(filepath.Base(dir))
	}
	if !projectSlug.MatchString(name) {
		return fmt.Errorf("project name must be a 3-63 character lowercase slug; pass --name")
	}
	files := map[string]string{
		"oort.json":          fmt.Sprintf("{\n  \"app\": {\"slug\": %q, \"dir\": \"dist\"},\n  \"queries\": [{\"name\": \"status\", \"file\": \"queries/status.sql\", \"parameters\": {}}]\n}\n", name),
		"queries/status.sql": "SELECT 'ready' AS status\n",
		"dist/oort-sdk.js":   `export function createClient(baseURL = "") { return { async query(name, parameters = {}) { const response = await fetch(baseURL + "/runtime/v1/queries/" + encodeURIComponent(name), {method:"POST", credentials:"same-origin", headers:{"Content-Type":"application/json"}, body:JSON.stringify({parameters})}); if (!response.ok) throw new Error("Query failed (" + response.status + ")"); return response.json(); } }; }` + "\n",
		"dist/main.js":       `import{createClient}from'./oort-sdk.js';const out=document.querySelector('#result');createClient().query('status').then(value=>out.textContent=JSON.stringify(value,null,2)).catch(error=>out.textContent=error.message);` + "\n",
		"dist/index.html":    `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Oort app</title><style>body{font:16px system-ui;max-width:48rem;margin:4rem auto;padding:0 1rem;color:#18212f}pre{background:#f4f6f8;padding:1rem;border-radius:.5rem}</style><main><h1>Oort app</h1><p>Connected to the private runtime.</p><pre id="result">Loading…</pre></main><script type="module" src="./main.js"></script></html>` + "\n",
	}
	for path := range files {
		if _, err := os.Stat(path); err == nil && !force {
			return fmt.Errorf("%s already exists; rerun with --force to replace generated files", path)
		}
	}
	for path, contents := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			return err
		}
	}
	if contents, err := os.ReadFile(".gitignore"); errors.Is(err, os.ErrNotExist) {
		_ = os.WriteFile(".gitignore", []byte(".oort/\n"), 0o644)
	} else if err == nil && !strings.Contains(string(contents), ".oort/") {
		file, openErr := os.OpenFile(".gitignore", os.O_APPEND|os.O_WRONLY, 0o644)
		if openErr == nil {
			_, _ = io.WriteString(file, "\n.oort/\n")
			_ = file.Close()
		}
	}
	if jsonOutput {
		return emitJSON(stdout, map[string]any{"app": name, "files": len(files)})
	}
	fmt.Fprintf(stdout, "Initialized %s. Run `oort app dev`, then `oort app deploy`.\n", name)
	return nil
}

func runDoctor(ctx context.Context, args []string, jsonOutput bool, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: oort doctor")
	}
	checks := map[string]string{}
	if err := exec.CommandContext(ctx, "docker", "compose", "version").Run(); err != nil {
		checks["docker"] = "unavailable"
	} else {
		checks["docker"] = "ok"
	}
	if _, err := manifest.Load(manifest.FileName); err != nil {
		checks["project"] = err.Error()
	} else {
		checks["project"] = "ok"
	}
	state, err := loadState()
	if err != nil {
		checks["credentials"] = err.Error()
	} else {
		checks["credentials"] = "ok"
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(state.APIURL, "/")+"/healthz", nil)
		if response, err := apiClient.Do(request); err != nil {
			checks["api"] = err.Error()
		} else {
			response.Body.Close()
			checks["api"] = response.Status
		}
	}
	if jsonOutput {
		return emitJSON(stdout, checks)
	}
	for _, name := range []string{"docker", "project", "credentials", "api"} {
		if value := checks[name]; value != "" {
			fmt.Fprintf(stdout, "%s\t%s\n", name, value)
		}
	}
	return nil
}

func runSimpleGET(ctx context.Context, args []string, path string, jsonOutput bool, stdout io.Writer) error {
	if len(args) != 0 {
		return fmt.Errorf("this command takes no arguments")
	}
	state, err := loadState()
	if err != nil {
		return err
	}
	payload, err := apiRequest(ctx, state, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return emitResponse(stdout, payload, jsonOutput)
}

func tenantRequest(ctx context.Context, method, suffix string, body []byte) ([]byte, error) {
	state, err := loadState()
	if err != nil {
		return nil, err
	}
	tenant, err := resolveTenant(ctx, state, "")
	if err != nil {
		return nil, err
	}
	return apiRequest(ctx, state, method, "/v1/tenants/"+url.PathEscape(tenant)+suffix, body)
}

func emitResponse(stdout io.Writer, payload []byte, jsonOutput bool) error {
	if !jsonOutput {
		var value any
		if json.Unmarshal(payload, &value) != nil {
			_, err := stdout.Write(append(payload, '\n'))
			return err
		}
		encoded, _ := json.MarshalIndent(value, "", "  ")
		_, err := fmt.Fprintln(stdout, string(encoded))
		return err
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return err
	}
	return emitJSON(stdout, value)
}

func emitJSON(stdout io.Writer, value any) error {
	return json.NewEncoder(stdout).Encode(map[string]any{"schema_version": 1, "data": value})
}

func unauthenticatedRequest(ctx context.Context, serverURL, method, path string, body []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(serverURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := apiClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("login server returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
	}
	return payload, nil
}

func fileQuery(args []string) (string, string, map[string]any, []string, error) {
	name, args, err := takeValueFlag(args, "--name")
	if err != nil {
		return "", "", nil, nil, err
	}
	parameterValues, args, err := takeValueFlags(args, "--param")
	if err != nil {
		return "", "", nil, nil, err
	}
	if len(args) < 1 {
		return "", "", nil, nil, fmt.Errorf("query file is required")
	}
	contents, err := os.ReadFile(args[0])
	if err != nil {
		return "", "", nil, nil, err
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(args[0]), filepath.Ext(args[0]))
	}
	parameters := map[string]any{}
	for _, item := range parameterValues {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			return "", "", nil, nil, fmt.Errorf("--param must be name=value")
		}
		parameters[key] = parseParameter(value)
	}
	return name, string(contents), parameters, args[1:], nil
}

func intFlag(args []string, name string, fallback int) (int, []string, error) {
	value, rest, err := takeValueFlag(args, name)
	if err != nil || value == "" {
		return fallback, rest, err
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, nil, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, rest, nil
}
