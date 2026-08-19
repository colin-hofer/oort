package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTenantCommands(t *testing.T) {
	originalClient := apiClient
	defer func() { apiClient = originalClient }()
	apiClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatal("CLI omitted bearer token")
		}
		var response intString
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tenants":
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input["slug"] != "acme" {
				t.Fatalf("unexpected create body: %v %v", input, err)
			}
			response = intString{http.StatusCreated, `{"tenant":{"id":"tenant-id","slug":"acme","role":"owner","created_at":"2026-08-18T00:00:00Z"}}`}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants":
			response = intString{http.StatusOK, `{"tenants":[{"id":"tenant-id","slug":"acme","role":"owner","created_at":"2026-08-18T00:00:00Z"}]}`}
		default:
			response = intString{http.StatusNotFound, `{}`}
		}
		return &http.Response{StatusCode: response.number, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response.text))}, nil
	})}

	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	stateDir := filepath.Join(stateHome, "oort")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(map[string]string{"api_url": "http://api.test", "token": "test-token"})
	if err := os.WriteFile(filepath.Join(stateDir, "local.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := Run(context.Background(), []string{"tenant", "create", "acme", "--json"}, &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"schema_version":1`) || !strings.Contains(output.String(), `"slug":"acme"`) || strings.Contains(output.String(), `"Slug"`) {
		t.Fatalf("unexpected JSON output: %s", output.String())
	}
	output.Reset()
	if err := Run(context.Background(), []string{"tenant", "list"}, &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "acme\towner\ttenant-id") {
		t.Fatalf("unexpected human output: %s", output.String())
	}
}

