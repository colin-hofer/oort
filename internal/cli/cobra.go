package cli

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/spf13/cobra"
)

var invocationMu sync.Mutex

var commandDescriptions = map[string]string{
	"auth login":               "Sign in through the configured identity provider",
	"auth logout":              "Revoke and remove the active credential",
	"auth token":               "Print the active credential",
	"auth whoami":              "Show the authenticated user",
	"context show":             "Show resolved profile, server, and tenant context",
	"doctor":                   "Check local dependencies and API connectivity",
	"tenant create":            "Create a tenant",
	"tenant list":              "List accessible tenants",
	"tenant use":               "Select the tenant for this project",
	"dataset upload":           "Upload a CSV or Parquet dataset",
	"dataset replace":          "Replace a dataset with a new snapshot",
	"dataset delete":           "Delete a dataset and its history",
	"dataset list":             "List datasets",
	"dataset show":             "Show dataset schema and history",
	"dataset sample":           "Show sample rows from a dataset",
	"query run":                "Execute a query draft",
	"query validate":           "Validate a query file locally",
	"query save":               "Save an immutable query revision",
	"query list":               "List saved queries",
	"query show":               "Show a saved query",
	"query delete":             "Delete a saved query and its revisions",
	"app init":                 "Initialize an app project",
	"app dev":                  "Develop an app with live reload",
	"app codegen":              "Generate TypeScript query contracts",
	"app deploy":               "Deploy the current app",
	"app open":                 "Create a private app login link",
	"app list":                 "List apps",
	"app show":                 "Show an app and its deployments",
	"app delete":               "Delete an app and its deployment history",
	"app deployment list":      "List app deployments",
	"app deployment show":      "Show a deployment",
	"app deployment rollback":  "Roll an app back to a deployment",
	"job list":                 "List background jobs",
	"job show":                 "Show a background job",
	"job wait":                 "Wait for a background job to finish",
	"job logs":                 "Show background job logs",
	"job cancel":               "Cancel a background job",
	"connector create":         "Create a REST/JSON connector",
	"connector list":           "List connectors",
	"connector show":           "Show a connector",
	"connector update":         "Update a connector",
	"connector sync":           "Sync a connector now",
	"connector delete":         "Delete a connector",
	"member list":              "List tenant members",
	"member add":               "Add a tenant member",
	"member update":            "Update a tenant member",
	"member remove":            "Remove a tenant member",
	"member invitation list":   "List pending and expired membership invitations",
	"member invitation renew":  "Renew a membership invitation",
	"member invitation revoke": "Revoke a membership invitation",
	"token list":               "List API tokens",
	"token create":             "Create a scoped API token",
	"token revoke":             "Revoke an API token",
	"platform run":             "Run the local platform",
	"platform dev":             "Develop the platform with hot reload",
	"platform status":          "Show local platform status",
	"platform logs":            "Show local dependency logs",
	"platform stop":            "Stop local platform dependencies",
	"platform reset":           "Delete all local platform data",
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	invocationMu.Lock()
	defer invocationMu.Unlock()
	currentOptions = invocationOptions{}
	root := newRoot(stdout, stderr)
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

func newRoot(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "oort",
		Short:         "Build and operate private data apps",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().StringVar(&currentOptions.Server, "server", "", "Oort API URL")
	root.PersistentFlags().StringVar(&currentOptions.Profile, "profile", "", "configuration profile")
	root.PersistentFlags().StringVar(&currentOptions.Tenant, "tenant", "", "tenant slug")
	root.PersistentFlags().BoolVar(&currentOptions.JSON, "json", false, "emit versioned JSON")

	auth := group("auth", "Authenticate with Oort")
	auth.AddCommand(leaf("login", "auth login", stdout, stderr), leaf("logout", "auth logout", stdout, stderr),
		leaf("token", "auth token", stdout, stderr), leaf("whoami", "auth whoami", stdout, stderr))
	root.AddCommand(auth)

	contextCommand := group("context", "Inspect resolved CLI context")
	contextCommand.AddCommand(leaf("show", "context show", stdout, stderr))
	root.AddCommand(contextCommand)
	root.AddCommand(leaf("doctor", "doctor", stdout, stderr))

	tenant := group("tenant", "Manage tenants and selection")
	tenant.AddCommand(leaf("create <slug> [--use]", "tenant create", stdout, stderr), leaf("list", "tenant list", stdout, stderr),
		leaf("use <slug> [--global]", "tenant use", stdout, stderr))
	root.AddCommand(tenant)

	dataset := group("dataset", "Manage datasets")
	dataset.AddCommand(leaf("upload <file> [--name <slug>] [--detach]", "dataset upload", stdout, stderr),
		leaf("replace <slug> <file> [--detach]", "dataset replace", stdout, stderr),
		leaf("list", "dataset list", stdout, stderr), leaf("show <slug>", "dataset show", stdout, stderr),
		leaf("sample <slug>", "dataset sample", stdout, stderr), leaf("delete <slug>", "dataset delete", stdout, stderr))
	root.AddCommand(dataset)

	query := group("query", "Validate, save, and execute queries")
	query.AddCommand(leaf("run <file> [--param name=value]", "query run", stdout, stderr),
		leaf("validate <file> [--name <slug>] [--param name=value]", "query validate", stdout, stderr),
		leaf("save <file> [--name <slug>] [--param name=value]", "query save", stdout, stderr),
		leaf("list", "query list", stdout, stderr), leaf("show <slug>", "query show", stdout, stderr),
		leaf("delete <slug>", "query delete", stdout, stderr))
	root.AddCommand(query)

	app := group("app", "Build and operate applications")
	app.AddCommand(leaf("init [--name <slug>] [--force]", "app init", stdout, stderr),
		leaf("dev [--listen <address>] [--proxy <url>] [-- command...]", "app dev", stdout, stderr),
		leaf("codegen [--output <file>]", "app codegen", stdout, stderr), leaf("deploy [--detach]", "app deploy", stdout, stderr),
		leaf("open", "app open", stdout, stderr), leaf("list", "app list", stdout, stderr),
		leaf("show <slug>", "app show", stdout, stderr), leaf("delete <slug>", "app delete", stdout, stderr))
	deployment := group("deployment", "Inspect and roll back app deployments")
	deployment.AddCommand(leaf("rollback <id>", "app deployment rollback", stdout, stderr),
		leaf("list [--app <slug>]", "app deployment list", stdout, stderr), leaf("show <id>", "app deployment show", stdout, stderr))
	app.AddCommand(deployment)
	root.AddCommand(app)

	jobs := group("job", "Inspect and control background jobs")
	jobs.AddCommand(leaf("list", "job list", stdout, stderr), leaf("show <id>", "job show", stdout, stderr),
		leaf("wait <id>", "job wait", stdout, stderr), leaf("logs <id>", "job logs", stdout, stderr),
		leaf("cancel <id>", "job cancel", stdout, stderr))
	root.AddCommand(jobs)

	connectors := group("connector", "Manage REST/JSON connectors")
	for _, item := range [][2]string{{"create <slug> --url <url> [--dataset <slug>]", "connector create"}, {"list", "connector list"},
		{"show <slug>", "connector show"}, {"update <slug>", "connector update"},
		{"sync <slug> [--detach]", "connector sync"}, {"delete <slug>", "connector delete"}} {
		connectors.AddCommand(leaf(item[0], item[1], stdout, stderr))
	}
	root.AddCommand(connectors)

	access := group("access", "Manage tenant access")
	members := group("member", "Manage tenant members")
	for _, item := range [][2]string{{"list", "member list"}, {"add <email> --role <role>", "member add"},
		{"update <user-id> --role <role>", "member update"}, {"remove <user-id>", "member remove"}} {
		members.AddCommand(leaf(item[0], item[1], stdout, stderr))
	}
	invitations := group("invitation", "Manage membership invitations")
	for _, item := range [][2]string{{"list", "member invitation list"}, {"renew <id>", "member invitation renew"},
		{"revoke <id>", "member invitation revoke"}} {
		invitations.AddCommand(leaf(item[0], item[1], stdout, stderr))
	}
	members.AddCommand(invitations)
	access.AddCommand(members)

	tokens := group("token", "Manage API tokens")
	for _, item := range [][2]string{{"list", "token list"}, {"create <name> --scope <scope>", "token create"}, {"revoke <id>", "token revoke"}} {
		tokens.AddCommand(leaf(item[0], item[1], stdout, stderr))
	}
	access.AddCommand(tokens)
	root.AddCommand(access)

	platform := group("platform", "Develop the Oort platform locally")
	for _, item := range [][2]string{{"run", "platform run"}, {"dev", "platform dev"}, {"status", "platform status"},
		{"logs [service] [--follow]", "platform logs"}, {"stop", "platform stop"}, {"reset --yes", "platform reset"}} {
		platform.AddCommand(leaf(item[0], item[1], stdout, stderr))
	}
	root.AddCommand(platform)

	completion := &cobra.Command{Use: "completion [bash|zsh|fish|powershell]", Short: "Generate shell completion", Args: cobra.ExactArgs(1)}
	completion.RunE = func(command *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return root.GenBashCompletion(stdout)
		case "zsh":
			return root.GenZshCompletion(stdout)
		case "fish":
			return root.GenFishCompletion(stdout, true)
		case "powershell":
			return root.GenPowerShellCompletion(stdout)
		default:
			return fmt.Errorf("unsupported shell %q", args[0])
		}
	}
	root.AddCommand(completion)
	return root
}

