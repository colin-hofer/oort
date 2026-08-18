CREATE TABLE users (
    id uuid PRIMARY KEY,
    email text NOT NULL UNIQUE,
    display_name text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tenants (
    id uuid PRIMARY KEY,
    slug text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$'),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('owner', 'admin', 'developer', 'viewer')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE membership_invitations (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    email text NOT NULL CHECK (email = lower(btrim(email)) AND length(email) BETWEEN 3 AND 320),
    role text NOT NULL CHECK (role IN ('owner', 'admin', 'developer', 'viewer')),
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    invited_by_user_id uuid,
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, email),
    FOREIGN KEY (tenant_id, invited_by_user_id)
        REFERENCES memberships(tenant_id, user_id) ON DELETE SET NULL (invited_by_user_id)
);

CREATE INDEX membership_invitations_tenant_status_idx
    ON membership_invitations (tenant_id, expires_at DESC)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE TABLE api_tokens (
    id uuid PRIMARY KEY,
    tenant_id uuid,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    name text NOT NULL,
    scopes text[] NOT NULL,
    expires_at timestamptz NOT NULL,
    created_by_user_id uuid REFERENCES users(id),
    last_used_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships(tenant_id, user_id) ON DELETE CASCADE
);

CREATE INDEX api_tokens_user_created_idx
    ON api_tokens (user_id, created_at DESC);

CREATE TABLE audit_events (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    actor_user_id uuid,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id uuid NOT NULL,
    request_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, actor_user_id)
        REFERENCES memberships(tenant_id, user_id) ON DELETE SET NULL (actor_user_id)
);

CREATE INDEX audit_events_tenant_created_idx
    ON audit_events (tenant_id, created_at DESC);

CREATE TABLE user_identities (
    issuer text NOT NULL,
    subject text NOT NULL,
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, subject)
);

CREATE TABLE control_sessions (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tenant_id uuid,
    scopes text[] NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships(tenant_id, user_id) ON DELETE CASCADE
);

CREATE INDEX control_sessions_expiry_idx ON control_sessions (expires_at);

CREATE TABLE oidc_auth_attempts (
    state_hash bytea PRIMARY KEY CHECK (octet_length(state_hash) = 32),
    nonce text NOT NULL,
    code_verifier text NOT NULL,
    cli_return_url text,
    invitation_id uuid REFERENCES membership_invitations(id) ON DELETE CASCADE,
    invitation_token_hash bytea CHECK (invitation_token_hash IS NULL OR octet_length(invitation_token_hash) = 32),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((invitation_id IS NULL) = (invitation_token_hash IS NULL))
);

CREATE INDEX oidc_auth_attempts_expiry_idx ON oidc_auth_attempts (expires_at);

CREATE TABLE cli_login_codes (
    code_hash bytea PRIMARY KEY CHECK (octet_length(code_hash) = 32),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX cli_login_codes_expiry_idx ON cli_login_codes (expires_at);

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

CREATE TABLE secret_refs (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind = 'bearer'),
    ciphertext bytea NOT NULL,
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    created_by_user_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, created_by_user_id)
        REFERENCES memberships(tenant_id, user_id)
);

CREATE TABLE connectors (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    dataset_id uuid NOT NULL,
    secret_ref_id uuid,
    slug text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$'),
    url text NOT NULL CHECK (length(url) BETWEEN 1 AND 4096),
    records_pointer text NOT NULL DEFAULT '',
    cursor_parameter text,
    next_cursor_pointer text,
    enabled boolean NOT NULL DEFAULT true,
    refresh_minutes integer NOT NULL CHECK (refresh_minutes BETWEEN 1 AND 10080),
    next_sync_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, slug),
    UNIQUE (tenant_id, dataset_id),
    FOREIGN KEY (tenant_id, dataset_id)
        REFERENCES datasets(tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, secret_ref_id)
        REFERENCES secret_refs(tenant_id, id)
);

CREATE INDEX connectors_due_idx
    ON connectors (next_sync_at, created_at)
    WHERE enabled;

CREATE TABLE sync_runs (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    dataset_id uuid NOT NULL,
    connector_id uuid,
    actor_user_id uuid,
    status text NOT NULL CHECK (status IN ('awaiting_upload', 'queued', 'running', 'succeeded', 'failed', 'cancelled')),
    format text NOT NULL CHECK (format IN ('csv', 'parquet', 'json')),
    source text NOT NULL DEFAULT 'upload' CHECK (source IN ('upload', 'connector')),
    object_key text NOT NULL,
    snapshot_id bigint,
    row_count bigint,
    byte_count bigint NOT NULL CHECK (byte_count >= 0),
    schema_json jsonb,
    error text,
    cancel_requested_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (tenant_id, dataset_id)
        REFERENCES datasets(tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, connector_id)
        REFERENCES connectors(tenant_id, id) ON DELETE SET NULL (connector_id),
    FOREIGN KEY (tenant_id, actor_user_id)
        REFERENCES memberships(tenant_id, user_id),
    CONSTRAINT sync_runs_connector_source_check CHECK (
        source = 'connector' OR connector_id IS NULL
    )
);

