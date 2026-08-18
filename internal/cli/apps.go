package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"oort/internal/manifest"
)

type deployment struct {
	ID         string  `json:"id"`
	AppSlug    string  `json:"app_slug"`
	PreviousID *string `json:"previous_deployment_id,omitempty"`
	Version    int     `json:"version"`
	Status     string  `json:"status"`
	ByteCount  int64   `json:"byte_count"`
	Error      *string `json:"error,omitempty"`
}

func runDeploy(ctx context.Context, args []string, jsonOutput bool, stdout, stderr io.Writer) error {
	detach, args := takeFlag(args, "--detach")
	tenantSlug, args, err := takeValueFlag(args, "--tenant")
	if err != nil {
		return err
	}
	idempotencyKey, args, err := takeValueFlag(args, "--idempotency-key")
	if err != nil {
		return err
	}
	if len(args) != 0 {
		return fmt.Errorf("usage: oort app deploy [--tenant <slug>] [--detach]")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	m, err := manifest.Load(filepath.Join(root, manifest.FileName))
	if err != nil {
		return err
	}
	bundle, err := os.CreateTemp("", "oort-bundle-*.zip")
	if err != nil {
		return fmt.Errorf("create app bundle: %w", err)
	}
	defer os.Remove(bundle.Name())
	defer bundle.Close()
	if err := manifest.BuildBundle(root, m, bundle); err != nil {
		return err
	}
	info, err := bundle.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 1 || info.Size() > manifest.MaxBundleBytes {
		return fmt.Errorf("compressed app bundle must be between 1 byte and 25 MiB")
	}
	if _, err := bundle.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, bundle); err != nil {
		return fmt.Errorf("checksum app bundle: %w", err)
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
	manifestJSON, _ := json.Marshal(m)
	body, _ := json.Marshal(map[string]any{
		"manifest": json.RawMessage(manifestJSON), "checksum": hex.EncodeToString(hash.Sum(nil)),
		"byte_count": info.Size(), "idempotency_key": idempotencyKey,
	})
	response, err := apiRequest(ctx, state, http.MethodPost,
		"/v1/tenants/"+url.PathEscape(tenantSlug)+"/deployments", body)
	if err != nil {
		return err
	}
	var created struct {
		Deployment deployment `json:"deployment"`
		Job        job        `json:"job"`
		Upload     *struct {
			URL     string      `json:"url"`
			Headers http.Header `json:"headers"`
		} `json:"upload,omitempty"`
	}
	if err := json.Unmarshal(response, &created); err != nil {
		return fmt.Errorf("decode deployment response: %w", err)
	}
	if created.Upload != nil {
		fmt.Fprintf(stderr, "Uploading %s version %d (%d bytes)...\n", m.App.Slug, created.Deployment.Version, info.Size())
		if _, err := bundle.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := putReader(ctx, created.Upload.URL, created.Upload.Headers, bundle, info.Size()); err != nil {
			return err
		}
		completed, err := apiRequest(ctx, state, http.MethodPost, "/v1/tenants/"+url.PathEscape(tenantSlug)+
			"/deployments/"+url.PathEscape(created.Deployment.ID)+"/complete", []byte("{}"))
		if err != nil {
			return err
		}
		var queued struct {
			Job job `json:"job"`
		}
		if err := json.Unmarshal(completed, &queued); err != nil {
			return fmt.Errorf("decode deployment job: %w", err)
		}
		created.Job = queued.Job
	}
	if detach {
		result := map[string]any{"deployment": created.Deployment, "job": created.Job}
		if jsonOutput {
			return emitJSON(stdout, result)
		}
		fmt.Fprintf(stdout, "Queued %s version %d (job %s).\n", m.App.Slug, created.Deployment.Version, created.Job.ID)
		return nil
	}
	completedJob, err := waitForJob(ctx, state, tenantSlug, created.Job.ID)
	if err != nil {
		return err
	}
	published, err := waitForDeployment(ctx, state, tenantSlug, created.Deployment.ID)
	if err != nil {
		return err
	}
	appURL, err := createLoginLink(ctx, state, tenantSlug, m.App.Slug)
	if err != nil {
		return err
	}
	rollback := ""
	if published.PreviousID != nil {
		rollback = "oort app deployment rollback " + *published.PreviousID + " --tenant " + tenantSlug
	}
	result := map[string]any{"deployment": published, "job": completedJob, "app_url": appURL, "rollback_command": rollback}
	if jsonOutput {
		return emitJSON(stdout, result)
	}
	fmt.Fprintf(stdout, "Deployed %s version %d.\n%s\n", m.App.Slug, published.Version, appURL)
	if rollback != "" {
		fmt.Fprintf(stdout, "Rollback: %s\n", rollback)
	} else {
		fmt.Fprintln(stdout, "Rollback: no previous release")
	}
	return nil
}

func runOpen(ctx context.Context, args []string, jsonOutput bool, stdout io.Writer) error {
	tenantSlug, args, err := takeValueFlag(args, "--tenant")
	if err != nil {
		return err
	}
	if len(args) != 0 {
		return fmt.Errorf("usage: oort app open [--tenant <slug>]")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	m, err := manifest.Load(filepath.Join(root, manifest.FileName))
	if err != nil {
		return err
	}
	state, err := loadState()
	if err != nil {
		return err
	}
	tenantSlug, err = resolveTenant(ctx, state, tenantSlug)
	if err != nil {
		return err
	}
	appURL, err := createLoginLink(ctx, state, tenantSlug, m.App.Slug)
	if err != nil {
		return err
	}
	if jsonOutput {
		return emitJSON(stdout, map[string]string{"url": appURL})
	}
	fmt.Fprintln(stdout, appURL)
	return nil
}

func runDeployment(ctx context.Context, args []string, jsonOutput bool, stdout io.Writer) error {
	tenantSlug, args, err := takeValueFlag(args, "--tenant")
	if err != nil {
		return err
	}
	if len(args) != 2 || args[0] != "rollback" {
		return fmt.Errorf("usage: oort app deployment rollback <deployment-id> [--tenant <slug>]")
	}
	state, err := loadState()
	if err != nil {
		return err
	}
	tenantSlug, err = resolveTenant(ctx, state, tenantSlug)
	if err != nil {
		return err
	}
	response, err := apiRequest(ctx, state, http.MethodPost, "/v1/tenants/"+url.PathEscape(tenantSlug)+
		"/deployments/"+url.PathEscape(args[1])+"/rollback", []byte("{}"))
	if err != nil {
		return err
	}
	if jsonOutput {
		return emitResponse(stdout, response, true)
	}
	var result struct {
		Deployment deployment `json:"deployment"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Rolled %s back to version %d.\nOpen it with: oort app open --tenant %s\n",
		result.Deployment.AppSlug, result.Deployment.Version, tenantSlug)
	return nil
}

func runGenerate(args []string, jsonOutput bool, stdout io.Writer) error {
	output, args, err := takeValueFlag(args, "--output")
	if err != nil {
		return err
	}
	if len(args) != 0 {
		return fmt.Errorf("usage: oort app codegen [--output <file>]")
	}
	if output == "" {
		output = "oort.generated.ts"
	}
	m, err := manifest.Load(manifest.FileName)
	if err != nil {
		return err
	}
	contents := generateTypeScript(m)
	if err := os.WriteFile(output, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write generated contract: %w", err)
	}
	if jsonOutput {
		return emitJSON(stdout, map[string]any{"file": output, "queries": len(m.Queries)})
	}
	fmt.Fprintf(stdout, "Generated %s for %d queries.\n", output, len(m.Queries))
	return nil
}

func generateTypeScript(m manifest.Manifest) string {
	queries := append([]manifest.Query(nil), m.Queries...)
	sort.Slice(queries, func(i, j int) bool { return queries[i].Name < queries[j].Name })
	var output strings.Builder
	output.WriteString("// Generated by oort app codegen. Do not edit.\n\n")
	output.WriteString("export interface OortQueries {\n")
	for _, query := range queries {
		output.WriteString("  ")
		output.WriteString(strconv.Quote(query.Name))
		output.WriteString(": { parameters: {")
		names := make([]string, 0, len(query.Parameters))
		for name := range query.Parameters {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			output.WriteString(" ")
			output.WriteString(strconv.Quote(name))
			output.WriteString(": ")
			output.WriteString(typeScriptType(query.Parameters[name]))
			output.WriteString(";")
		}
		output.WriteString(" }; rows: unknown[][] };\n")
	}
	output.WriteString("}\n\nexport type OortQueryName = keyof OortQueries;\n")
	return output.String()
}

func typeScriptType(kind string) string {
	if kind == "boolean" {
		return "boolean"
	}
	if kind == "string" {
		return "string"
	}
	return "number"
}

func createLoginLink(ctx context.Context, state localState, tenantSlug, appSlug string) (string, error) {
	response, err := apiRequest(ctx, state, http.MethodPost, "/v1/tenants/"+url.PathEscape(tenantSlug)+
		"/apps/"+url.PathEscape(appSlug)+"/login-link", []byte("{}"))
	if err != nil {
		return "", err
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(response, &result); err != nil || result.URL == "" {
		return "", fmt.Errorf("decode app login link")
	}
	return result.URL, nil
}

func waitForDeployment(ctx context.Context, state localState, tenantSlug, deploymentID string) (deployment, error) {
	endpoint := "/v1/tenants/" + url.PathEscape(tenantSlug) + "/deployments/" + url.PathEscape(deploymentID)
	for {
		response, err := apiRequest(ctx, state, http.MethodGet, endpoint, nil)
		if err != nil {
			return deployment{}, err
		}
		var result struct {
			Deployment deployment `json:"deployment"`
		}
		if err := json.Unmarshal(response, &result); err != nil {
			return deployment{}, fmt.Errorf("decode deployment status: %w", err)
		}
		switch result.Deployment.Status {
		case "succeeded":
			return result.Deployment, nil
		case "failed":
			if result.Deployment.Error != nil {
				return deployment{}, fmt.Errorf("app publish failed: %s", *result.Deployment.Error)
			}
			return deployment{}, fmt.Errorf("app publish failed")
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return deployment{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func putReader(ctx context.Context, target string, headers http.Header, reader io.Reader, size int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, target, reader)
	if err != nil {
		return fmt.Errorf("create app upload request: %w", err)
	}
	request.ContentLength = size
	request.Header = headers.Clone()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("upload app bundle: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("upload app bundle: object storage returned HTTP %d", response.StatusCode)
	}
	return nil
}
