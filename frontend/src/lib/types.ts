export interface User {
  id: string;
  email: string;
  org_id: string;
  role: string;
}

export interface Project {
  id: string;
  name: string;
  slug: string;
  platform: string;
  dsn: string;
  otlp_endpoint: string;
}

export interface Issue {
  id: string;
  project_id: string;
  title: string;
  culprit: string;
  level: string;
  status: string;
  platform: string;
  first_seen: string;
  last_seen: string;
  event_count: number;
  github_url: string;
  first_release: string;
  ai_triage: string;
  sensitive: string;
}

export interface AiConfig {
  enabled: boolean;
  base_url: string;
  model: string;
  format: string;
  auto_triage: boolean;
  triage_daily_budget: number;
}

export interface Release {
  version: string;
  created_at: string;
  new_issues: number;
}

export interface ReleaseFreshness {
  version: string;
  deploy_marked_at?: string;
  last_heartbeat_at?: string;
  age_seconds: number;
}

export interface ClientHealth {
  platform: string;
  environment: string;
  release: string;
  event_count: number;
  error_count: number;
  error_rate: number;
  affected_issue_count: number;
  first_seen: string | null;
  last_seen: string | null;
}

export interface BusinessHealth {
  check_name: string;
  attempts: number;
  ok_count: number;
  error_count: number;
  error_rate: number;
  avg_elapsed_ms: number;
  max_elapsed_ms: number;
  last_completed_at?: string;
  last_ok_at?: string;
  last_error_at?: string;
}

/** Metadata only. The confidential key is returned once on creation. */
export interface BusinessCheckKey {
  id: string;
  name: string;
  prefix: string;
  created_at: string;
  expires_at: string;
  revoked_at?: string | null;
}

export interface BusinessCheckKeyCreated extends BusinessCheckKey {
  key: string;
}

export interface ConnectorQuotaHealth {
  connection_id?: string;
  provider: string;
  operation: string;
  channel: string;
  quota_points: number;
  retry_points: number;
  sync_lag_points: number;
  quota_remaining?: number;
  quota_observed_at?: string;
  retry_after_ms?: number;
  retry_observed_at?: string;
  sync_lag_ms?: number;
  sync_lag_observed_at?: string;
  last_completed_at?: string;
}

export type ConnectorConnectionStatus = 'pending' | 'active' | 'paused' | 'error' | 'revoked';

/** Tenant-scoped connector metadata. Credentials are write-only and are never returned. */
export interface ConnectorConnection {
  id: string;
  provider: string;
  installation_id: string;
  installation_name: string;
  status: ConnectorConnectionStatus;
  scopes: string[];
  credential_configured: boolean;
  created_at: string;
  updated_at: string;
}

export interface CreateConnectorConnectionInput {
  provider: string;
  installation_id: string;
  installation_name?: string;
  status?: ConnectorConnectionStatus;
  scopes?: string[];
  /** Write-only credential; the API never includes it in a response. */
  secret?: string;
}

export interface UpdateConnectorConnectionInput {
  provider?: string;
  installation_id?: string;
  installation_name?: string;
  status?: ConnectorConnectionStatus;
  scopes?: string[];
  /** Omit to preserve the existing credential; supplying it rotates the credential. */
  secret?: string;
}

export interface ConnectorSyncHealth {
  id: string;
  connection_id: string;
  provider: string;
  installation_name?: string;
  connection_status: ConnectorConnectionStatus;
  operation: string;
  state: 'idle' | 'running' | 'succeeded' | 'failed';
  cursor_configured: boolean;
  cursor_version: number;
  attempt_count: number;
  last_started_at?: string;
  last_completed_at?: string;
  last_success_at?: string;
  last_error_code?: string;
  last_sync_lag_ms?: number;
  next_run_at?: string;
  lease_active: boolean;
  lease_expires_at?: string;
  updated_at?: string;
}

export interface ConnectorCheckResult {
  provider: string;
  operation: string;
  outcome: 'ok' | 'error';
  status_class: string;
  receipt_status: 'accepted' | 'unavailable' | 'unscoped' | 'invalid';
  error_code?: string;
  quota_remaining?: number;
  retry_after_ms?: number;
}

