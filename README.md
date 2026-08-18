# Nebulous

Nebulous is a multi-tenant platform for turning uploaded or connected data into
small private web apps. A developer supplies data, defines constrained queries,
and deploys a static frontend; Nebulous owns storage, query execution,
authorization, releases, and operations.

The product is a data-app platform, not a general-purpose PaaS. It hosts static
frontends and managed data operations. It does not run arbitrary application
backends.

## Design intent

Nebulous should let a developer or coding agent go from a data source to a
working app without operating a database or backend:

1. Authenticate and select a tenant.
2. Upload data or configure a connector.
3. Inspect the resulting dataset and test a typed query.
4. Declare the queries an app may use.
5. Deploy an immutable frontend release.
6. Open the private app and query data through the browser SDK.

The CLI, public API, dashboard, and SDK are different clients of the same
platform behavior. No workflow should exist only in the dashboard, and an
agent integration should wrap the public API or structured CLI output instead
of introducing another implementation.

Product decisions should favor:

- a complete path over broad but disconnected features;
- fast local feedback and actionable errors;
- explicit tenant and environment context;
- idempotent, inspectable, and reversible mutations;
- stable machine-readable output alongside concise human output;
- secure defaults and hard tenant boundaries;
- accessible, keyboard-friendly management surfaces;
- measured performance before caches, pools, or distributed services.

## Current foundation

The repository already provides a working local vertical slice:

- loopback-only local authentication plus production OIDC login, browser
  sessions, and one-time CLI login exchange;
- owner, admin, developer, and viewer roles with member and scoped-token
  management;
- CSV and Parquet upload through S3-compatible object storage;
- scheduled REST/JSON connectors with encrypted bearer secrets, bounded
  pagination, restricted egress, retries, and atomic publication;
- PostgreSQL-leased import, connector, and publish jobs with heartbeats,
  cancellation, and inspectable logs;
- tenant-isolated DuckLake catalogs and atomic dataset snapshots;
- saved, typed, read-only queries executed in bounded child processes;
- immutable app bundles, deployment promotion, and rollback;
- private app origins with single-use login codes and host-only sessions;
- deployment-pinned query grants and a small browser query SDK;
- an embedded control-plane dashboard for datasets, query authoring,
  connectors, apps, jobs, access, and audit activity;
- a Cobra-based `neb` CLI covering authentication, context, initialization,
  resource inspection, connectors, jobs, access, local services, application
  development, deployment, opening, and rollback;
- separate hot loops for platform development (`neb platform dev`) and app
  development (`neb app dev`).

Non-local startup fails closed unless OIDC and the public control-plane URL are
configured.

## Install from source

Install the self-contained `neb` binary from the repository root:

```text
go install ./cmd/neb
```

Add `$(go env GOPATH)/bin` to `PATH` if it is not already there. The binary
contains the CLI, platform runtime, dashboard, and local Compose definition, so
`neb platform run` works outside a source checkout.

## Intended capabilities

### Identity and tenancy

- Production login, logout, session, and headless/device authentication.
- Owner, admin, developer, and viewer roles.
- Member management and explicitly scoped, expiring API tokens.
- Tenant context resolution from flags, environment, project context, then
  user configuration, without silently guessing across tenants.
- Audit events for every mutation and secret use.

### Data and connectors

- Dataset listing, schema, sample rows, freshness, lineage, and sync history.
- CSV and Parquet uploads with bounded size and idempotency.
- One built-in REST/JSON pull connector with secret references, pagination,
  cursors, retries, restricted egress, and scheduled syncs.
- Atomic publication: a failed or cancelled sync never replaces the last good
  snapshot.
- Conservative schema-drift failures with useful remediation diagnostics.

A connector is configuration that produces datasets; it is not itself a
queryable data source. Do not add a plugin system before a second connector
proves a shared interface is useful.

### Queries

- Locally validated, saved `SELECT` queries with immutable revisions.
- Declared parameter names and types, bound through prepared statements.
- Draft validation and execution for developers through the same isolated path
  used by deployed apps.
- Bounded JSON results with columns, rows, snapshot IDs, and truncation state.
- Query inspection, diagnostics, cancellation, and stable CLI/API errors.

Browser apps invoke named queries only. They never submit arbitrary SQL or
receive catalog or object-store credentials.

### Apps and deployments

- Static frontend assets plus declared query files in `nebulous.json`.
- Deterministic, path-safe bundles stored as immutable objects.
- Releases that pin exact query revisions.
- One current-deployment pointer per app; promotion and rollback move only that
  pointer.
- Dedicated app origins, private runtime sessions, strict host validation, and
  hardened static response headers.
- Release history, inspection, logs, and copy-pasteable rollback commands.

The minimal manifest remains:

