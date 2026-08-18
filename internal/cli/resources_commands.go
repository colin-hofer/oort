package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"nebulous/internal/queryexec"
)

func runDatasetResource(ctx context.Context, path string, args []string, jsonOutput bool, stdout io.Writer) error {
	var endpoint string
	switch path {
	case "dataset list":
		if len(args) != 0 {
			return fmt.Errorf("usage: neb dataset list")
		}
		endpoint = "/datasets"
	case "dataset show", "dataset sample":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb %s <slug>", path)
		}
		endpoint = "/datasets/" + url.PathEscape(args[0])
		if path == "dataset sample" {
			endpoint += "/sample"
		}
	}
	payload, err := tenantRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return emitResponse(stdout, payload, jsonOutput)
}

func runQueryResource(ctx context.Context, path string, args []string, jsonOutput bool, stdout io.Writer) error {
	switch path {
	case "query list":
		if len(args) != 0 {
			return fmt.Errorf("usage: neb query list")
		}
		payload, err := tenantRequest(ctx, http.MethodGet, "/queries", nil)
		if err != nil {
			return err
		}
		return emitResponse(stdout, payload, jsonOutput)
	case "query show":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb query show <slug>")
		}
		payload, err := tenantRequest(ctx, http.MethodGet, "/queries/"+url.PathEscape(args[0]), nil)
		if err != nil {
			return err
		}
		return emitResponse(stdout, payload, jsonOutput)
	case "query validate", "query save":
		name, sqlText, parameters, rest, err := fileQuery(args)
		if err != nil || len(rest) != 0 {
			if err != nil {
				return err
			}
			return fmt.Errorf("usage: neb %s <file> [--name <slug>] [--param name=value]", path)
		}
		cleaned, types, err := queryexec.Validate(sqlText, parameters)
		if err != nil {
			return err
		}
		if path == "query validate" {
			result := map[string]any{"name": name, "sql": cleaned, "parameter_types": types}
			if jsonOutput {
				return emitJSON(stdout, result)
			}
			fmt.Fprintf(stdout, "%s is valid (%d parameters).\n", name, len(types))
			return nil
		}
		body, _ := json.Marshal(map[string]any{"sql": cleaned, "parameter_types": types})
		payload, err := tenantRequest(ctx, http.MethodPut, "/queries/"+url.PathEscape(name), body)
		if err != nil {
			return err
		}
		return emitResponse(stdout, payload, jsonOutput)
	}
	return nil
}

func runAppResource(ctx context.Context, path string, args []string, jsonOutput bool, stdout io.Writer) error {
	endpoint := ""
	switch path {
	case "app list":
		if len(args) != 0 {
			return fmt.Errorf("usage: neb app list")
		}
		endpoint = "/apps"
	case "app show":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb app show <slug>")
		}
		endpoint = "/apps/" + url.PathEscape(args[0])
	case "app deployment list":
		app, rest, err := takeValueFlag(args, "--app")
		if err != nil || len(rest) != 0 {
			return fmt.Errorf("usage: neb app deployment list [--app <slug>]")
		}
		endpoint = "/deployments"
		if app != "" {
			endpoint += "?" + url.Values{"app": {app}}.Encode()
		}
	case "app deployment show":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb app deployment show <id>")
		}
		endpoint = "/deployments/" + url.PathEscape(args[0])
	}
	payload, err := tenantRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return emitResponse(stdout, payload, jsonOutput)
}

