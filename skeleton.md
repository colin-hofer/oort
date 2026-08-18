# Nebulous implementation plan

This is a staged implementation plan derived from `design.md`. It is not a
directory wishlist. Create only the files required by the current stage and
let package boundaries follow working code.

The first objective is to retire the risky assumptions. The second is a thin
end-to-end product path. A complete but empty architecture is not progress.

## Ground rules

- One Go module and two shipped binaries:
  - `neb`: the fast, developer-facing CLI and local orchestrator;
  - `nebulous`: the platform binary with `server`, `worker`, `query-exec`, and
    `migrate` modes.
- Keep `neb` independent of DuckDB and server code. It should remain a small,
  portable binary with fast startup.
- Start with the Go standard library, `database/sql`, the PostgreSQL driver,
  and the official DuckDB Go client. Add a dependency only when it removes more
  code or risk than it introduces.
- Use concrete types and functions. Do not add repository interfaces, service
  interfaces, factories, or a shared `model` package for one implementation.
- Keep types beside the behavior that owns them. Split a package when it has
  two distinct reasons to change, not when a diagram has another box.
- Every stage ends in one runnable workflow and one check that proves its main
  invariant.
- Security boundaries are real boundaries: tenant credentials, OS processes,
  origins, and database constraints. Package names are organization only.

## Stage 0: disposable risk spike

Start here. Do not scaffold the product packages yet.

```text
nebulous/
├── compose.yaml
├── design.md
├── go.mod
├── skeleton.md
└── spike/
    └── ducklake/
        ├── Dockerfile
        ├── main.go
        └── integration_test.go
```

The spike uses PostgreSQL and S3-compatible storage from `compose.yaml` to
answer these questions with measurements:

1. Can the Go DuckDB client attach a DuckLake catalog backed by PostgreSQL and
   write data files to the local object store reproducibly?
2. Does the chosen tenant isolation scheme give tenant A credentials that
   cannot list, attach, read, or write tenant B's catalog or object prefix?
3. Can one process keep reading a stable snapshot while another commits an
   ingest transaction?
4. Does killing a fresh child process using the proposed `query-exec` protocol
   stop a pathological query and release its CPU, memory, temporary files, and
   database connections?
5. With external access, extension autoload/install, and community extensions
   disabled, can a query still read host files or reach the network?
6. What are cold and warm costs for process startup, catalog attach, a trivial
   query, a 10,000-row query, and JSON result streaming?
7. Does the intended Linux image contain every required DuckDB extension at
   build time without downloading code at query time?

The integration test creates two tenants, writes distinguishable data, runs
concurrent read/write work, attempts cross-tenant access, and asserts the last
good snapshot survives a failed ingest. It may require Docker and be guarded by
an explicit integration-test flag; it must be one command in CI.

### Execution log

Stage 0 passed on 2026-08-18. The opt-in test starts Compose, builds the worker
image, verifies the required extensions with Docker networking disabled, and
runs the storage, isolation, snapshot, sandbox, cancellation, and performance
checks:

```text
NEB_DUCKLAKE_INTEGRATION=1 go test -v ./spike/ducklake
```

The warm sample was reproduced 30 times against the image left by that test;
the nearest-rank p95 is element 29 after sorting:

```bash
for i in {1..30}; do
  printf -v suffix '%02x' "$i"
  docker run --rm --network nebulous_default \
    -e NEB_DUCKLAKE_PG_HOST=postgres -e NEB_DUCKLAKE_PG_PORT=5432 \
    -e NEB_DUCKLAKE_S3_ENDPOINT=objectstore:8333 \
    nebulous-ducklake-spike:dev roundtrip "run_p${BASHPID}${suffix}"
done | jq -s '
  def stats: sort | {median: ((.[14] + .[15]) / 2), p95: .[28]};
  {process_ms: (map(.read_process_ms) | stats),
   attach_ms: (map(.read.attach_ms) | stats),
   scan_ms: (map(.read.work_ms) | stats)}'
```

Tested with DuckDB 1.5.5, DuckLake extension `d8a1881e`, PostgreSQL 18.4, and
SeaweedFS 4.42 on Linux with an 8-core Intel Core Ultra 7 268V and 30 GiB RAM.
Across 30 warm catalog runs, fresh process plus attach and scan measured 297 ms
median / 327 ms p95; attach alone measured 53 ms median / 68 ms p95. Two
non-superuser PostgreSQL roles, separate catalog databases, and bucket-scoped
S3 credentials denied tenant A every attach, list, read, and write attempt
against tenant B. A reader stayed at 10,000 rows while another process committed
row 10,001, then saw the commit; a deliberately failed ingest left 10,001 rows.