func TestAuthTokenAndResourceDeletes(t *testing.T) {
	originalClient := apiClient
	defer func() { apiClient = originalClient }()
	var requested []string
	apiClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Method != http.MethodDelete {
			t.Fatalf("unexpected request: %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		requested = append(requested, r.URL.Path)
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OORT_TOKEN", "test-token")
	t.Setenv("OORT_API_URL", "http://api.test")
	t.Setenv("OORT_TENANT", "acme")

	var output bytes.Buffer
	if err := Run(context.Background(), []string{"auth", "token"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if output.String() != "test-token\n" {
		t.Fatalf("token command returned %q", output.String())
	}

	for _, item := range []struct {
		resource string
		slug     string
		path     string
	}{
		{"dataset", "orders", "/v1/tenants/acme/datasets/orders"},
		{"query", "recent-orders", "/v1/tenants/acme/queries/recent-orders"},
		{"app", "sales", "/v1/tenants/acme/apps/sales"},
	} {
		output.Reset()
		if err := Run(context.Background(), []string{item.resource, "delete", item.slug}, &output, io.Discard); err != nil {
			t.Fatal(err)
		}
		if output.String() != fmt.Sprintf("Deleted %s %s.\n", item.resource, item.slug) {
			t.Fatalf("unexpected delete output for %s: %q", item.path, output.String())
		}
		if requested[len(requested)-1] != item.path {
			t.Fatalf("%s delete used %s", item.resource, requested[len(requested)-1])
		}
	}
}

func TestMemberInvitationCommands(t *testing.T) {
	originalClient := apiClient
	defer func() { apiClient = originalClient }()
	const invitation = `{"outcome":"invitation_created","invitation":{"id":"11111111-1111-4111-8111-111111111111","email":"new@example.com","role":"developer","status":"pending","expires_at":"2026-08-25T00:00:00Z"},"accept_url":"http://127.0.0.1:8080/auth/invitations/link-secret"}`
	apiClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var response intString
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tenants/acme/members":
			response = intString{http.StatusCreated, invitation}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/acme/members/invitations":
			response = intString{http.StatusOK, `{"invitations":[{"id":"11111111-1111-4111-8111-111111111111","email":"new@example.com","role":"developer","status":"pending","expires_at":"2026-08-25T00:00:00Z"}]}`}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/renew"):
			response = intString{http.StatusOK, strings.Replace(invitation, "invitation_created", "invitation_renewed", 1)}
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/members/invitations/"):
			response = intString{http.StatusNoContent, ""}
		default:
			response = intString{http.StatusNotFound, `{}`}
		}
		return &http.Response{StatusCode: response.number, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response.text))}, nil
	})}

	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	stateDir := filepath.Join(stateHome, "oort")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(map[string]string{"api_url": "http://api.test", "token": "test-token"})
	if err := os.WriteFile(filepath.Join(stateDir, "local.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := Run(context.Background(), []string{"access", "member", "add", "new@example.com", "--role", "developer", "--tenant", "acme"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Invitation created") || !strings.Contains(output.String(), "http://127.0.0.1:8080/auth/invitations/link-secret") {
		t.Fatalf("invitation link was not prominent:\n%s", output.String())
	}
	output.Reset()
	if err := Run(context.Background(), []string{"access", "member", "invitation", "list", "--tenant", "acme"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "new@example.com\tdeveloper\tpending") {
		t.Fatalf("unexpected invitation list: %s", output.String())
	}
	output.Reset()
	if err := Run(context.Background(), []string{"access", "member", "invitation", "renew", "11111111-1111-4111-8111-111111111111", "--tenant", "acme", "--json"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"schema_version":1`) || !strings.Contains(output.String(), `"outcome":"invitation_renewed"`) || !strings.Contains(output.String(), `"accept_url"`) {
		t.Fatalf("unexpected renewal JSON: %s", output.String())
	}
	if err := Run(context.Background(), []string{"access", "member", "invitation", "revoke", "11111111-1111-4111-8111-111111111111", "--tenant", "acme"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

type intString struct {
	number int
	text   string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestPortValidation(t *testing.T) {
	t.Setenv("OORT_LOCAL_S3_PORT", "not-a-port")
	if _, err := parsePort("OORT_LOCAL_S3_PORT", "9000"); err == nil {
		t.Fatal("invalid port was accepted")
	}
}

func TestMaterializeCompose(t *testing.T) {
	path, err := materializeCompose(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "postgres:18.4-alpine3.24") {
		t.Fatal("materialized Compose file omitted PostgreSQL")
	}
}

func TestGenerateContract(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	manifest := `{"app":{"slug":"sales","dir":"dist"},"queries":[{"name":"recent-orders","file":"queries/recent.sql","parameters":{"limit":"integer","active":"boolean"}}]}`
	if err := os.WriteFile("oort.json", []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{"app", "codegen", "--output", "contract.ts"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile("contract.ts")
	if err != nil {
		t.Fatal(err)
	}
	generated := string(contents)
	if !strings.Contains(generated, `"recent-orders": { parameters: { "active": boolean; "limit": number; }`) {
		t.Fatalf("unexpected generated contract:\n%s", generated)
	}
}

func TestAppInitGeneratesCSPCompatibleBootstrap(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	if err := Run(context.Background(), []string{"app", "init", "--name", "starter"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile("dist/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(index), `<script type="module">`) || !strings.Contains(string(index), `<script type="module" src="./main.js"></script>`) {
		t.Fatalf("starter index uses an inline script: %s", index)
	}
	main, err := os.ReadFile("dist/main.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), "createClient().query('status')") {
		t.Fatalf("starter bootstrap is incomplete: %s", main)
	}
}

func TestCommandTreeIsAForwardOnlyCutover(t *testing.T) {
	var help bytes.Buffer
	if err := Run(context.Background(), []string{"--help"}, &help, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"app", "platform", "job", "auth", "access"} {
		if !strings.Contains(help.String(), command) {
			t.Fatalf("root help omitted %q:\n%s", command, help.String())
		}
	}
	for _, old := range [][]string{{"deploy"}, {"local", "dev"}, {"run", "list"}, {"login"}, {"member", "list"}} {
		if err := Run(context.Background(), old, io.Discard, io.Discard); err == nil {
			t.Fatalf("old command %q is still accepted", strings.Join(old, " "))
		}
	}
}

func BenchmarkHelp(b *testing.B) {
	for b.Loop() {
		if err := Run(context.Background(), []string{"--help"}, io.Discard, io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}