func runJobResource(ctx context.Context, path string, args []string, jsonOutput bool, stdout io.Writer) error {
	follow, args := takeFlag(args, "-f")
	followLong, args := takeFlag(args, "--follow")
	follow = follow || followLong
	switch path {
	case "job list":
		if len(args) != 0 || follow {
			return fmt.Errorf("usage: neb job list")
		}
		payload, err := tenantRequest(ctx, http.MethodGet, "/jobs", nil)
		if err != nil {
			return err
		}
		return emitResponse(stdout, payload, jsonOutput)
	case "job show":
		if len(args) != 1 || follow {
			return fmt.Errorf("usage: neb job show <id>")
		}
		payload, err := tenantRequest(ctx, http.MethodGet, "/jobs/"+url.PathEscape(args[0]), nil)
		if err != nil {
			return err
		}
		return emitResponse(stdout, payload, jsonOutput)
	case "job wait":
		if len(args) != 1 || follow {
			return fmt.Errorf("usage: neb job wait <id>")
		}
		payload, err := waitForTenantJob(ctx, args[0])
		if err != nil {
			return err
		}
		return emitResponse(stdout, payload, jsonOutput)
	case "job cancel":
		if len(args) != 1 || follow {
			return fmt.Errorf("usage: neb job cancel <id>")
		}
		payload, err := tenantRequest(ctx, http.MethodPost, "/jobs/"+url.PathEscape(args[0])+"/cancel", []byte("{}"))
		if err != nil {
			return err
		}
		return emitResponse(stdout, payload, jsonOutput)
	case "job logs":
		if len(args) != 1 || (follow && jsonOutput) {
			return fmt.Errorf("usage: neb job logs <id> [-f]; --json cannot be combined with -f")
		}
		after := int64(0)
		for {
			payload, err := tenantRequest(ctx, http.MethodGet, "/jobs/"+url.PathEscape(args[0])+"/logs?after="+fmt.Sprint(after), nil)
			if err != nil {
				return err
			}
			var response struct {
				Logs []struct {
					Sequence int64  `json:"sequence"`
					Level    string `json:"level"`
					Message  string `json:"message"`
				} `json:"logs"`
			}
			if err := json.Unmarshal(payload, &response); err != nil {
				return err
			}
			if jsonOutput {
				return emitResponse(stdout, payload, true)
			}
			for _, log := range response.Logs {
				fmt.Fprintf(stdout, "%s\t%s\n", log.Level, log.Message)
				after = log.Sequence
			}
			if !follow {
				return nil
			}
			jobPayload, err := tenantRequest(ctx, http.MethodGet, "/jobs/"+url.PathEscape(args[0]), nil)
			if err != nil {
				return err
			}
			var current struct {
				Job struct {
					Status string `json:"status"`
				} `json:"job"`
			}
			if err := json.Unmarshal(jobPayload, &current); err != nil {
				return err
			}
			if current.Job.Status == "succeeded" || current.Job.Status == "failed" || current.Job.Status == "cancelled" {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return nil
}

func waitForTenantJob(ctx context.Context, id string) ([]byte, error) {
	for {
		payload, err := tenantRequest(ctx, http.MethodGet, "/jobs/"+url.PathEscape(id), nil)
		if err != nil {
			return nil, err
		}
		var current struct {
			Job struct {
				Status string  `json:"status"`
				Error  *string `json:"error"`
			} `json:"job"`
		}
		if err := json.Unmarshal(payload, &current); err != nil {
			return nil, err
		}
		switch current.Job.Status {
		case "succeeded":
			return payload, nil
		case "failed", "cancelled":
			message := "job " + current.Job.Status
			if current.Job.Error != nil {
				message += ": " + *current.Job.Error
			}
			return nil, fmt.Errorf("%s", message)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func runConnectorResource(ctx context.Context, path string, args []string, jsonOutput bool, stdout, stderr io.Writer) error {
	switch path {
	case "connector list":
		if len(args) != 0 {
			return fmt.Errorf("usage: neb connector list")
		}
		payload, err := tenantRequest(ctx, http.MethodGet, "/connectors", nil)
		if err != nil {
			return err
		}
		return emitResponse(stdout, payload, jsonOutput)
	case "connector show":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb connector show <slug>")
		}
		payload, err := tenantRequest(ctx, http.MethodGet, "/connectors/"+url.PathEscape(args[0]), nil)
		if err != nil {
			return err
		}
		return emitResponse(stdout, payload, jsonOutput)
	case "connector sync", "connector delete":
		detach, args := takeFlag(args, "--detach")
		if len(args) != 1 {
			return fmt.Errorf("usage: neb %s <slug> [--detach]", path)
		}
		method, suffix, body := http.MethodPost, "/sync", []byte("{}")
		if path == "connector delete" {
			if detach {
				return fmt.Errorf("--detach is only valid with neb connector sync")
			}
			method, suffix, body = http.MethodDelete, "", nil
		}
		payload, err := tenantRequest(ctx, method, "/connectors/"+url.PathEscape(args[0])+suffix, body)
		if err != nil {
			return err
		}
		if method == http.MethodDelete {
			if jsonOutput {
				return emitJSON(stdout, map[string]any{"deleted": true, "connector": args[0]})
			}
			fmt.Fprintf(stdout, "Deleted connector %s.\n", args[0])
			return nil
		}
		if !detach {
			var queued struct {
				Job struct {
					ID string `json:"id"`
				} `json:"job"`
			}
			if err := json.Unmarshal(payload, &queued); err != nil || queued.Job.ID == "" {
				return fmt.Errorf("decode connector job")
			}
			payload, err = waitForTenantJob(ctx, queued.Job.ID)
			if err != nil {
				return err
			}
		}
		return emitResponse(stdout, payload, jsonOutput)
	case "connector create", "connector update":
		return writeConnector(ctx, path, args, jsonOutput, stdout, stderr)
	}
	return nil
}

func writeConnector(ctx context.Context, path string, args []string, jsonOutput bool, stdout, stderr io.Writer) error {
	dataset, args, err := takeValueFlag(args, "--dataset")
	if err != nil {
		return err
	}
	endpointURL, args, err := takeValueFlag(args, "--url")
	if err != nil {
		return err
	}
	records, args, _ := takeValueFlag(args, "--records-pointer")
	cursor, args, _ := takeValueFlag(args, "--cursor-param")
	next, args, _ := takeValueFlag(args, "--next-cursor-pointer")
	secretEnv, args, _ := takeValueFlag(args, "--bearer-token-env")
	refreshValue, args, err := takeValueFlag(args, "--refresh-minutes")
	if err != nil {
		return err
	}
	refresh := 60
	if refreshValue != "" {
		refresh, err = strconv.Atoi(refreshValue)
		if err != nil {
			return fmt.Errorf("--refresh-minutes must be an integer")
		}
	}
	disabled, args := takeFlag(args, "--disabled")
	enabledFlag, args := takeFlag(args, "--enabled")
	if disabled && enabledFlag {
		return fmt.Errorf("choose --enabled or --disabled")
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: neb %s <slug> --url <url> [--dataset <slug>] [--records-pointer </path>]", path)
	}
	slug := args[0]
	body := map[string]any{}
	if path == "connector update" {
		payload, err := tenantRequest(ctx, http.MethodGet, "/connectors/"+url.PathEscape(slug), nil)
		if err != nil {
			return err
		}
		var existing struct {
			Connector map[string]any `json:"connector"`
		}
		if err := json.Unmarshal(payload, &existing); err != nil {
			return err
		}
		body = map[string]any{
			"url":                 existing.Connector["url"],
			"records_pointer":     existing.Connector["records_pointer"],
			"cursor_parameter":    existing.Connector["cursor_parameter"],
			"next_cursor_pointer": existing.Connector["next_cursor_pointer"],
			"enabled":             existing.Connector["enabled"],
			"refresh_minutes":     existing.Connector["refresh_minutes"],
		}
		if refreshValue == "" {
			if current, ok := existing.Connector["refresh_minutes"].(float64); ok {
				refresh = int(current)
			}
		}
	}
	if dataset == "" {
		dataset = slug
	}
	if endpointURL == "" {
		endpointURL, _ = body["url"].(string)
	}
	if records == "" {
		records, _ = body["records_pointer"].(string)
	}
	body["slug"], body["dataset"], body["url"], body["records_pointer"] = slug, dataset, endpointURL, records
	body["refresh_minutes"] = refresh
	if disabled || enabledFlag {
		body["enabled"] = enabledFlag
	} else if _, ok := body["enabled"]; !ok {
		body["enabled"] = true
	}
	if cursor != "" || next != "" {
		body["cursor_parameter"], body["next_cursor_pointer"] = cursor, next
	}
	if secretEnv != "" {
		secret := os.Getenv(secretEnv)
		if secret == "" {
			return fmt.Errorf("%s is empty", secretEnv)
		}
		body["bearer_token"] = secret
		fmt.Fprintln(stderr, "Bearer token read from environment and will not be shown again.")
	}
	encoded, _ := json.Marshal(body)
	method, endpoint := http.MethodPost, "/connectors"
	if path == "connector update" {
		method, endpoint = http.MethodPut, "/connectors/"+url.PathEscape(slug)
	}
	payload, err := tenantRequest(ctx, method, endpoint, encoded)
	if err != nil {
		return err
	}
	return emitResponse(stdout, payload, jsonOutput)
}

func runMemberResource(ctx context.Context, path string, args []string, jsonOutput bool, stdout io.Writer) error {
	role, args, err := takeValueFlag(args, "--role")
	if err != nil {
		return err
	}
	method, endpoint := http.MethodGet, "/members"
	var body []byte
	switch path {
	case "member list":
		if len(args) != 0 {
			return fmt.Errorf("usage: neb access member list")
		}
	case "member add":
		if len(args) != 1 || role == "" {
			return fmt.Errorf("usage: neb access member add <email> --role <role>")
		}
		method = http.MethodPost
		body, _ = json.Marshal(map[string]string{"email": args[0], "role": role})
	case "member update":
		if len(args) != 1 || role == "" {
			return fmt.Errorf("usage: neb access member update <user-id> --role <role>")
		}
		method, endpoint = http.MethodPatch, "/members/"+url.PathEscape(args[0])
		body, _ = json.Marshal(map[string]string{"role": role})
	case "member remove":
		if len(args) != 1 || role != "" {
			return fmt.Errorf("usage: neb access member remove <user-id>")
		}
		method, endpoint = http.MethodDelete, "/members/"+url.PathEscape(args[0])
	}
	payload, err := tenantRequest(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if method == http.MethodDelete && len(payload) == 0 {
		if jsonOutput {
			return emitJSON(stdout, map[string]bool{"removed": true})
		}
		fmt.Fprintln(stdout, "Member removed.")
		return nil
	}
	return emitResponse(stdout, payload, jsonOutput)
}

func runTokenResource(ctx context.Context, path string, args []string, jsonOutput bool, stdout, stderr io.Writer) error {
	switch path {
	case "token list":
		if len(args) != 0 {
			return fmt.Errorf("usage: neb access token list")
		}
		payload, err := tenantRequest(ctx, http.MethodGet, "/tokens", nil)
		if err != nil {
			return err
		}
		return emitResponse(stdout, payload, jsonOutput)
	case "token revoke":
		if len(args) != 1 {
			return fmt.Errorf("usage: neb access token revoke <id>")
		}
		_, err := tenantRequest(ctx, http.MethodDelete, "/tokens/"+url.PathEscape(args[0]), nil)
		if err != nil {
			return err
		}
		if jsonOutput {
			return emitJSON(stdout, map[string]bool{"revoked": true})
		}
		fmt.Fprintln(stdout, "Token revoked.")
		return nil
	case "token create":
		scopes, args, err := takeValueFlags(args, "--scope")
		if err != nil {
			return err
		}
		days, args, err := intFlag(args, "--expires-in-days", 30)
		if err != nil || len(args) != 1 || len(scopes) == 0 {
			return fmt.Errorf("usage: neb access token create <name> --scope <scope> [--scope <scope>] [--expires-in-days <days>]")
		}
		body, _ := json.Marshal(map[string]any{"name": args[0], "scopes": scopes, "expires_in_days": days})
		payload, err := tenantRequest(ctx, http.MethodPost, "/tokens", body)
		if err != nil {
			return err
		}
		if !jsonOutput {
			fmt.Fprintln(stderr, "Copy the token secret now; it will not be shown again.")
		}
		return emitResponse(stdout, payload, jsonOutput)
	}
	return nil
}
