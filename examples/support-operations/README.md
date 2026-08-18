# Support operations example

QueueWatch is a complete Oort data app for open-source support managers. A scheduled REST/JSON connector imports the 100 most recently updated DuckDB issues from GitHub, a CSV upload supplies local triage policies, SQL queries join both datasets, and the private static app presents backlog and aging risk through named query grants.

The connector calls GitHub directly—there is no fixture server. The small GitHub-shaped file under `testdata/` exists only so automated tests stay deterministic and offline.

## Run it locally

From the repository root, install the current CLI and start Oort:

```sh
go install ./cmd/oort
oort platform run
```

In a second terminal, authenticate and select a tenant. Reuse an existing tenant if you already have one:

```sh
oort auth login
oort tenant create support-demo --use
cd examples/support-operations
```

Upload the business-owned triage policy, then connect GitHub's public Issues Search API and publish its first snapshot:

```sh
oort dataset upload data/triage-policies.csv --name triage-policies
oort connector create duckdb-issues \
  --dataset github-issues \
  --url 'https://api.github.com/search/issues?q=repo%3Aduckdb%2Fduckdb%20is%3Aissue&sort=updated&order=desc&per_page=100' \
  --records-pointer /items \
  --refresh-minutes 60
oort connector sync duckdb-issues
```

Public GitHub data works without credentials. For a higher rate limit, set `GITHUB_TOKEN` and append `--bearer-token-env GITHUB_TOKEN` to `oort connector create`; Oort encrypts the bearer token and does not return it.

Inspect the datasets and test the same typed query used by the app:

```sh
oort dataset show github-issues
oort dataset sample github-issues
oort query run queries/ops-summary.sql --param days=30
```

Develop or deploy the app:

```sh
oort app dev
oort app deploy
oort app open
```

`oort app dev` serves the dashboard at `http://127.0.0.1:8787` and reloads query files on each request. A deployment pins immutable revisions of the four queries declared in `oort.json`; the browser cannot send arbitrary SQL.

The GitHub Search API returns at most 100 items per page and has a separate search rate limit. This example intentionally treats the newest 100 issues as its operational working set. GitHub documents the [search endpoint](https://docs.github.com/en/rest/search/search#search-issues-and-pull-requests) and [REST rate limits](https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api).

## Deployment and data resources

`oort app deploy` intentionally publishes only static assets and immutable query revisions. Connectors, secrets, datasets, and their current snapshots belong to the tenant environment, not to one app release. This prevents a frontend rollback from rolling back or recreating production data infrastructure.

For now, treat the commands above as environment provisioning:

1. Provision the connector and uploaded reference dataset once per tenant.
2. Sync them and verify their schemas.
3. Deploy or roll back the app independently as often as needed.

The committed CSV is source-controlled seed data, but uploading it is an explicit tenant mutation. If several apps need repeatable environments, the next platform feature should be a separate declarative resource apply command with idempotent connector/dataset definitions and deploy-time prerequisite checks—not implicit resource creation inside `app deploy`.

## What this demonstrates

- `duckdb-issues` is a scheduled external HTTPS connector with an atomic sync into `github-issues`.
- `triage-policies` is a directly uploaded CSV dataset for business-owned ownership, SLA, and capacity targets.
- The SQL files classify nested GitHub labels and use typed parameters, joins, aggregates, and a stable dataset-relative reporting window.
- The browser SDK calls only deployment-granted queries with same-origin credentials.
- The app is dependency-free, responsive, accessible, and compatible with the runtime Content Security Policy.