func group(use, short string) *cobra.Command {
	return &cobra.Command{Use: use, Short: short}
}

func leaf(use, path string, stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{Use: use, Short: commandDescriptions[path], DisableFlagParsing: true, Args: cobra.ArbitraryArgs}
	command.RunE = func(command *cobra.Command, args []string) error {
		if helpRequested(args) {
			return command.Help()
		}
		args, jsonOutput, err := trailingGlobals(args)
		if err != nil {
			return err
		}
		return runCommand(command.Context(), path, args, currentOptions.JSON || jsonOutput, stdout, stderr)
	}
	return command
}

func helpRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func trailingGlobals(args []string) ([]string, bool, error) {
	var command []string
	for index, value := range args {
		if value == "--" {
			command, args = append([]string(nil), args[index:]...), args[:index]
			break
		}
	}
	jsonOutput, args := takeFlag(args, "--json")
	server, args, err := takeValueFlag(args, "--server")
	if err != nil {
		return nil, false, err
	}
	profile, args, err := takeValueFlag(args, "--profile")
	if err != nil {
		return nil, false, err
	}
	if server != "" {
		currentOptions.Server = server
	}
	if profile != "" {
		currentOptions.Profile = profile
	}
	tenant, args, err := takeValueFlag(args, "--tenant")
	if err != nil {
		return nil, false, err
	}
	if tenant != "" {
		currentOptions.Tenant = tenant
	}
	return append(args, command...), jsonOutput, nil
}