The query image ran read-only with no capabilities or privilege escalation, a
64-process limit, 128 MiB DuckDB memory, 64 MiB temporary storage, locked
configuration, and external access disabled. Host, network, file-write, and
extension-install attempts failed. Killing the query process group released it
and its temporary directory in 305 ms. Across 30 runs constrained to one CPU
and 768 MiB, p95 was 39.8 ms for a fresh trivial query process, 41.9 ms for a
100,000-row scan returning 100 rows, and 67.4 ms to stream 10,000 JSON rows.

Decision: keep DuckLake and the short-lived `query-exec` design. The known
ceiling is that SeaweedFS's simple static ACL is bucket-scoped, so the local
proof uses one bucket per tenant; production still needs equivalent
credential-scoped storage and OS-level egress policy. These are development
baselines, not the documented 4-vCPU release-machine baseline. The disposable
spike was removed after this record was captured.

Exit with a short decision record containing the versions tested, isolation
scheme, benchmark hardware/results, required extensions, and known ceiling.
Then delete `spike/`. If DuckLake fails, choose the smallest replacement before
product code imports it.

## Stage 1: tenant walking skeleton

After the spike passes, create only:

```text
nebulous/
├── cmd/
│   ├── neb/main.go
│   └── nebulous/main.go
├── internal/
│   ├── cli/cli.go
│   ├── db/
│   │   ├── db.go
│   │   ├── migrate.go
│   │   └── migrations/001_init.sql
│   └── server/
│       ├── server.go
│       └── server_test.go
├── compose.yaml
├── design.md
├── go.mod
└── skeleton.md
```

The walking path is:

```text
neb local up
  -> PostgreSQL and object storage become healthy
neb local run
  -> migrations -> local identity -> server
neb tenant create acme
  -> authenticated HTTP request -> tenant-scoped SQL -> JSON response
neb tenant list
  -> the created tenant, readable by a human or with --json
```

`cmd/*/main.go` only parses the top-level mode, handles signals, and calls an
internal package. `internal/server` owns the HTTP handlers and current business
rules. `internal/db` owns the connection pool, embedded sequential migrations,
and concrete SQL functions. Split server behavior into more files in the same
package before creating more packages.

The first migration contains only users, API-token hashes, tenants, memberships,
and audit events. Add sessions with the first browser login flow and add the jobs
table with the first background job; neither deserves an empty abstraction.

Local mode binds to loopback, creates an ephemeral local identity and scoped
token, and stores the token with mode `0600` under the user's state directory.
The process must refuse local/dev authentication on a non-loopback listener.
Production startup must fail closed when real authentication is not configured.

Exit when two local identities can create tenants and an integration test proves
that changing slugs, IDs, tokens, or request bodies cannot cross the tenant
boundary.

### Execution log

Stage 1 passed on 2026-08-18. `neb local up` brought both dependencies healthy
and passed a real PostgreSQL `SELECT 1` plus authenticated S3 `HeadBucket`.
`neb local run` migrated the database, created a scoped local identity, wrote
its token with mode `0600`, and served on loopback. The built CLI then created
and listed a tenant through the authenticated HTTP API.

The opt-in test creates two identities and tenants, rejects changed tokens,
identity-bearing query parameters and bodies, global slug reuse, and a direct
cross-tenant audit foreign key:

```text
NEB_INTEGRATION=1 go test -v ./internal/server -run TestTenantBoundary
```

Across 30 runs on the Stage 0 machine, `neb --help` measured 9.3 ms median /
11.1 ms p95. Stage 2 may now start.

## Stage 2: upload to saved query

Add code only along this path:

```text
neb dataset upload customers.csv
  -> direct signed upload -> import job -> DuckLake snapshot
neb query run queries/customers.sql --param limit=50
  -> saved/draft query resolution -> isolated subprocess -> bounded result
```

Expected growth, when the working code requires it:

```text
internal/
├── jobs/         # PostgreSQL leasing plus dataset import executor
├── queryexec/    # parent protocol and child execution
├── storage/      # the one S3-compatible client actually in use
└── server/       # datasets.go and queries.go remain in this package
```

