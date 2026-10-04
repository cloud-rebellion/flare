import type {
  AlertRule,
  CreateAlertRuleInput,
  ApiKey,
  ApiKeyCreated,
  ApiKeyRole,
  PartialErasure,
  Artifact,
  AiConfig,
  AuditEntry,
  Channel,
  GithubConfig,
  Invite,
  Member,
  OidcConfig,
  Release,
  ReleaseFreshness,
  ClientHealth,
  BusinessHealth,
  BusinessCheckKey,
  BusinessCheckKeyCreated,
  ConnectorConnection,
  ConnectorCheckResult,
  ConnectorSyncRun,
  CreateConnectorConnectionInput,
  UpdateConnectorConnectionInput,
  ConnectorQuotaHealth,
  ConnectorSyncHealth,
  Issue,
  IssueEvent,
  LogRow,
  LogPage,
  LogVolumeBucket,
  MetricName,
  MetricPoint,
  Monitor,
  Overview,
  Project,
  Span,
  TraceSummary,
  User
} from './types';

let csrfToken: string | null = null;

async function csrf(force = false): Promise<string> {
  if (csrfToken !== null && !force) return csrfToken;
  const res = await fetch('/api/csrf', { credentials: 'same-origin' });
  const data = await res.json();
  csrfToken = data.csrf_token ?? '';
  return csrfToken!;
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function req<T>(method: string, path: string, body?: unknown, retriedCSRF = false): Promise<T> {
  const headers: Record<string, string> = {};
  const opts: RequestInit = { method, credentials: 'same-origin', headers };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  if (method !== 'GET') {
    headers['X-CSRF-Token'] = await csrf();
  }
  const res = await fetch('/api' + path, opts);
  // The CSRF cookie outlives nothing: it expires while a long-lived tab is
  // still open, and the token was cached for the page's whole lifetime, so
  // every write failed until a manual reload. Refetch once and retry.
  // Only retry a CSRF rejection, not an RBAC one. Retrying every 403 replayed
  // authorization-denied writes a second time, which doubles the server-side
  // work and the audit noise for a viewer clicking a button they cannot use.
  // gorilla/csrf answers "Forbidden - CSRF token invalid".
  if (res.status === 403 && method !== 'GET' && !retriedCSRF) {
    const body403 = await res.clone().text();
    if (/csrf/i.test(body403)) {
      await csrf(true);
      return req<T>(method, path, body, true);
    }
  }
  // An expired or revoked session used to surface as a red error banner on
  // whatever page the user was on, with no way forward. Send them to the login
  // page instead. /auth/* is excluded: those endpoints answer 401 as a normal
  // result (bad password, not-signed-in probe on load) and handle it locally.
  if (res.status === 401 && !path.startsWith('/auth/') && typeof window !== 'undefined') {
    csrfToken = null; // a new session issues a new CSRF token
    if (window.location.pathname !== '/login') {
      window.location.href = '/login?error=session_expired';
    }
  }
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const e = await res.json();
      if (e.error) msg = e.error;
    } catch {
      /* keep statusText */
    }
    throw new ApiError(res.status, msg);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const api = {
  me: () => req<User>('GET', '/auth/me'),
  login: (email: string, password: string) => req<User>('POST', '/auth/login', { email, password }),
  register: (email: string, password: string, org_name: string) =>
    req<User>('POST', '/auth/register', { email, password, org_name }),
  logout: () => req<void>('POST', '/auth/logout'),
  forgotPassword: (email: string) => req<void>('POST', '/auth/forgot-password', { email }),
  resetPassword: (token: string, password: string) =>
    req<void>('POST', '/auth/reset-password', { token, password }),
  acceptInvite: (token: string, password: string) =>
    req<User>('POST', '/auth/accept-invite', { token, password }),

  githubConfig: () => req<GithubConfig>('GET', '/integrations/github'),
  setGithubConfig: (repo: string, token: string) =>
    req<GithubConfig>('PUT', '/integrations/github', { repo, token }),
  deleteGithubConfig: () => req<void>('DELETE', '/integrations/github'),
  createGithubIssue: (issueId: string) =>
    req<{ github_url: string }>('POST', `/issues/${issueId}/github`),

  aiConfig: () => req<AiConfig>('GET', '/integrations/ai'),
  setAiConfig: (cfg: {
    base_url: string;
    api_key: string;
    model: string;
    format: string;
    enabled: boolean;
    auto_triage: boolean;
    triage_daily_budget: number;
  }) => req<AiConfig>('PUT', '/integrations/ai', cfg),
  deleteAiConfig: () => req<void>('DELETE', '/integrations/ai'),
  triageIssue: (issueId: string, refresh = false) =>
    req<{ triage: string; cached: boolean; evidence_digest?: string }>('POST', `/issues/${issueId}/triage${refresh ? '?refresh=true' : ''}`),

  oidcConfig: () => req<OidcConfig>('GET', '/integrations/oidc'),
  setOidcConfig: (cfg: {
    issuer: string;
    client_id: string;
    client_secret: string;
    default_role: string;
    enabled: boolean;
  }) => req<OidcConfig>('PUT', '/integrations/oidc', cfg),
  deleteOidcConfig: () => req<void>('DELETE', '/integrations/oidc'),

  auditLog: () => req<AuditEntry[]>('GET', '/audit-log'),
  exportData: () => req<Record<string, unknown>>('GET', '/export'),
  // Returns a body ONLY when the erasure was partial (202): the hot tier is
  // erased but an S3 cold tier still holds aged telemetry. A 204 means fully
  // erased. Callers must read this rather than assuming success.
  changePassword: (current_password: string, new_password: string) =>
    req<{ status: string }>('POST', '/auth/password', { current_password, new_password }),

  deleteOrg: () => req<PartialErasure | void>('DELETE', '/org'),

  members: () => req<Member[]>('GET', '/members'),
  updateMemberRole: (userId: string, role: string) =>
    req<void>('PATCH', `/members/${userId}`, { role }),
  removeMember: (userId: string) => req<void>('DELETE', `/members/${userId}`),
  invites: () => req<Invite[]>('GET', '/invites'),
  inviteMember: (email: string, role: string) => req<Invite>('POST', '/invites', { email, role }),
  revokeInvite: (inviteId: string) => req<void>('DELETE', `/invites/${inviteId}`),

  projects: () => req<Project[]>('GET', '/projects'),
  createProject: (name: string, platform: string) =>
    req<Project>('POST', '/projects', { name, platform }),
  project: (id: string) => req<Project>('GET', `/projects/${id}`),
  // Resolves a provisioner deep-link's numeric DSN id behind the session, so
  // /go/<dsnID> is no longer an unauthenticated existence oracle.
  projectByDsnID: (dsnID: string) => req<Project>('GET', `/projects/by-dsn/${dsnID}`),
  deleteProject: (id: string) => req<PartialErasure | void>('DELETE', `/projects/${id}`),

  issues: (pid: string, status?: string, opts?: { q?: string; limit?: number; offset?: number }) => {
    const p = new URLSearchParams();
    if (status) p.set('status', status);
    if (opts?.q) p.set('q', opts.q);
    if (opts?.limit != null) p.set('limit', String(opts.limit));
    if (opts?.offset) p.set('offset', String(opts.offset));
    const qs = p.toString();
    return req<{ issues: Issue[]; total: number }>(
      'GET',
      `/projects/${pid}/issues${qs ? `?${qs}` : ''}`
    );
  },
  issue: (id: string) => req<Issue>('GET', `/issues/${id}`),
  issueEvents: (id: string) => req<IssueEvent[]>('GET', `/issues/${id}/events`),
  setIssueStatus: (id: string, status: string) => req<Issue>('PATCH', `/issues/${id}`, { status }),

  metrics: (pid: string) => req<MetricName[]>('GET', `/projects/${pid}/metrics`),
  metricSeries: (pid: string, name: string, windowMin = 60) =>
    req<MetricPoint[]>(
      'GET',
      `/projects/${pid}/metrics/query?name=${encodeURIComponent(name)}&window=${windowMin}`
    ),
  businessHealth: (pid: string, hours = 24, limit = 100) =>
    req<BusinessHealth[]>('GET', `/projects/${pid}/business-health?hours=${hours}&limit=${limit}`),
  businessKeys: (pid: string) =>
    req<BusinessCheckKey[]>('GET', `/projects/${pid}/business-keys`),
  createBusinessKey: (pid: string, name: string, expiresInDays: number) =>
    req<BusinessCheckKeyCreated>('POST', `/projects/${pid}/business-keys`, {
      name,
      expires_in_days: expiresInDays
    }),
  revokeBusinessKey: (pid: string, keyID: string) =>
    req<void>('DELETE', `/projects/${pid}/business-keys/${keyID}`),

  logs: (
    pid: string,
    opts: {
      q?: string;
      severity?: string;
      trace?: string;
      /** Window size in hours. Without it the server returns the newest records
       *  from the whole retained history. */
      hours?: number;
      /** Keyset paging cursor: the observed_at of the oldest row already shown. */
      before?: string;
      limit?: number;
    } = {}
  ) => {
    const p = new URLSearchParams();
    if (opts.q) p.set('q', opts.q);
    if (opts.severity) p.set('severity', opts.severity);
    if (opts.trace) p.set('trace_id', opts.trace);
    if (opts.hours) p.set('hours', String(opts.hours));
    if (opts.before) p.set('before', opts.before);
    if (opts.limit) p.set('limit', String(opts.limit));
    const qs = p.toString();
    return req<LogPage>('GET', `/projects/${pid}/logs${qs ? `?${qs}` : ''}`);
  },

  overview: () => req<Overview>('GET', '/overview'),

  logVolume: (pid: string, hours = 24) =>
    req<LogVolumeBucket[]>('GET', `/projects/${pid}/analytics/log-volume?hours=${hours}`),

  releases: (pid: string) => req<Release[]>('GET', `/projects/${pid}/releases`),
  releaseFreshness: (pid: string, hours = 24, limit = 100) =>
    req<ReleaseFreshness[]>('GET', `/projects/${pid}/release-freshness?hours=${hours}&limit=${limit}`),
  clientHealth: (pid: string, hours = 24, limit = 100) =>
    req<ClientHealth[]>('GET', `/projects/${pid}/client-health?hours=${hours}&limit=${limit}`),
  connectorSyncHealth: (limit = 100) =>
    req<ConnectorSyncHealth[]>('GET', `/connector-sync-health?limit=${limit}`),
  connectorQuotaHealth: (hours = 24, limit = 100) =>
    req<ConnectorQuotaHealth[]>('GET', `/connector-quota-health?hours=${hours}&limit=${limit}`),
  projectConnectorQuotaHealth: (pid: string, hours = 24, limit = 100) =>
    req<ConnectorQuotaHealth[]>('GET', `/projects/${pid}/connector-quota-health?hours=${hours}&limit=${limit}`),
  connectorConnections: () => req<ConnectorConnection[]>('GET', '/connector-connections'),
  connectorConnection: (id: string) => req<ConnectorConnection>('GET', `/connector-connections/${id}`),
  createConnectorConnection: (input: CreateConnectorConnectionInput) =>
    req<ConnectorConnection>('POST', '/connector-connections', input),
  updateConnectorConnection: (id: string, input: UpdateConnectorConnectionInput) =>
    req<ConnectorConnection>('PATCH', `/connector-connections/${id}`, input),
  deleteConnectorConnection: (id: string) => req<void>('DELETE', `/connector-connections/${id}`),
  checkConnectorConnection: (id: string) =>
    req<ConnectorCheckResult>('POST', `/connector-connections/${id}/check`),
  runConnectorSync: (id: string) =>
    req<ConnectorSyncRun>('POST', `/connector-connections/${id}/sync`),
  createRelease: (pid: string, version: string) =>
    req<Release>('POST', `/projects/${pid}/releases`, { version }),

  traces: (pid: string) => req<TraceSummary[]>('GET', `/projects/${pid}/traces`),
  trace: (pid: string, traceID: string) => req<Span[]>('GET', `/projects/${pid}/traces/${traceID}`),

  apiKeys: () => req<ApiKey[]>('GET', '/keys'),
  createApiKey: (name: string, role: ApiKeyRole = 'viewer') =>
    req<ApiKeyCreated>('POST', '/keys', { name, role }),
  deleteApiKey: (id: string) => req<void>('DELETE', `/keys/${id}`),

  channels: () => req<Channel[]>('GET', '/channels'),
  createChannel: (type: string, config: Record<string, unknown>) =>
    req<Channel>('POST', '/channels', { type, config }),
  testChannel: (id: string, projectId?: string) => req<{ ok: boolean; error?: string }>(
    'POST', `/channels/${id}/test`, projectId ? { project_id: projectId } : undefined
  ),
  deleteChannel: (id: string) => req<void>('DELETE', `/channels/${id}`),
  /** Mute/unmute a channel, or set its project routing. Omitted fields are
   *  left alone; project_ids: [] means "all projects". */
  updateChannel: (id: string, patch: { enabled?: boolean; project_ids?: string[] }) =>
    req<{ status: string }>('PATCH', `/channels/${id}`, patch),

  alertRules: (pid: string) => req<AlertRule[]>('GET', `/projects/${pid}/alert-rules`),
  createAlertRule: (pid: string, rule: CreateAlertRuleInput) =>
    req<AlertRule>('POST', `/projects/${pid}/alert-rules`, rule),
  deleteAlertRule: (pid: string, ruleId: string) =>
    req<void>('DELETE', `/projects/${pid}/alert-rules/${ruleId}`),

  monitors: (pid: string) => req<Monitor[]>('GET', `/projects/${pid}/monitors`),
  createMonitor: (
    pid: string,
    body: { slug: string; name: string; interval_seconds: number; grace_seconds: number }
  ) => req<Monitor>('POST', `/projects/${pid}/monitors`, body),
  updateMonitor: (
    id: string,
    body: { name: string; interval_seconds: number; grace_seconds: number }
  ) => req<Monitor>('PATCH', `/monitors/${id}`, body),
  deleteMonitor: (id: string) => req<void>('DELETE', `/monitors/${id}`),

  artifacts: (pid: string) => req<Artifact[]>('GET', `/projects/${pid}/artifacts`),
  uploadSourceMap: (pid: string, release: string, name: string, content: string) =>
    req<Artifact>('POST', `/projects/${pid}/artifacts`, { release, name, content }),
  deleteArtifact: (pid: string, artifactId: string) =>
    req<void>('DELETE', `/projects/${pid}/artifacts/${artifactId}`)
};