/** Result of one explicit, lease-bounded connector worker pass. */
export interface ConnectorSyncRun {
  operation: string;
  state: 'idle' | 'running' | 'succeeded' | 'failed';
  outcome?: 'ok' | 'error';
  status_class?: string;
  error_code?: string;
  claimed: boolean;
  skipped: boolean;
  skip_reason?: 'lease_busy';
  next_run_at?: string;
}

export interface GithubConfig {
  configured: boolean;
  repo: string;
}

export interface AuditEntry {
  action: string;
  target: string;
  actor: string;
  created_at: string;
}

export interface OidcConfig {
  enabled: boolean;
  issuer: string;
  client_id: string;
  default_role: string;
  redirect_uri: string;
  login_url: string;
}

export interface Frame {
  filename?: string;
  function?: string;
  module?: string;
  lineno?: number;
  colno?: number;
  in_app?: boolean;
  context_line?: string;
  pre_context?: string[];
  post_context?: string[];
  symbolicated?: boolean;
}

export interface Artifact {
  id: string;
  release: string;
  name: string;
  size: number;
  parse_status: 'unvalidated' | 'valid' | 'invalid_json' | 'invalid_mappings' | 'unsupported';
  created_at: string;
}

export interface Member {
  id: string;
  email: string;
  role: string;
  is_you: boolean;
  created_at: string;
}

export interface Invite {
  id: string;
  email: string;
  role: string;
  expires_at: string;
  created_at: string;
  accept_url?: string;
  emailed?: boolean;
}

export interface IssueEvent {
  id: string;
  level: string;
  message: string;
  exception_type: string;
  exception_value: string;
  platform: string;
  environment: string;
  release: string;
  stacktrace: { frames: Frame[] } | null;
  trace_id: string;
  span_id: string;
  received_at: string;
}

export interface LogRow {
  id: string;
  severity: string;
  body: string;
  attributes: Record<string, unknown> | null;
  trace_id: string;
  span_id: string;
  observed_at: string;
}

export interface TraceSummary {
  trace_id: string;
  root_name: string;
  span_count: number;
  has_error: boolean;
  duration_ms: number;
  started: string;
}

export type ApiKeyRole = 'viewer' | 'member';

export interface ApiKey {
  id: string;
  name: string;
  prefix: string;
  role: ApiKeyRole;
  /** Email of the user who minted it. Empty for keys created before the
   *  creator column existed, which a password reset will NOT revoke. */
  created_by: string;
  created_at: string;
  last_used_at: string | null;
}

export interface ApiKeyCreated {
  id: string;
  name: string;
  prefix: string;
  role: ApiKeyRole;
  key: string;
}

export interface MetricName {
  name: string;
  kind: string;
  points: number;
  last_seen: string;
}

export interface MetricPoint {
  value: number;
  labels: Record<string, unknown> | null;
  observed_at: string;
}

export interface Span {
  span_id: string;
  parent_span_id: string;
  name: string;
  kind: string;
  status: string;
  start_unix_ms: number;
  duration_ms: number;
  attributes: Record<string, unknown> | null;
}

export interface LogVolumeBucket {
  hour: string;
  severity: string;
  count: number;
}

export type AlertChannelType = 'log' | 'email' | 'slack' | 'webhook' | 'automation_webhook';

export interface Channel {
  id: string;
  type: AlertChannelType;
  config: Record<string, unknown>;
  enabled: boolean;
  last_attempt_at: string | null;
  last_ok_at: string | null;
  last_error: string;
  /** Projects this channel is routed to. EMPTY MEANS ALL PROJECTS, which is
   *  both the default and the behaviour before routing existed. */
  project_ids: string[];
}

export interface Monitor {
  id: string;
  slug: string;
  name: string;
  interval_seconds: number;
  grace_seconds: number;
  last_ping_at: string | null;
  last_status: string;
  state: string; // new | ok | missing | failed
  checkin_url: string;
}

export interface AlertRule {
  id: string;
  name: string;
  type: AlertRuleType;
  threshold: number;
  window_minutes: number;
  metric_name?: string;
  metric_labels?: Record<string, string>;
  business_check_name?: string;
  comparison?: AlertRuleComparison;
  threshold_value?: number;
  span_name?: string;
  percentile?: number;
  connector_provider?: string;
  connector_operation?: string;
  connector_channel?: string;
  connector_error_code?: string;
  connector_connection_id?: string;
  connector_sync_mode?: AlertConnectorSyncMode;
  connector_quota_metric?: ConnectorQuotaMetric;
  release_version?: string;
  service_name?: string;
  log_severity?: AlertRuleSeverity;
  usage_surface?: AlertUsageSurface;
  usage_operation?: string;
  usage_measure?: AlertUsageMeasure;
  api_key_hygiene_mode?: AlertAPIKeyHygieneMode;
  ai_triage_mode?: AlertAITriageMode;
  enabled: boolean;
}

