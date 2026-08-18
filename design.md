# Nebulous

Status: product and architecture draft

Nebulous is a multi-tenant platform for turning external data into small,
published web apps. A user connects or uploads data, exposes a constrained set
of queries, and deploys a static frontend that can call those queries. The same
workflow must be usable by a human through the CLI and by a coding agent.

The first release is a **data app platform**, not a general-purpose PaaS. It
hosts static frontends and managed data operations; it does not run arbitrary
application backends.

Nebulous is developer-first. The `neb` CLI is a primary product surface, not
an administrative afterthought, and every workflow should feel deliberate in
the terminal, the browser, and generated apps.

## Product goal

A tenant builder should be able to go from a CSV or API to a private, working
web app without operating a database or backend:

1. Create or select a tenant.
2. Upload data or configure a connector.
3. Inspect the resulting dataset and test a query.
4. Register the query as part of an app release.
5. Deploy the app's static assets.
6. Open the app and query tenant data through the Nebulous client SDK.

The target agent experience is the same flow through non-interactive CLI
commands. MCP support can wrap the public API later; it must not become a
second implementation of platform behavior.

## Experience principles

- **One complete path everywhere.** Every MVP workflow available in the web UI
  is available through `neb` and the public API. The UI may explain a workflow,
  but it cannot be the only way to complete it.
- **Fast first success.** A developer with a frontend and a data file should be
  able to initialize, preview, and deploy an app in under ten minutes without
  reading architecture documentation.
- **Excellent defaults, visible decisions.** Infer tenant and project context
  when unambiguous, show what was selected, and require flags only when the
  choice matters. Never silently guess across tenants.
- **Local feedback first.** Validate manifests, query syntax, parameters, and
  bundles locally when possible. Network work shows progress and remains safe
  to interrupt or retry.
- **Errors teach the fix.** Errors state what failed, why it matters, the next
  command or action to try, and a request/run ID for deeper investigation.
- **Inspectable and reversible.** Deployments, syncs, and queries show their
  inputs and status. Mutations are idempotent where practical; deploys are
  immutable and easy to roll back.
- **Human and machine friendly.** Interactive output is concise and polished;
  structured output is stable, complete, and free of decoration.
- **Consistent product language.** The CLI, API, UI, SDK, logs, and docs use the
  same resource names and lifecycle states.
- **Accessible by default.** Platform screens and generated starter apps meet
  keyboard, contrast, focus, reduced-motion, and screen-reader basics.

Design quality is part of correctness. New flows must define loading, empty,
success, partial, error, permission-denied, and destructive-action states. The
web UI should expose useful IDs, logs, API examples, and equivalent `neb`
commands instead of hiding the underlying system.

## Core terms

- **Tenant**: the hard isolation and billing boundary, usually a person, team,
  company, or organization. Tenant URLs use a mutable slug, while all storage
  and authorization use an immutable tenant ID.
- **Member**: a user with a role in a tenant.
- **Connector**: configuration and credentials for reading an external system.
- **Dataset**: a tenant-owned, queryable table or view. An upload can create a
  dataset without a connector.
- **Sync**: one attempt to update a dataset from a connector.
- **Query**: saved, parameterized, read-only SQL that an app is allowed to run.
- **App**: identity, access policy, and release history for a frontend.
- **Deployment**: an immutable app release containing static assets and pinned
  query revisions.

Calling these objects by one name consistently is important. In particular, a
connector is not itself a queryable data source; it produces datasets.

## MVP scope

The first useful vertical slice includes:

- user authentication and tenant membership;
- owner, admin, builder, and viewer roles;
- CSV and Parquet upload into a dataset;
- dataset schema and sample-row inspection;
- saved, parameterized `SELECT` queries;
- private static apps deployed as immutable releases;
- a small browser SDK for invoking an app's saved queries;
- the first-class `neb` CLI with interactive and machine-readable output;
- audit events, job logs, and basic per-tenant resource limits.

After that slice works, add one scheduled pull connector. A REST/JSON connector
with token authentication is a better test of the connector model than
building a plugin system up front.

### Explicit non-goals for the MVP

- arbitrary backend code, functions, or containers;
- raw SQL submitted by a deployed browser app;
- writes from apps, webhooks, or connector actions;
- public/anonymous apps or custom domains;
- a connector marketplace, user-authored connector code, or workflow DAGs;
- streaming ingestion, CDC, or real-time subscriptions;
- cross-tenant datasets or sharing;
- enterprise SSO, billing automation, and fine-grained row policies;
- a separate agent-only API.

