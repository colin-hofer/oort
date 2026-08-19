// Typed client for the Oort control-plane API. Authentication rides on the
// HttpOnly session cookie; mutations carry the header the server requires as
// its CSRF check.

export type User = {id: string; email: string};

export type Tenant = {id: string; slug: string; role: string; created_at: string};

export type Dataset = {
  id: string;
  slug: string;
  current_snapshot_id?: number;
  schema?: {name: string; type: string}[] | null;
  row_count?: number;
  byte_count?: number;
  last_sync_status?: string;
  created_at: string;
  updated_at: string;
};

export type QueryRevision = {
  id: string;
  query_id: string;
  slug: string;
  version: number;
  sql: string;
  parameter_types: Record<string, string>;
  created_at: string;
};

export type App = {
  id: string;
  slug: string;
  current_deployment_id?: string;
  current_version?: number;
  current_status?: string;
  url?: string;
  created_at: string;
  updated_at: string;
};

export type Deployment = {
  id: string;
  app_id: string;
  app_slug: string;
  version: number;
  status: string;
  byte_count: number;
  error?: string;
  created_at: string;
  published_at?: string;
};

export type DatasetSync = {
  id: string;
  dataset_id: string;
  dataset_slug: string;
  status: string;
  format: string;
  snapshot_id?: number;
  row_count?: number;
  byte_count: number;
  error?: string;
  created_at: string;
  started_at?: string;
  finished_at?: string;
};

export type Job = {
  id: string;
  kind: 'dataset_import' | 'connector_sync' | 'app_publish';
  status: string;
  dataset_slug?: string;
  app_slug?: string;
  sync_id?: string;
  deployment_id?: string;
  attempts: number;
  error?: string;
  cancel_requested_at?: string;
  created_at: string;
  updated_at: string;
};

export type JobLog = {sequence: number; level: string; message: string; created_at: string};

export type Connector = {
  id: string;
  dataset_id: string;
  dataset_slug: string;
  slug: string;
  url: string;
  records_pointer: string;
  cursor_parameter?: string;
  next_cursor_pointer?: string;
  auth_configured: boolean;
  enabled: boolean;
  refresh_minutes: number;
  next_sync_at: string;
  last_status?: string;
  last_error?: string;
  last_finished_at?: string;
  created_at: string;
  updated_at: string;
};

export type Member = {
  user_id: string;
  email: string;
  display_name?: string;
  role: 'owner' | 'admin' | 'developer' | 'viewer';
  created_at: string;
};

export type Invitation = {
  id: string;
  tenant: string;
  email: string;
  role: Member['role'];
  invited_by_user_id?: string;
  expires_at: string;
  created_at: string;
  status: 'pending' | 'expired';
};

export type AddMemberOutcome =
  | {outcome: 'member_added'; member: Member}
  | {outcome: 'invitation_created'; invitation: Invitation; accept_url: string};

export type RenewInvitationOutcome = {
  outcome: 'invitation_renewed';
  invitation: Invitation;
  accept_url: string;
};

export type ApiToken = {
  id: string;
  name: string;
  user_id: string;
  scopes: string[];
  expires_at: string;
  created_at: string;
  last_used_at?: string;
  revoked_at?: string;
};

export type ActivityEvent = {
  id: string;
  action: string;
  resource_type: string;
  resource_id: string;
  request_id: string;
  created_at: string;
};

export type Dashboard = {
  tenant: Tenant;
  datasets: Dataset[];
  queries: QueryRevision[];
  apps: App[];
  deployments: Deployment[];
  syncs: DatasetSync[];
  activity: ActivityEvent[];
};

export type QueryResult = {
  columns: {name: string; type: string}[];
  rows: unknown[][];
  snapshot_id: number;
  truncated: boolean;
};

export class ApiError extends Error {
  status: number;
  code?: string;
  requestId?: string;

