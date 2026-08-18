package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	stateDir := filepath.Join(stateHome, "nebulous")
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

type intString struct {
	number int
	text   string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestPortValidation(t *testing.T) {
	t.Setenv("NEB_LOCAL_S3_PORT", "not-a-port")
	if _, err := parsePort("NEB_LOCAL_S3_PORT", "9000"); err == nil {
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
	if err := os.WriteFile("nebulous.json", []byte(manifest), 0o600); err != nil {
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