These can be added when a concrete app requires them. They should not shape the
first implementation.

## Default product decisions

These defaults let implementation begin without pretending every product
question is settled:

- All apps are private to tenant members.
- A tenant member has one tenant-wide role. Per-dataset grants are deferred.
- Builders may create datasets, connectors, queries, apps, and deployments.
- Only owners/admins may manage members, API tokens, and connector credentials.
- Dataset schema drift fails a sync after initial creation. Add automatic safe
  evolution only after the failure and review experience exists.
- The platform API is versioned JSON over HTTP. Large tabular results may add
  Arrow later; JSON is sufficient to prove the workflow.
- One region and one control-plane deployment are sufficient initially.

## Architecture

```text
 Browser / CLI / agent
          |
          v
  Go API and app gateway --------> object storage
          |                       (app bundles + data files)
          v                              ^
      PostgreSQL                         |
 (control data + job queue +       isolated workers
  DuckLake catalog metadata)       (ingest + query)
```

Start as one Go module with two binaries. `neb` is the small, portable CLI;
`nebulous` is the platform binary with server, worker, migration, and isolated
query-execution modes. Production runs platform modes as separate processes
with different credentials and resource limits. Keeping DuckDB out of `neb`
preserves fast CLI startup and straightforward cross-platform distribution.

### 1. Control plane

The Go API owns:

- authentication, sessions, tenant resolution, and authorization;
- CRUD for connectors, datasets, queries, apps, and deployments;
- upload orchestration and signed object-store transfers;
- job creation, scheduling, cancellation, and status;
- app runtime authorization and response serialization;
- audit events and usage accounting.

Use a conventional HTTP router and `database/sql`. Keep business rules in the
same packages used by HTTP handlers and the CLI-facing API; do not introduce a
second internal service boundary until deployment or load requires it.

### 2. PostgreSQL

PostgreSQL is the source of truth for users, tenants, memberships, resource
metadata, deployment state, and jobs. Use a PostgreSQL jobs table with leases,
heartbeats, attempts, and `FOR UPDATE SKIP LOCKED` instead of adding a message
broker for the MVP.

Every tenant-owned control table contains `tenant_id`. Composite foreign keys
must prevent a child in one tenant from referencing a parent in another.
Authorization always derives the tenant ID from the authenticated membership;
the slug or a tenant ID supplied in a request is never sufficient authority.
PostgreSQL row-level security is worthwhile defense in depth after the basic
query patterns are covered by integration tests.

DuckLake recommends PostgreSQL as its catalog for a multi-user lakehouse. Its
managed catalog objects must live separately from Nebulous control tables.
The feasibility spike must choose and prove a per-tenant catalog isolation
scheme (separate PostgreSQL schema or database) so a query worker never attaches
a catalog containing another tenant's data.

### 3. Object storage and DuckLake

Use S3-compatible object storage for immutable app bundles, staged uploads,
and tenant data files. All keys begin with the immutable tenant ID; clients do
not choose keys directly. Development may use a local S3-compatible service.

DuckLake is the leading storage choice because snapshots, transactions, schema
evolution, and multi-process access match the ingestion/query workload. Treat
it as a decision gated by a short spike, not as a premise that the rest of the
product must hide behind a speculative storage abstraction.

Each successful ingest commits one DuckLake transaction/snapshot. A failed or
cancelled ingest must leave the previously published snapshot readable. The
control plane records the resulting snapshot ID on the sync run for auditing
and reproducibility.

### 4. Workers

Durable workers claim jobs from PostgreSQL and execute one tenant-scoped
operation at a time. The initial job kinds are:

- `dataset_import` for CSV/Parquet uploads;
- `app_publish` for validating and promoting an uploaded bundle;
- `connector_sync` once the first connector is added.

Interactive queries run synchronously in a short-lived `nebulous query-exec`
subprocess supervised by the API. The subprocess receives a resolved,
tenant-scoped request over stdin and streams a bounded result over stdout. This
avoids a second RPC service and keeps DuckDB outside the long-lived API process;
replace it with a remote runner only when scaling requires it.

DuckDB is embedded in workers and query subprocesses through its official Go
client. Start a fresh query subprocess for each request so credentials and
configuration cannot leak between tenants. Run it with:

- credentials that can access only one tenant catalog and object prefix;
- no ambient cloud credentials;
- network egress disabled unless a connector job explicitly needs it;
- community, unsigned, autoloaded, and autoinstalled extensions disabled;
- an explicit extension allowlist installed into the worker image;
- locked DuckDB configuration and restricted filesystem paths;
- CPU, memory, temporary-disk, result-size, and wall-clock limits;
- cancellation that terminates the worker process if cooperative cancellation
  misses its deadline.

DuckDB documents untrusted SQL as equivalent to executing code and recommends
process/container isolation. Parsing for `SELECT`-only behavior and prepared
parameters are useful checks, but they are not the security boundary.

### 5. App delivery and origin isolation

User-deployed JavaScript must not share an origin with the Nebulous control
plane. Otherwise an app can act with a signed-in user's dashboard session.

- Control plane: `https://cloud.example.com/{tenantSlug}`
- App runtime: `https://{appSlug}--{tenantSlug}.apps.example.com/`
- The management path `/{tenantSlug}/apps/{appSlug}` may redirect to the app
  origin, but it must not serve app JavaScript from the control-plane origin.

App runtime cookies are host-only and separate from control-plane cookies. On
first visit, the control plane redirects with a short-lived, single-use login
code that the app origin exchanges for its own session; authentication tokens
must not remain in the URL. The runtime gateway maps the host to immutable
tenant/app IDs, checks membership, and serves the current immutable deployment.
Static responses use a strict Content Security Policy and cannot access object
storage directly.

An initial `nebulous.json` manifest needs only:

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

`neb deploy` uploads the asset directory and query definitions, validates
both, and creates an immutable deployment. Promotion changes one current
deployment pointer; rollback changes it back. The deployment pins query
revisions so editing a draft query cannot silently break a running app.

## Query API

Deployed apps call named queries, not arbitrary SQL:

```text
POST /runtime/v1/queries/recent-orders
Content-Type: application/json

{ "parameters": { "limit": 50 } }
```

The runtime derives tenant, app, deployment, and user identity from the host
and session. It then verifies that the deployment contains the named query,
validates each parameter against its declared type and limits, binds values
through a prepared statement, and invokes the tenant-scoped query subprocess.

The response initially contains columns, JSON rows, the pinned dataset
snapshot IDs, and truncation metadata. Enforce a conservative maximum row
count and response byte size. Pagination is query-defined for the MVP; generic
offset pagination is not guaranteed to be stable or cheap.

Builders may test draft SQL through the management API and CLI, but it uses the
same isolated execution path. If direct SQL is later exposed as a product
feature, it remains a builder tool and never inherits broader credentials than
saved-query execution.

## Connector and ingest lifecycle

1. The control plane creates a sync run and a leased job.
2. A worker resolves the connector config and a short-lived secret reference.
3. Input is downloaded into a size-limited staging area.
4. The worker detects format/schema and validates it against the dataset.
5. Data is written inside one DuckLake transaction.
6. On commit, the worker records the snapshot ID, row count, byte count, and
   structured diagnostics and marks the run successful.
7. On failure, staged objects are eligible for cleanup and the previous
   snapshot remains current.

Runs are idempotent by `(connector_id, external_cursor)` when a connector
provides a stable cursor, or by an explicit idempotency key for uploads. A
retry must not publish duplicate data silently.

Connector configuration never contains secret values. Store encrypted secret
material in a managed secret store when available; PostgreSQL may hold only a
development implementation encrypted by a key outside the database. Logs,
errors, and audit payloads must redact headers, tokens, connection strings,
and sampled sensitive values.

The REST connector introduces SSRF risk. Resolve and validate destinations,
block loopback/link-local/private network ranges by default, re-check redirects,
limit response size and duration, and run it with restricted egress.

## Authorization model

| Capability | Owner | Admin | Builder | Viewer |
| --- | --- | --- | --- | --- |
| Manage tenant and owners | yes | no | no | no |
| Manage members and tokens | yes | yes | no | no |
| Manage connector secrets | yes | yes | no | no |
| Create datasets, queries, and apps | yes | yes | yes | no |
| Deploy and roll back apps | yes | yes | yes | no |
| Use private apps and inspect data | yes | yes | yes | yes |