`nebulous query-exec` re-executes the already-built platform binary via
`os.Executable`; it never invokes `go run` per query. The request is framed on
stdin and results are streamed on stdout. stderr is diagnostic only. The parent
sets a deadline and byte cap, closes stdin, drains bounded output, and kills the
whole child process group if cancellation misses its grace period.

Do not introduce a DuckLake interface while it has one implementation. Put
catalog attach/transaction code with `queryexec` or the import executor; extract
a small concrete package only after both genuinely share it.

Exit when an upload becomes queryable, a failed import leaves the previous
snapshot readable, cross-tenant queries fail, and the measured query path meets
the initial performance budgets below.

The first worker polls PostgreSQL every 250 ms with jitter and claims work with
`FOR UPDATE SKIP LOCKED`. Add `LISTEN/NOTIFY` only if measured polling load or
claim latency requires it.

### Execution log

Stage 2 passed on 2026-08-18. The CLI streams signed CSV and Parquet uploads
directly to object storage, a PostgreSQL-leased worker commits them to DuckLake,
and saved typed queries run in a bounded re-executed `nebulous query-exec`
process. Query children load only the extensions installed by trusted worker
startup, discard ambient AWS/PostgreSQL credentials, disable external access,
lock configuration, cap memory/temp files/results/time, and are terminated as a
process group on cancellation.

The combined Stage 1/2 integration gate uses the actual `neb dataset upload`
and `neb query run` implementations. It creates two independently credentialed
PostgreSQL catalog databases, imports CSV and Parquet, rejects cross-tenant,
host-file, and network reads, kills a pathological query within the two-second
gate, and proves a schema-changing failed import leaves the last good rows and
snapshot readable:

```text
NEB_INTEGRATION=1 NEB_STAGE2_INTEGRATION=1 \
  go test -v ./internal/server -count=1
```

On the Stage 0 machine, the default 250 ms polling loop claimed the observed
job in 166 ms. Across 30 saved-query HTTP/CLI-compatible executions, median was
329 ms and nearest-rank p95 was 391 ms, below the 500 ms attach budget. Local
benchmarks measured query request framing at 8.1 us/op and 2.5 KiB/op; bounded
results are drained to mode-`0600` temporary files rather than held in API
memory.

The known local ceiling remains unchanged: Compose exposes one development S3
administrator credential. Query SQL cannot use it for direct file/network
access after the attached tenant catalog is locked, while release deployment
still requires the tenant-scoped object credentials and OS egress policy proven
by Stage 0.

## Stage 3: deploy one app

Add the app runtime only when upload-to-query works:

```text
internal/
├── manifest/     # nebulous.json validation used by CLI and server
└── runtime/      # separate app host, session, query grants, static delivery
sdk/
└── js/           # public browser SDK and generated TypeScript contract
```

Keep app publishing in the existing jobs package until it becomes independently
complex. Store one immutable bundle object plus its checksum and metadata on the
deployment. Do not create a database row for every static asset; the validated
bundle manifest is enough until the CDN/storage implementation proves otherwise.

Pin allowed query revisions with a `deployment_queries` table, not a JSON array,
so PostgreSQL can enforce tenant-scoped foreign keys. Promotion updates one
current deployment pointer; rollback updates it back.

The app gateway and control plane may run in one process, but browser isolation
comes from distinct, validated hosts and cookie scopes—not listener ports. Each
listener rejects hosts belonging to the other surface. Integration tests cover
the reverse-proxy host mapping, login-code exchange, cookie attributes, and an
app's inability to call an undeclared query.

Exit when `neb deploy` produces a private app URL, `neb open` reaches it, and
the CLI prints a rollback command that restores the prior release.

## Later stages, created when reached

- **Connector:** add connector tables, secret storage, scheduling, and one
  built-in REST/JSON executor. No plugin interface before a second connector.
- **Dashboard:** add `web/` when the first browser management flow is built.
  It calls the same public API as `neb`.
- **Limits:** keep constants close to query/import code first. Extract a limits
  package only after limits are shared and tenant plans actually vary.
- **MCP:** wrap the public API or `neb --json` after an agent workflow shows a
  real gap. Do not duplicate business logic.

Empty `auth`, `audit`, `model`, `secrets`, `limits`, `docs`, `testdata`, or SDK
directories are not part of the starting skeleton. Create a directory when its
first real file lands.

