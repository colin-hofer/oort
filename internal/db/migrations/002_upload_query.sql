CREATE TABLE datasets (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    slug text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$'),
    current_snapshot_id bigint,
    schema_json jsonb,
    row_count bigint,
    byte_count bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, slug)
);

CREATE TABLE sync_runs (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    dataset_id uuid NOT NULL,
    actor_user_id uuid NOT NULL,
    status text NOT NULL CHECK (status IN ('awaiting_upload', 'queued', 'running', 'succeeded', 'failed')),
    format text NOT NULL CHECK (format IN ('csv', 'parquet')),
    object_key text NOT NULL,
    snapshot_id bigint,
    row_count bigint,
    byte_count bigint NOT NULL CHECK (byte_count >= 0),
    schema_json jsonb,
    error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, dataset_id)
        REFERENCES datasets(tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, actor_user_id)
        REFERENCES memberships(tenant_id, user_id)
);

CREATE TABLE jobs (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('dataset_import')),
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    sync_run_id uuid NOT NULL,
    status text NOT NULL CHECK (status IN ('awaiting_upload', 'queued', 'running', 'succeeded', 'failed')),
    available_at timestamptz NOT NULL DEFAULT now(),
    leased_by text,
    lease_expires_at timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, kind, idempotency_key),
    FOREIGN KEY (tenant_id, sync_run_id)
        REFERENCES sync_runs(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX jobs_claim_idx
    ON jobs (available_at, created_at)
    WHERE status = 'queued';

CREATE TABLE queries (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    slug text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$'),
    current_revision_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, slug)
);

CREATE TABLE query_revisions (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    query_id uuid NOT NULL,
    version integer NOT NULL CHECK (version > 0),
    sql_text text NOT NULL CHECK (length(sql_text) BETWEEN 1 AND 1048576),
    parameter_types jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, query_id, version),
    FOREIGN KEY (tenant_id, query_id)
        REFERENCES queries(tenant_id, id) ON DELETE CASCADE
);

ALTER TABLE queries ADD CONSTRAINT queries_current_revision_fk
    FOREIGN KEY (tenant_id, current_revision_id)
    REFERENCES query_revisions(tenant_id, id);