Runtime access requires both membership and a grant from the current deployment
to the named query. API tokens have explicit scopes and an expiry; they do not
inherit all abilities of the user who created them. Every mutation and every
secret use writes an audit event with actor, tenant, action, resource, request
ID, and timestamp.

## MVP resource model

The MVP grows into these concepts, not necessarily one table per bullet. Add
their tables in the phase that first uses them rather than in an initial mega-
migration:

- users, sessions, tenants, tenant memberships, and API tokens;
- connectors and secret references;
- datasets and their DuckLake catalog/table identities;
- sync runs and jobs;
- saved queries and immutable query revisions;
- apps, immutable deployments, and bundle metadata;
- audit events. Add materialized usage counters when measured volume or billing
  requires them.

Use UUIDs as external identifiers, timestamps in UTC, and unique constraints
such as `(tenant_id, slug)`. Mutable slugs never appear in storage keys or
cross-resource foreign keys.

## CLI and agent workflow

The executable is named `neb`. It is a thin client of the public API and the
canonical developer workflow. A normal first session should read cleanly:

```text
neb login
neb init
neb tenant create acme --use
neb dataset upload customers.csv --name customers
neb query run queries/recent-orders.sql --param limit=50
neb dev
neb deploy
```

The initial command groups are:

```text
neb login|logout|whoami
neb init|dev|generate|deploy|open|context
neb tenant create|list|use
neb dataset upload|list|describe
neb query validate|run
neb deployment list|inspect|rollback
neb run list|get|logs|cancel
neb local up|run|status|logs|down|reset
neb completion|doctor
```

`neb init` creates the smallest valid manifest and sample query without network
access. `neb dev`
validates changes continuously and provides a loopback-only same-origin runtime
gateway in front of any frontend dev server. An optional frontend command is
passed explicitly after `--`; manifests never contain executable commands. The
CLI credential stays in the `neb` process and is never exposed to browser code.
`neb generate` writes
deterministic TypeScript types for query names, parameters, and result columns.
`neb deploy` performs a local preflight, reports each
upload/validation/promotion stage, and ends with the app URL and exact rollback
command.

`neb dev` always develops an app. `neb local ...` runs Nebulous itself. Local
Compose runs only PostgreSQL and an S3-compatible test store; Go server, worker,
and query processes stay on the host for fast rebuilds, debugging, and
profiling. `neb local` wraps `docker compose` rather than reimplementing it, and
the direct Compose commands remain documented. `down` preserves volumes;
`reset --yes` is the only command that removes this project's local data.

CLI behavior is part of the compatibility contract:

- Commands follow `neb <noun> <verb>` except the small top-level happy-path
  commands. Help includes examples for common tasks, not just a flag inventory.
- Names and slugs work wherever they are unambiguous; users should not have to
  copy UUIDs during normal work.
- Context resolution is flags, environment, project manifest, then user config.
  The selected tenant is shown before a mutation and `neb context` explains
  where every resolved value came from.
- Every command that returns a result, including mutations, supports `--json`
  with a versioned schema. stdout contains only requested data; progress,
  warnings, and diagnostics go to stderr. On failure stdout is empty. Color and
  spinners disable automatically outside a TTY.
- `neb` creates an idempotency key for every mutation, reuses it across its own
  retries, and accepts an explicit override for orchestration. Destructive
  commands explain impact and ask once in a TTY; automation can use `--yes`.
- Success output says what changed and the useful next action. Errors use stable
  codes, actionable text, and distinct exit statuses for usage, authentication,
  authorization, validation, conflict, and server failures.
- Long operations stream useful progress and remain resumable or safely
  retryable. Ctrl-C requests cancellation and clearly reports final state.
- Authentication uses a browser-based login with a headless/device fallback.
  CI uses an explicitly scoped token from `NEB_TOKEN`; credentials stored on
  disk use OS credential storage when available and restrictive permissions
  otherwise.
- Shell completions, signed release binaries, and package-manager installation
  ship with the first public CLI release. A self-update mechanism is deferred.

Documentation is executable product surface: examples are tested in CI, every
resource page includes CLI and API examples, and `neb help` links to the docs
for the installed CLI version.

Once the CLI can complete the end-to-end workflow, publish a small agent skill
that explains the manifest and commands. Add MCP only when a real agent flow
needs capabilities that structured CLI output cannot provide.

## Web product experience