/** Alert conditions accepted by the project alert-rules API. */
export type AlertRuleType =
  | 'new_issue'
  | 'regression'
  | 'spike'
  | 'anomaly'
  | 'silence'
  | 'metric_threshold'
  | 'trace_percentile'
  | 'service_level'
  | 'business_health'
  | 'connector_error_rate'
  | 'connector_quota'
  | 'connector_freshness'
  | 'connector_sync_health'
  | 'service_freshness'
  | 'release_error_rate'
  | 'release_freshness'
  | 'deploy_failure'
  | 'deploy_acceptance'
  | 'rollback'
  | 'log_severity'
  | 'usage_threshold'
  | 'api_key_hygiene'
  | 'ai_triage'
  | 'ingest_acceptance';

export type AlertRuleComparison = 'gt' | 'gte' | 'lt' | 'lte';
export type AlertRuleSeverity = 'trace' | 'debug' | 'info' | 'warn' | 'error' | 'fatal';
export type ConnectorQuotaMetric = 'quota_remaining' | 'retry_after_ms' | 'sync_lag_ms';
export type AlertConnectorSyncMode = 'failed' | 'overdue';
export type AlertUsageSurface = 'dsn' | 'business_key' | 'api_key' | 'mcp';
export type AlertUsageMeasure = 'requests' | 'errors' | 'denied';
export type AlertAPIKeyHygieneMode = 'unused' | 'expiring';
export type AlertAITriageMode = 'failed' | 'stuck';

export interface CreateAlertRuleInput {
  name: string;
  type: AlertRuleType;
  threshold?: number;
  window_minutes?: number;
  metric_name?: string;
  metric_labels?: Record<string, string>;
  business_check_name?: string;
  comparison?: AlertRuleComparison;
  threshold_value?: number;
  span_name?: string;
  percentile?: number;
  connector_provider?: string;
  connector_operation?: string;
  connector_channel?: string;
  connector_error_code?: string;
  connector_connection_id?: string;
  connector_sync_mode?: AlertConnectorSyncMode;
  connector_quota_metric?: ConnectorQuotaMetric;
  release_version?: string;
  service_name?: string;
  log_severity?: AlertRuleSeverity;
  usage_surface?: AlertUsageSurface;
  usage_operation?: string;
  usage_measure?: AlertUsageMeasure;
  api_key_hygiene_mode?: AlertAPIKeyHygieneMode;
  ai_triage_mode?: AlertAITriageMode;
}

export interface OverviewHourBucket {
  hour: string;
  count: number;
}

export interface OverviewIssue {
  id: string;
  title: string;
  level: string;
  event_count: number;
  project_id: string;
  project_name: string;
}

export type ProjectHealth = 'healthy' | 'alert' | 'spike' | 'silent';

export interface OverviewProject {
  id: string;
  name: string;
  slug: string;
  events_24h: number;
  unresolved: number;
  status: ProjectHealth;
  /** Newest signal of any level (heartbeats included), unix seconds. 0 = no pulse in 7d. */
  last_seen_unix: number;
  volume: number[];
}

export interface Overview {
  events_24h: number;
  unresolved: number;
  new_today: number;
  volume: OverviewHourBucket[];
  top_issues: OverviewIssue[];
  projects: OverviewProject[];
  alerting_blind: boolean;
}

/** Body of a 202 from an erasure endpoint: the request was accepted but not
 *  fully carried out. Aged telemetry remains in the object store and only an
 *  operator can remove it, so this must be shown, not swallowed. */
export interface PartialErasure {
  status: 'partial';
  hot_tier: string;
  cold_tier: string;
  cold_tier_note: string;
}

/** A page of logs. next_before is absent when there are no older records, which
 *  is what lets the UI tell "end of data" from "this page happened to be short". */
export interface LogPage {
  logs: LogRow[];
  next_before?: string;
}