  constructor(status: number, message: string, code?: string, requestId?: string) {
    super(message);
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

type Options = Omit<RequestInit, 'body'> & {body?: unknown};

async function request<T>(path: string, init: Options = {}): Promise<T> {
  const headers = new Headers(init.headers);
  headers.set('X-Oort-Request', 'browser');
  let body = init.body as BodyInit | null | undefined;
  if (init.body != null && !(init.body instanceof Blob) && typeof init.body !== 'string') {
    headers.set('Content-Type', 'application/json');
    body = JSON.stringify(init.body);
  }
  const response = await fetch(path, {credentials: 'same-origin', ...init, headers, body});
  const text = await response.text();
  let payload: any = null;
  try {
    payload = text ? JSON.parse(text) : null;
  } catch {
    payload = null;
  }
  if (!response.ok) {
    throw new ApiError(
      response.status,
      payload?.error?.message || `Request failed with HTTP ${response.status}`,
      payload?.error?.code,
      payload?.error?.request_id,
    );
  }
  return payload as T;
}

const tenantPath = (tenant: string, rest = '') => `/v1/tenants/${encodeURIComponent(tenant)}${rest}`;

export const api = {
  me: () => request<{user: User}>('/v1/me'),

  logout: () => request<void>('/v1/control-session', {method: 'DELETE'}),

  connect: (token: string) =>
    request<{user: User}>('/v1/control-session', {
      method: 'POST',
      headers: {Authorization: `Bearer ${token}`},
      body: {},
    }),

  listTenants: () => request<{tenants: Tenant[]}>('/v1/tenants'),

  createTenant: (slug: string) => request<{tenant: Tenant}>('/v1/tenants', {method: 'POST', body: {slug}}),

  dashboard: (tenant: string) => request<Dashboard>(tenantPath(tenant, '/dashboard')),

  uploadDataset: async (tenant: string, slug: string, file: File) => {
    const format = file.name.toLowerCase().endsWith('.parquet') ? 'parquet' : 'csv';
    const created = await request<{upload: {id: string}; job: Job}>(tenantPath(tenant, '/dataset-uploads'), {
      method: 'POST',
      body: {slug, format, byte_count: file.size, idempotency_key: crypto.randomUUID()},
    });
    const uploadPath = `/dataset-uploads/${encodeURIComponent(created.upload.id)}/content`;
    return request<{job: Job}>(tenantPath(tenant, uploadPath), {
      method: 'PUT',
      headers: {'Content-Type': 'application/octet-stream'},
      body: file,
    });
  },

  deleteDataset: (tenant: string, slug: string) =>
    request<void>(tenantPath(tenant, `/datasets/${encodeURIComponent(slug)}`), {method: 'DELETE'}),

  executeDraftQuery: (tenant: string, sql: string, parameters: Record<string, unknown>) =>
    request<{result: QueryResult}>(tenantPath(tenant, '/queries/execute'), {
      method: 'POST',
      body: {sql, parameters},
    }),

  saveQuery: (tenant: string, slug: string, sql: string, parameters: Record<string, unknown>) =>
    request<{query: QueryRevision}>(tenantPath(tenant, `/queries/${encodeURIComponent(slug)}`), {
      method: 'PUT',
      body: {sql, parameters},
    }),

  deleteQuery: (tenant: string, slug: string) =>
    request<void>(tenantPath(tenant, `/queries/${encodeURIComponent(slug)}`), {method: 'DELETE'}),

  listJobs: (tenant: string) => request<{jobs: Job[]}>(tenantPath(tenant, '/jobs')),
  getJob: (tenant: string, id: string) => request<{job: Job}>(tenantPath(tenant, `/jobs/${encodeURIComponent(id)}`)),
  jobLogs: (tenant: string, id: string, after = 0) =>
    request<{logs: JobLog[]}>(tenantPath(tenant, `/jobs/${encodeURIComponent(id)}/logs?after=${after}`)),
  cancelJob: (tenant: string, id: string) =>
    request<{job: Job}>(tenantPath(tenant, `/jobs/${encodeURIComponent(id)}/cancel`), {method: 'POST', body: {}}),

  listConnectors: (tenant: string) => request<{connectors: Connector[]}>(tenantPath(tenant, '/connectors')),
  createConnector: (tenant: string, input: Record<string, unknown>) =>
    request<{connector: Connector}>(tenantPath(tenant, '/connectors'), {method: 'POST', body: input}),
  updateConnector: (tenant: string, slug: string, input: Record<string, unknown>) =>
    request<{connector: Connector}>(tenantPath(tenant, `/connectors/${encodeURIComponent(slug)}`), {
      method: 'PUT', body: input,
    }),
  syncConnector: (tenant: string, slug: string) =>
    request<{job: Job}>(tenantPath(tenant, `/connectors/${encodeURIComponent(slug)}/sync`), {method: 'POST', body: {}}),
  deleteConnector: (tenant: string, slug: string) =>
    request<void>(tenantPath(tenant, `/connectors/${encodeURIComponent(slug)}`), {method: 'DELETE'}),

  listMembers: (tenant: string) => request<{members: Member[]}>(tenantPath(tenant, '/members')),
  addMember: (tenant: string, email: string, role: Member['role']) =>
    request<AddMemberOutcome>(tenantPath(tenant, '/members'), {method: 'POST', body: {email, role}}),
  changeMemberRole: (tenant: string, user: string, role: Member['role']) =>
    request<{member: Member}>(tenantPath(tenant, `/members/${encodeURIComponent(user)}`), {method: 'PATCH', body: {role}}),
  removeMember: (tenant: string, user: string) =>
    request<void>(tenantPath(tenant, `/members/${encodeURIComponent(user)}`), {method: 'DELETE'}),

  listInvitations: (tenant: string) =>
    request<{invitations: Invitation[]}>(tenantPath(tenant, '/members/invitations')),
  renewInvitation: (tenant: string, invitation: string) =>
    request<RenewInvitationOutcome>(tenantPath(tenant, `/members/invitations/${encodeURIComponent(invitation)}/renew`), {
      method: 'POST', body: {},
    }),
  revokeInvitation: (tenant: string, invitation: string) =>
    request<void>(tenantPath(tenant, `/members/invitations/${encodeURIComponent(invitation)}`), {method: 'DELETE'}),

  listTokens: (tenant: string) => request<{tokens: ApiToken[]}>(tenantPath(tenant, '/tokens')),
  createToken: (tenant: string, name: string, scopes: string[], expiresInDays: number) =>
    request<{token: ApiToken; secret: string}>(tenantPath(tenant, '/tokens'), {
      method: 'POST', body: {name, scopes, expires_in_days: expiresInDays},
    }),
  revokeToken: (tenant: string, id: string) =>
    request<void>(tenantPath(tenant, `/tokens/${encodeURIComponent(id)}`), {method: 'DELETE'}),

  rollback: (tenant: string, deployment: string) =>
    request<{deployment: Deployment}>(
      tenantPath(tenant, `/deployments/${encodeURIComponent(deployment)}/rollback`),
      {method: 'POST', body: {}},
    ),

  deleteApp: (tenant: string, app: string) =>
    request<void>(tenantPath(tenant, `/apps/${encodeURIComponent(app)}`), {method: 'DELETE'}),

  appLoginLink: (tenant: string, app: string) =>
    request<{url: string}>(tenantPath(tenant, `/apps/${encodeURIComponent(app)}/login-link`), {
      method: 'POST',
      body: {},
    }),
};