The web product supports discovery, visual inspection, and operations without
becoming a separate workflow from the CLI:

- The tenant and environment are always visible. Switching context is explicit
  and never carries an in-progress mutation into another tenant.
- The tenant home emphasizes the next useful action, recent runs, failing
  resources, and current deployments instead of a grid of generic metrics.
- Resource pages use the same structure: overview, configuration, activity,
  logs, and access. They expose the corresponding `neb` command and API example.
- Dataset inspection combines schema, samples, freshness, lineage, and sync
  history. Query editing combines SQL, parameters, result shape, limits, and
  diagnostics without hiding generated SQL or execution metadata.
- App pages make preview, current deployment, release history, access, and
  rollback obvious. Destructive or production-changing actions show the exact
  target and consequence.
- Background work persists across navigation and has one consistent status and
  log experience. Do not trap users behind modal progress indicators.
- Builder screens prioritize keyboard use and information density on desktop;
  monitoring and simple recovery remain usable on small screens.
- Visual design is calm, precise, and tool-like. Typography, spacing, and state
  hierarchy do the work; status never relies on color alone. Reusable tokens
  and components are extracted after patterns repeat, not invented in advance.

Every significant flow gets a short usability pass with a new-user developer,
an experienced operator, and a keyboard/screen-reader pass before it is called
complete.

## Operational baseline

- Structured logs include request, tenant, actor, job, and deployment IDs, but
  never secret material or raw query parameters by default.
- Metrics cover HTTP latency/errors, queue depth/age, job duration/failures,
  query resource usage, object bytes, and deployment success.
- Trace one request through API, queue, worker, and storage operations.
- Back up PostgreSQL and versioned object storage. A restore drill must recover
  both to a mutually consistent point.
- Apply per-tenant limits for concurrent jobs, queued jobs, dataset bytes,
  query time, scanned bytes where measurable, returned rows, and app bundle
  size.
- Graceful shutdown stops claiming work, finishes or releases leases, and does
  not mark partial work successful.

### Performance budgets

Performance budgets are release gates from the first implementation, not a
later optimization phase. On recorded reference hardware, initial local p95
budgets are 100 ms for local CLI commands, 250 ms for edit-to-next-request
feedback, 500 ms for a fresh query subprocess plus catalog attach, and one
second for a representative query that scans 100,000 local rows and returns
100. Warm Compose dependencies should become healthy within 15 seconds and
`neb dev` should be ready within two seconds after the API is healthy.

Store benchmark baselines in CI and investigate material regressions with
profiles. Preinstall DuckDB extensions, stream uploads and bounded results, and
avoid eager CLI initialization. Do not add caches or a resident query pool
until measurements identify a bottleneck and an invalidation/isolation model.

## Delivery plan

### Phase 0: feasibility and threat-model spike

Produce disposable code and short decision records for the risky assumptions:

- Attach a DuckLake catalog backed by PostgreSQL and data in S3-compatible
  storage from a Go worker using the checked-in local Compose environment.
- Prove the chosen per-tenant catalog isolation with two tenants and separate
  worker credentials.
- Read a stable snapshot while a second worker commits an update.
- Cancel a pathological query and demonstrate CPU, memory, disk, network, file,
  extension, and time limits.
- Build and run the worker in the intended Linux deployment image.
- Measure CLI startup, query subprocess/IPC, catalog attach, and representative
  cold/warm query latency against the initial budgets.
- Prove that app-origin and control-plane cookies cannot cross trust boundaries.

Exit criterion: either DuckLake passes these checks, or the plan records the
smallest replacement storage approach before product code depends on it.

### Phase 1: tenant and API foundation

- Authentication, tenants, memberships, roles, and scoped API tokens.
- Tenant-scoped repository/query patterns and cross-tenant integration tests.
- Audit events, request IDs, structured errors, migrations, and health checks.
- CLI login, tenant selection, and JSON output conventions.
- `neb init`, contextual help, error conventions, and shell completion.
- `neb local up|run|status|logs|down|reset` over the dependency-only Compose
  project.

Exit criterion: two tenants can use the API and automated tests demonstrate
that IDs, slugs, and tokens cannot cross the boundary.

### Phase 2: upload-to-query slice