## Local development

There are two deliberately different workflows:

- `neb dev` develops an app against a selected Nebulous environment.
- `neb local ...` develops or runs the Nebulous platform itself.

The meaning of `neb dev` never changes based on the current directory.

### Topology

Docker Compose runs dependencies only. Go processes run on the host for fast
compile/restart, debugger support, and direct profiling.

```text
host: neb / nebulous server / nebulous worker / query subprocesses
  |
  +-- 127.0.0.1:55432 PostgreSQL control data + DuckLake catalogs
  +-- 127.0.0.1:9000  S3-compatible API
  +-- 127.0.0.1:9001  object-store development console
```

Do not add Redis, a message broker, pgAdmin, a mail catcher, or an observability
stack to the default Compose project. PostgreSQL is the queue; `psql`, structured
logs, and the object-store console cover the first stages.

The checked-in `compose.yaml` has fixed development-only credentials, named
volumes, health checks, loopback-only published ports, and configurable host
ports. It does not build Nebulous or mount the source tree.

It uses pinned SeaweedFS `mini` as the local S3 test double because MinIO's
community repository and prebuilt distribution are no longer maintained. Do
not call SeaweedFS-specific APIs: production remains an S3-compatible storage
contract. Local DuckDB configuration uses the loopback endpoint, path-style
URLs, `us-east-1`, and TLS disabled.

### Direct commands

The Compose commands remain a documented escape hatch:

```text
docker compose up -d --wait
docker compose ps
docker compose logs -f postgres objectstore
docker compose down
```

`down` preserves data. Deleting volumes is always a separate, explicit action.

### `neb local` contract

Implement these commands as small wrappers around `docker compose` and the
platform binary; do not reimplement a container client:

```text
neb local up                 # start dependencies, wait, print endpoints
neb local run                # migrate, then run current platform modes in foreground
neb local status             # processes, health, ports, current local identity
neb local logs [service]     # compose logs; -f follows
neb local down               # stop dependencies, preserve named volumes
neb local reset --yes        # stop and delete only this project's volumes/state
```

Behavior requirements:

- Find `compose.yaml` from the repository root and use a fixed Compose project
  name so commands cannot target an unrelated project.
- Check Docker/Compose availability and port conflicts before starting. Errors
  include the exact remediation or environment override.
- `up` is idempotent and waits for Compose readiness, then performs a real
  PostgreSQL `SELECT 1` and authenticated S3 `HeadBucket`. It prints the
  database, S3, and console endpoints without printing credentials unless
  explicitly requested.
- `run` locates the sibling `nebulous` binary; in a source checkout it may run
  `go run ./cmd/nebulous` as a fallback. The platform process re-executes its
  own compiled path for query children, so queries never trigger recompilation.
- Ctrl-C gracefully stops the local server/worker but leaves dependency data
  intact. A second Ctrl-C forces termination and reports what may be retried.
- `reset` resolves and displays the exact Compose project, volumes, and local
  state directory before deletion. It never accepts a caller-supplied broad
  filesystem path.
- All commands support `--json`; stdout/stderr and exit-code rules match the
  rest of `neb`.

For local origin isolation use `http://cloud.localhost:8080` and
`http://{appSlug}--{tenantSlug}.apps.localhost:8081`. Host validation remains
enabled locally. Port overrides are `NEB_LOCAL_POSTGRES_PORT`,
`NEB_LOCAL_S3_PORT`, and `NEB_LOCAL_S3_CONSOLE_PORT`; `neb local status` shows
the resolved values.

### App development loop

`neb dev` validates the manifest and known query files, then starts a
loopback-only gateway. With `--proxy http://127.0.0.1:5173`, it proxies frontend
HTTP and WebSocket traffic while serving `/runtime/v1/*` itself. With no proxy,
it serves `app.dir` directly. An optional frontend command is explicit after
`--`; the manifest never contains executable shell commands.

```text
neb dev --proxy http://127.0.0.1:5173 -- npm run dev -- --host 127.0.0.1
```

The gateway keeps the CLI credential server-side, accepts only its printed Host
and Origin, and makes development use the same browser API path as production.
No long-lived token is embedded in JavaScript or written into the app bundle.
Ctrl-C forwards the signal to the frontend child, cancels in-flight runtime
requests, removes the development session, and exits within two seconds.