CREATE INDEX sync_runs_connector_created_idx
    ON sync_runs (tenant_id, connector_id, created_at DESC)
    WHERE connector_id IS NOT NULL;

CREATE TABLE apps (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    slug text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$'),
    current_deployment_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, slug)
);

CREATE TABLE deployments (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    app_id uuid NOT NULL,
    actor_user_id uuid NOT NULL,
    previous_deployment_id uuid,
    version integer NOT NULL CHECK (version > 0),
    status text NOT NULL CHECK (status IN ('awaiting_upload', 'queued', 'running', 'succeeded', 'failed', 'cancelled')),
    object_key text NOT NULL,
    checksum bytea NOT NULL CHECK (octet_length(checksum) = 32),
    byte_count bigint NOT NULL CHECK (byte_count BETWEEN 1 AND 26214400),
    manifest_json jsonb NOT NULL,
    error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, app_id, id),
    UNIQUE (tenant_id, app_id, version),
    FOREIGN KEY (tenant_id, app_id)
        REFERENCES apps(tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, actor_user_id)
        REFERENCES memberships(tenant_id, user_id),
    FOREIGN KEY (tenant_id, app_id, previous_deployment_id)
        REFERENCES deployments(tenant_id, app_id, id)
);

ALTER TABLE apps ADD CONSTRAINT apps_current_deployment_fk
    FOREIGN KEY (tenant_id, id, current_deployment_id)
    REFERENCES deployments(tenant_id, app_id, id);

CREATE TABLE deployment_queries (
    tenant_id uuid NOT NULL,
    deployment_id uuid NOT NULL,
    query_revision_id uuid NOT NULL,
    PRIMARY KEY (tenant_id, deployment_id, query_revision_id),
    FOREIGN KEY (tenant_id, deployment_id)
        REFERENCES deployments(tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, query_revision_id)
        REFERENCES query_revisions(tenant_id, id)
);

CREATE TABLE runtime_login_codes (
    code_hash bytea PRIMARY KEY CHECK (octet_length(code_hash) = 32),
    tenant_id uuid NOT NULL,
    app_id uuid NOT NULL,
    user_id uuid NOT NULL,
    expires_at timestamptz NOT NULL,
    FOREIGN KEY (tenant_id, app_id)
        REFERENCES apps(tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships(tenant_id, user_id) ON DELETE CASCADE
);

CREATE TABLE runtime_sessions (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    tenant_id uuid NOT NULL,
    app_id uuid NOT NULL,
    user_id uuid NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (tenant_id, app_id)
        REFERENCES apps(tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, user_id)
        REFERENCES memberships(tenant_id, user_id) ON DELETE CASCADE
);

CREATE TABLE jobs (
    id uuid PRIMARY KEY,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('dataset_import', 'connector_sync', 'app_publish')),
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    sync_run_id uuid,
    deployment_id uuid,
    status text NOT NULL CHECK (status IN ('awaiting_upload', 'queued', 'running', 'succeeded', 'failed', 'cancelled')),
    available_at timestamptz NOT NULL DEFAULT now(),
    leased_by text,
    lease_expires_at timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error text,
    cancel_requested_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id),
    UNIQUE (tenant_id, kind, idempotency_key),
    UNIQUE (tenant_id, sync_run_id),
    UNIQUE (tenant_id, deployment_id),
    FOREIGN KEY (tenant_id, sync_run_id)
        REFERENCES sync_runs(tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (tenant_id, deployment_id)
        REFERENCES deployments(tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT jobs_resource_check CHECK (
        (kind IN ('dataset_import', 'connector_sync') AND sync_run_id IS NOT NULL AND deployment_id IS NULL) OR
        (kind = 'app_publish' AND sync_run_id IS NULL AND deployment_id IS NOT NULL)
    )
);

CREATE INDEX jobs_claim_idx
    ON jobs (available_at, created_at)
    WHERE status = 'queued';

CREATE INDEX jobs_tenant_created_idx
    ON jobs (tenant_id, created_at DESC);

CREATE TABLE job_logs (
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    job_id uuid NOT NULL,
    sequence bigint NOT NULL,
    level text NOT NULL CHECK (level IN ('info', 'warn', 'error')),
    message text NOT NULL CHECK (length(message) BETWEEN 1 AND 2000),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, job_id, sequence),
    FOREIGN KEY (tenant_id, job_id)
        REFERENCES jobs(tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX job_logs_job_created_idx
    ON job_logs (tenant_id, job_id, sequence);