- Direct CSV/Parquet upload with limits and idempotency.
- PostgreSQL job leasing and the first real worker for dataset imports.
- Dataset creation, schema inspection, sync runs, and snapshot recording.
- Draft query validation and isolated execution.
- Saved query revisions, typed parameters, result limits, and cancellation.
- CLI commands for dataset and query operations.
- `neb dev` with local validation and the authenticated query bridge.

Exit criterion: a builder uploads a file and retrieves filtered results from a
saved query; a failed import leaves the prior version readable.

### Phase 3: app publishing slice

- Manifest parsing and path-safe bundle validation.
- Immutable deployment upload, promotion, and rollback.
- Dedicated app origin, private runtime sessions, and static asset caching.
- Browser SDK for typed named-query calls.
- Deployment-query grants and pinned query revisions.
- `neb generate`, `neb deploy`, `neb open`, and a copy-pasteable rollback
  command.

Exit criterion: the sample app deploys from the CLI, is usable by a viewer,
cannot call an undeclared query, and rolls back without rebuilding.

This is the first externally useful MVP.

### Phase 4: first scheduled connector

- REST/JSON connector with secret references, restricted egress, pagination,
  cursors, retries, and sync history.
- Basic schedules using the existing PostgreSQL job mechanism.
- Conservative schema-drift failures and clear remediation diagnostics.

Exit criterion: a scheduled sync updates a live app atomically, retries safely,
and exposes enough logs for a tenant builder to diagnose a failure.

### Phase 5: agent ergonomics and hardening

- Agent skill built around the existing CLI and sample project.
- End-to-end non-interactive deploy from a clean directory.
- Quotas, backup/restore drill, load tests, accessibility review of platform
  screens, and abuse/security review.
- MCP adapter only if validated agent workflows justify it.

Exit criterion: an agent can create and deploy the sample app with a scoped
token and no dashboard-only step, and operators can restore and investigate it.

The human CLI gate is equally important: a developer starting from a clean
machine can install `neb`, discover commands through help, deploy the sample,
diagnose a deliberate error, and roll back without opening the dashboard.

## Tests that gate the MVP

- A member, API token, resource ID, slug collision, or worker job from tenant A
  cannot read or mutate tenant B.
- A saved query cannot read arbitrary host files, reach the network, load an
  unapproved extension, modify data, exceed limits, or return unbounded output.
- Invalid query parameters cannot alter query structure.
- Failed/cancelled imports never replace the last good dataset snapshot.
- Duplicate upload/sync requests do not duplicate published data.
- A deployed app cannot access control-plane credentials or undeclared queries.
- Bundle extraction rejects traversal, symlinks, oversized files, and executable
  server content.
- Deployment promotion and rollback are atomic under concurrent requests.
- Secrets and sensitive parameters do not appear in logs or audit payloads.
- CLI golden tests keep human output readable, structured output compatible,
  stdout pipeable, exit statuses stable, and errors actionable.
- The documented clean-machine quickstart runs unchanged in CI.
- `docker compose config --quiet` and the warm local readiness budget gate the
  local environment.
- CLI, query framing, and representative integration benchmarks flag material
  regressions against recorded baselines.

## Open product questions

These affect later sequencing but do not block the first vertical slice:

1. Is Nebulous primarily a hosted service, a self-hosted product, or both?
2. Which identity provider and login methods are required first?
3. Which real connector would validate demand after file upload?
4. Are anonymous/public apps required, and if so, how are usage and data grants
   bounded?
5. What initial limits should define the target tenant: dataset size, daily
   refreshes, concurrent users, and query latency?
6. Do builders need private networking to reach databases in the first year?
7. When apps gain writes, are they CRUD against managed datasets or actions
   against external connectors? Those are different trust and transaction
   models and should not be combined by default.

## Relevant technical references

- [DuckLake: when to use it](https://ducklake.select/docs/stable/)
- [DuckLake catalog database choices](https://ducklake.select/docs/stable/duckdb/usage/choosing_a_catalog_database)
- [DuckLake transactions and snapshots](https://ducklake.select/docs/stable/duckdb/advanced_features/transactions)
- [DuckDB security guidance](https://duckdb.org/docs/current/operations_manual/securing_duckdb/overview)
- [Official DuckDB Go client](https://duckdb.org/docs/lts/clients/go)
- [SeaweedFS development S3 quickstart](https://github.com/seaweedfs/seaweedfs#quick-start-with-weed-mini)
- [Docker Compose readiness and dependency health](https://docs.docker.com/compose/how-tos/startup-order/)