## Database invariants

Write these into constraints and integration tests rather than relying on every
caller to remember them:

- Tenant slugs are globally unique because they appear in platform hostnames.
- Dataset, query, connector, and app slugs are unique by `(tenant_id, slug)`.
- Every tenant-owned relationship carries `tenant_id` in its foreign key.
- Every tenant-owned parent has `UNIQUE (tenant_id, id)` so those composite
  foreign keys are enforceable.
- API tokens and sessions store hashes, never reusable plaintext secrets.
- Job idempotency is unique by `(tenant_id, kind, idempotency_key)`, not globally.
- Connector cursors are unique by tenant and connector when non-null.
- Deployment query pins use tenant-scoped foreign keys through
  `deployment_queries(tenant_id, deployment_id, query_revision_id)`.
- The current deployment must belong to the same tenant and app.
- Immutable revisions and deployments are never updated in place.

Add tables by phase:

| Stage | Tables |
| --- | --- |
| 1 | users, tenants, memberships, api_tokens, audit_events |
| 2 | jobs, datasets, sync_runs, queries, query_revisions |
| 3 | sessions, apps, deployments, deployment_queries |
| Connector | connectors, secret_refs, schedules |

Usage counters and per-file deployment rows are deferred until measured usage
or storage behavior requires them.

## Performance is a feature

Record the benchmark machine and dataset beside every result. These are initial
regression gates for local release builds, measured over 30 runs on a documented
4-vCPU, 8-GiB Linux machine with initialized volumes and already-pulled images.
Record median and p95; changing a budget requires a benchmark and short decision
note.

| Path | Initial budget |
| --- | --- |
| `neb --help`, `neb context`, local manifest validation | p95 <= 100 ms, no network |
| Warm Compose dependencies to healthy | <= 15 s |
| `neb dev` after the API is healthy | <= 2 s |
| Manifest/query edit visible on next request | <= 250 ms |
| Fresh query process + catalog attach for `SELECT 1` | p95 <= 500 ms |
| Scan 100,000 rows and return 100 | p95 <= 1 s end to end |
| Queued job to worker claim | p95 <= 500 ms |
| Ctrl-C to supervised query-process exit | <= 2 s |
| Preflight a 25-MiB app bundle | <= 2 s and <= 64 MiB extra RSS |
| Deployment promotion after upload | p95 <= 500 ms |

These are regression alarms, not reasons to fake work or remove safety checks.
If the reference environment cannot meet a budget, record the baseline and the
profile before changing the architecture.

Performance rules:

- `neb` lazily loads config and never initializes DuckDB or contacts the network
  for local/help commands.
- Build SQL and HTTP clients once per process; size their connection pools from
  measured concurrency and close them on shutdown.
- Stream uploads directly to object storage and stream bounded query results;
  do not buffer whole files or result sets in the API.
- Preinstall DuckDB extensions. Query execution never performs extension or
  dependency downloads.
- Keep hot metadata requests to a small, known number of SQL queries and add an
  integration assertion when an N+1 regression is plausible.
- Benchmark the subprocess design before adding a pool. First remove repeated
  setup and rely on OS/object-store caches. Add a bounded, tenant-isolated pool
  only if profiles show process/catalog startup breaks the query budget.
- Add caching only for a measured bottleneck with an invalidation rule.
- Keep debug/profiling endpoints loopback-only in local mode and authenticated
  or disabled elsewhere.

Initial deployment-wide safety limits are a 1-GiB upload, 25-MiB app bundle,
10-second query, 10,000 returned rows, 10-MiB response, 512-MiB query memory,
1-GiB temporary disk, two concurrent queries per tenant, and one ingest per
tenant. Keep these as server configuration, not tenant-plan tables, until real
plans differ.

Minimum performance checks are `go test -bench` for CLI startup/parsing and
query framing, plus the opt-in Docker integration benchmark from Stage 0. CI
stores results and flags material regressions; it does not fail on single-run
timing noise.

## Definition of a good skeleton

At every commit:

- `go test ./...` passes;
- `go vet ./...` passes;
- `docker compose config --quiet` passes;
- the current documented workflow runs from a clean checkout;
- there are no empty packages or interfaces with one implementation;
- tenant and trust-boundary checks exist before feature breadth;
- human CLI output is readable and `--json` remains pipeable;
- relevant performance budgets have a recorded baseline.