```json
{
  "app": { "slug": "sales", "dir": "dist" },
  "queries": [
    {
      "name": "recent-orders",
      "file": "queries/recent-orders.sql",
      "parameters": { "limit": "integer" }
    }
  ]
}
```

At runtime an app calls a named grant:

```text
POST /runtime/v1/queries/recent-orders
Content-Type: application/json

{ "parameters": { "limit": 50 } }
```

### Developer and agent workflow

The target first session is:

```text
neb auth login
neb app init
neb tenant create acme --use
neb dataset upload customers.csv --name customers
neb query run queries/recent-orders.sql --param limit=50
neb app dev
neb app deploy
neb app open
```

The command tree is resource-first: `auth`, `context`, `tenant`, `dataset`,
`query`, `connector`, `app`, `job`, `access`, and `platform`. `run` is only a
verb (`query run`, `platform run`); background work is always a job. Uploads,
connector syncs, and deployments wait by default, accept `--detach`, and can be
resumed with `neb job wait <id>` or inspected with `neb job logs <id> --follow`.
There are no shorthand aliases for older command shapes.

The CLI covers login and identity, tenant context, dataset inspection, query
validation and explicit saves, deployment inspection, job logs/cancellation,
shell completion, diagnostics, and clean-directory initialization. Structured
results use a versioned `--json` envelope; stdout remains pipeable and progress
and diagnostics use stderr.

### Membership invitations

Adding a member who has already signed in takes effect immediately. For an
unknown email, the same command prints a one-time acceptance link that expires
after seven days; copy it and send it to the intended recipient:

```text
neb access member add teammate@example.com --role developer
```

Nebulous does not send invitation email. Pending and expired invitations can be
managed explicitly, and renewing a link invalidates the previous link:

```text
neb access member invitation list
neb access member invitation renew <id>
neb access member invitation revoke <id>
```

In production, acceptance requires the identity provider to return the same
verified email address. Local loopback mode treats possession of the link as
authentication and switches the browser to the invited local identity. Use
`--json` when a script needs the versioned outcome and acceptance URL.

`neb app dev` develops an app against a selected Nebulous environment. It should
serve the static directory or proxy an explicitly supplied frontend server
while keeping platform credentials server-side. `neb platform ...` develops
the Nebulous platform itself.

Once the CLI completes the full workflow non-interactively, publish a small
agent skill around it. Add MCP only if a validated agent workflow cannot be
served by the public API or `neb --json`.

## Architecture

```text
Browser / neb / agent
         |
         v
Go control plane and app gateway --------> S3-compatible object storage
         |                                  (uploads, data, app bundles)
         v                                               ^
     PostgreSQL                                          |
(identity, tenants, resources, jobs,              isolated workers
 audit, DuckLake catalog metadata)          (imports and app publishing)
         |
         +----> short-lived query subprocesses ----> tenant DuckLake catalog
```

Nebulous remains one Go module with one `neb` binary. It contains the public
CLI and platform runtime, and re-executes itself through hidden internal modes
for isolated query processes.

Production may run those modes as separate processes with different
credentials and resource limits. That does not require separate artifacts,
services, or an internal RPC layer.

### Repository structure

```text
cmd/neb/             public CLI binary
internal/cli/        CLI behavior and local workflow
internal/db/         PostgreSQL persistence grouped by product domain
internal/jobs/       leased import and deployment workers
internal/manifest/   manifest and bundle validation
internal/platform/   server, worker, local, and migration process modes
internal/queryexec/  isolated query/import execution
internal/runtime/    private app host and named-query gateway
internal/server/     control-plane HTTP API and embedded dashboard
internal/storage/    concrete S3-compatible object client
sdk/js/              browser query SDK
```

Keep business rules near the concrete code that uses them. Continue using
`database/sql`, the standard HTTP stack, PostgreSQL jobs, and the existing S3
client. Add an abstraction only after multiple real implementations need it.

### Management experience

The dashboard is a calm, light-first developer workbench. Tenant context,
failures, current releases, IDs, logs, and recovery actions remain visible.
Typography and structure carry the hierarchy; color is restrained and status
never relies on color alone. Dense desktop authoring adapts to small-screen
monitoring and recovery without introducing decorative metric cards, gradients,
or terminal-themed chrome.

## Trust boundaries and invariants

- Every tenant-owned control row carries immutable `tenant_id`.
- Composite foreign keys prevent cross-tenant relationships.
- Tenant slugs are globally unique because they appear in hostnames.
- Dataset, query, connector, and app slugs are unique within a tenant.
- Storage keys and resource relationships use immutable IDs, never mutable
  slugs.
- Tokens, login codes, and sessions store hashes rather than reusable secrets.
- Job idempotency is unique by tenant, job kind, and idempotency key.
- Query revisions and deployments are immutable.
- Deployment query grants use tenant-scoped foreign keys.
- The current deployment must belong to the same tenant and app.
- User-deployed JavaScript never shares the control-plane origin.

DuckDB treats untrusted SQL as code. SQL validation is useful, but the security
boundary is the supervised query subprocess with tenant-only credentials,
restricted filesystem and network access, an extension allowlist, and explicit
CPU, memory, temporary-disk, output, and time limits.

Connector secrets never live in connector configuration or logs. REST
connectors must reject loopback, link-local, and private destinations by
default, revalidate redirects, cap response size and duration, and run with
restricted egress.

## Operational requirements

- Structured logs carry request, tenant, actor, sync, job, and deployment IDs
  without raw secrets or query parameters.
- Metrics cover HTTP latency/errors, queue depth and age, job duration and
  failures, query resources, object bytes, and deployment success.
- Graceful shutdown stops claiming work, finishes or releases leases, and
  never marks partial work successful.
- PostgreSQL and versioned object storage are backed up and restored to a
  mutually consistent point in a tested drill.
- Per-tenant limits cover concurrent and queued jobs, stored bytes, query time
  and output, returned rows, and app bundle size.
- Performance baselines cover CLI startup, dependency readiness, worker claim
  latency, query subprocess startup, representative query latency, bundle
  preflight, and deployment promotion.

Keep deployment-wide constants near their enforcement points until real tenant
plans require shared configuration. Add caching, resident query pools, a CDN,
or a message broker only after measurements identify the need and an isolation
and invalidation model is clear.

## Near-term direction

The remaining work is depth rather than another product surface:

1. Add operational metrics for the limits and structured events already
   enforced by the control plane and workers.
2. Exercise connector failure/cancellation, concurrent deployment, tenant
   isolation, and the clean-machine workflow in automated integration tests.
3. Document and test backup/restore, package release binaries, and run an
   accessibility pass over every dashboard mutation.
4. Publish an agent skill around the versioned CLI only after the
   non-interactive workflow proves stable.

## Intentional non-goals

- Arbitrary backend code, functions, or containers.
- Raw SQL from deployed browser apps.
- App writes, webhooks, streaming ingestion, CDC, or workflow DAGs.
- Public apps, custom domains, cross-tenant sharing, or row-level grants.
- A connector marketplace or user-authored connector code.
- Enterprise SSO, billing automation, or a separate agent-only API.

These should not shape the core until a concrete product requirement needs
them.

## Local development

Docker Compose runs PostgreSQL and the S3-compatible development store. Go
processes remain on the host:

```text
neb platform run
```

`neb platform run` starts local dependencies, the control plane, and the worker
together. For platform
development, install the dashboard packages once and run the hot-reloading
loop:

```text
npm --prefix internal/server/web install
neb platform dev
```

That command starts PostgreSQL and object storage, rebuilds/restarts Go with
Air, and serves the dashboard with Vite at `http://127.0.0.1:5173`. Vite keeps
the local token server-side while proxying API requests.

Inside an initialized app project, `neb app dev` serves the manifest's static
directory with live reload and executes query files against the selected
tenant. Use `neb app dev --proxy http://127.0.0.1:3000 -- npm run dev` to retain a
frontend framework's own HMR server.

Open the control plane at `http://127.0.0.1:8080`. App hosts use
`http://{appSlug}--{tenantSlug}.apps.localhost:8080`.

Stop the foreground platform with Ctrl-C, then preserve local data while
stopping dependencies with:

```text
neb platform stop
```

The canonical Compose definition is embedded from `internal/cli/compose.yaml`;
direct Compose commands against that file remain a supported source-checkout
escape hatch. Do not add Redis, a message broker, or an observability stack to
the default local project.

### Production identity

Set `NEB_OIDC_ISSUER`, `NEB_OIDC_CLIENT_ID`, optionally
`NEB_OIDC_CLIENT_SECRET`, and an HTTPS `NEB_PUBLIC_URL`. Set a stable
`NEB_SECRET_KEY` for connector-secret encryption and use HTTPS app/control
origins so secure cookies remain enabled.

### Pre-release compatibility

Nebulous has no compatibility layer or deprecated API aliases. Before the first
release, schema, API, and CLI changes are clean breaks; update callers and start
development databases from the current migrations instead of carrying old
behavior forward.

If this checkout previously ran the proof-of-concept migrations, rebuild the
local database once before starting it:

```text
neb platform reset --yes
```

## Quality gate

Changes are not complete until the relevant workflow is exercised and these
baseline checks pass:

```text
go test ./...
go vet ./...
docker compose -f internal/cli/compose.yaml config --quiet
```

Integration coverage must protect tenant isolation, query sandboxing and
limits, failed-sync atomicity, idempotency, app/control origin separation,
bundle traversal and size checks, deployment concurrency, secret redaction,
structured CLI output, and the documented clean-machine workflow.
