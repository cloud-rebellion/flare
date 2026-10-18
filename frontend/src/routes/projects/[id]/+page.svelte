<script lang="ts">
  import { onMount } from 'svelte';
  import ProjectTabs from '$lib/components/ProjectTabs.svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { relativeTime, levelColor } from '$lib/format';
  import type {
    AlertRule,
    AlertRuleComparison,
    AlertRuleSeverity,
    AlertRuleType,
    AlertAITriageMode,
    AlertAPIKeyHygieneMode,
    AlertConnectorSyncMode,
    AlertConnectorSourceKind,
    AlertUsageMeasure,
    AlertUsageSurface,
    Artifact,
    ConnectorQuotaMetric,
    CreateAlertRuleInput,
    Issue,
    Monitor,
    PartialErasure,
    Project
  } from '$lib/types';

  const id = $derived(page.params.id ?? '');

  let project = $state<Project | null>(null);
  let issues = $state<Issue[] | null>(null);
  let total = $state(0);
  let error = $state<string | null>(null);
  let status = $state('unresolved');
  let setupOpen = $state(false);
  // Issue list paging + search. Without these the list was capped at the
  // backend's default 50 with no way to reach anything past it.
  let search = $state('');
  let page_ = $state(0);
  const perPage = 50;
  const pageCount = $derived(Math.max(1, Math.ceil(total / perPage)));

  let rules = $state<AlertRule[]>([]);
  let ruleName = $state('');
  let hasChannel = $state(true); // assume true until loaded, so no flash
  let ruleType = $state<AlertRuleType>('new_issue');
  let ruleThreshold = $state(10);
  let ruleWindow = $state(5);
  let ruleMetricName = $state('');
  let ruleMetricLabels = $state('');
  let ruleBusinessCheckName = $state('');
  let ruleComparison = $state<AlertRuleComparison>('gte');
  let ruleThresholdValue = $state(0.95);
  let ruleSpanName = $state('');
  let rulePercentile = $state(95);
  let ruleConnectorProvider = $state('');
  let ruleConnectorOperation = $state('');
  let ruleConnectorChannel = $state('');
  let ruleConnectorErrorCode = $state('');
  let ruleConnectorConnectionID = $state('');
  let ruleConnectorSourceKind = $state<AlertConnectorSourceKind>('server_observed');
  let ruleConnectorSyncMode = $state<AlertConnectorSyncMode>('overdue');
  let ruleConnectorQuotaMetric = $state<ConnectorQuotaMetric>('quota_remaining');
  let ruleReleaseVersion = $state('');
  let ruleServiceName = $state('');
  let ruleLogSeverity = $state<AlertRuleSeverity>('error');
  let ruleUsageSurface = $state<AlertUsageSurface>('mcp');
  let ruleUsageOperation = $state('tools_call');
  let ruleUsageMeasure = $state<AlertUsageMeasure>('errors');
  let ruleAPIKeyHygieneMode = $state<AlertAPIKeyHygieneMode>('unused');
  let ruleAITriageMode = $state<AlertAITriageMode>('failed');
  let confirmName = $state('');
  let deleting = $state(false);
  let partialErasure = $state<PartialErasure | null>(null);

  let monitors = $state<Monitor[]>([]);
  let monSlug = $state('');
  let monName = $state('');
  let monEveryMin = $state(60);
  let monGraceMin = $state(5);
  let monErr = $state<string | null>(null);
  let monEdits = $state<Record<string, { every: number; grace: number }>>({});

  let artifacts = $state<Artifact[]>([]);
  let smRelease = $state('');
  let smName = $state('');
  let smFile = $state<File | null>(null);
  let smBusy = $state(false);

  const filters = ['unresolved', 'resolved', 'ignored', 'all'];
  const usageOperations: Record<AlertUsageSurface, string[]> = {
    dsn: ['envelope', 'event', 'logs', 'metrics', 'traces', 'connector_attempts', 'checkins', 'symbols', 'otlp_logs', 'otlp_traces', 'otlp_metrics'],
    business_key: ['business_checks'],
    api_key: ['read', 'write', 'mcp'],
    mcp: ['initialize', 'ping', 'tools_list', 'tools_call', 'tool_overview', 'tool_list_projects', 'tool_list_issues', 'tool_get_issue', 'tool_search_logs', 'tool_get_trace', 'tool_query_metrics', 'tool_update_issue_status', 'tool_triage_issue']
  };

  $effect(() => {
    if (session.loaded && !session.user) goto('/login', { replaceState: true });
  });

  onMount(async () => {
    try {
      project = await api.project(id);
      rules = await api.alertRules(id);
      monitors = await api.monitors(id);
      artifacts = await api.artifacts(id);
      hasChannel = (await api.channels()).some((c) => c.enabled);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load project';
    }
    loadIssues();
  });

  // Monotonic request id: a slow response from an earlier keystroke must never
  // overwrite the results of a newer one.
  let issuesSeq = 0;

  async function loadIssues() {
    const seq = ++issuesSeq;
    issues = null;
    try {
      const res = await api.issues(id, status === 'all' ? undefined : status, {
        q: search.trim() || undefined,
        limit: perPage,
        offset: page_ * perPage
      });
      if (seq !== issuesSeq) return; // superseded
      issues = res.issues;
      total = res.total;
      // Rows can be resolved or ignored between page loads, so the offset we
      // asked for may now be past the end. Step back instead of stranding the
      // user on an empty page.
      if (issues.length === 0 && page_ > 0) {
        page_ = Math.max(0, Math.min(page_ - 1, Math.ceil(total / perPage) - 1));
        loadIssues();
      }
    } catch (err) {
      if (seq !== issuesSeq) return;
      error = err instanceof ApiError ? err.message : 'Failed to load issues';
      issues = [];
    }
  }

  function setStatus(s: string) {
    status = s;
    page_ = 0;
    loadIssues();
  }

  // Debounced so typing does not fire a request per keystroke.
  let searchTimer: ReturnType<typeof setTimeout> | undefined;
  function onSearchInput() {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      page_ = 0;
      loadIssues();
    }, 250);
  }

  function goPage(delta: number) {
    const next = page_ + delta;
    if (next < 0 || next * perPage >= total) return;
    page_ = next;
    loadIssues();
  }

  async function addRule(e: SubmitEvent) {
    e.preventDefault();
    if (!ruleName.trim()) return;
    try {
      const rule: CreateAlertRuleInput = {
        name: ruleName,
        type: ruleType
      };
      switch (ruleType) {
        case 'spike':
        case 'anomaly':
        case 'silence':
          rule.threshold = ruleThreshold;
          rule.window_minutes = ruleWindow;
          break;
        case 'metric_threshold':
          rule.metric_name = ruleMetricName.trim();
          if (ruleMetricLabels.trim()) {
            const parsed = JSON.parse(ruleMetricLabels);
            if (!parsed || Array.isArray(parsed) || typeof parsed !== 'object') {
              throw new Error('Metric labels must be a JSON object');
            }
            rule.metric_labels = parsed as Record<string, string>;
          }
          rule.comparison = ruleComparison;
          rule.threshold_value = ruleThresholdValue;
          rule.window_minutes = ruleWindow;
          break;
        case 'trace_percentile':
          rule.span_name = ruleSpanName.trim();
          rule.comparison = ruleComparison;
          rule.threshold_value = ruleThresholdValue;
          rule.percentile = rulePercentile;
          rule.window_minutes = ruleWindow;
          break;
        case 'service_level':
          rule.span_name = ruleSpanName.trim();
          // Service-level rules reuse the bounded alert fields: threshold is
          // the latency objective in milliseconds and threshold_value is the
          // minimum availability target.
          rule.threshold = ruleThreshold;
          rule.threshold_value = ruleThresholdValue;
          rule.window_minutes = ruleWindow;
          break;
        case 'business_health':
          rule.business_check_name = ruleBusinessCheckName.trim();
          rule.comparison = 'gte';
          rule.threshold_value = ruleThresholdValue;
          rule.window_minutes = ruleWindow;
          break;
        case 'connector_error_rate':
          rule.connector_provider = ruleConnectorProvider.trim();
          rule.connector_source_kind = ruleConnectorSourceKind;
          rule.connector_operation = ruleConnectorOperation.trim() || undefined;
          rule.connector_channel = ruleConnectorChannel.trim() || undefined;
          rule.connector_error_code = ruleConnectorErrorCode.trim() || undefined;
          rule.connector_connection_id = ruleConnectorSourceKind === 'server_observed' ? ruleConnectorConnectionID.trim() || undefined : undefined;
          rule.comparison = ruleComparison;
          rule.threshold_value = ruleThresholdValue;
          rule.window_minutes = ruleWindow;
          break;
        case 'connector_quota':
          rule.connector_provider = ruleConnectorProvider.trim();
          rule.connector_source_kind = ruleConnectorSourceKind;
          rule.connector_operation = ruleConnectorOperation.trim() || undefined;
          rule.connector_channel = ruleConnectorChannel.trim() || undefined;
          rule.connector_connection_id = ruleConnectorSourceKind === 'server_observed' ? ruleConnectorConnectionID.trim() || undefined : undefined;
          rule.connector_quota_metric = ruleConnectorQuotaMetric;
          rule.comparison = ruleComparison;
          rule.threshold_value = ruleThresholdValue;
          rule.window_minutes = ruleWindow;
          break;
        case 'connector_freshness':
          rule.connector_provider = ruleConnectorProvider.trim();
          rule.connector_source_kind = ruleConnectorSourceKind;
          rule.connector_operation = ruleConnectorOperation.trim() || undefined;
          rule.connector_channel = ruleConnectorChannel.trim() || undefined;
          rule.connector_connection_id = ruleConnectorSourceKind === 'server_observed' ? ruleConnectorConnectionID.trim() || undefined : undefined;
          rule.window_minutes = ruleWindow;
          break;
        case 'connector_sync_health':
          rule.connector_provider = ruleConnectorProvider.trim();
          rule.connector_operation = ruleConnectorOperation.trim();
          rule.connector_connection_id = ruleConnectorConnectionID.trim();
          rule.connector_sync_mode = ruleConnectorSyncMode;
          rule.threshold = ruleThreshold;
          rule.window_minutes = ruleWindow;
          break;
        case 'release_error_rate':
          rule.release_version = ruleReleaseVersion.trim() || undefined;
          rule.comparison = ruleComparison;
          rule.threshold_value = ruleThresholdValue;
          rule.window_minutes = ruleWindow;
          break;
        case 'release_freshness':
          rule.release_version = ruleReleaseVersion.trim();
          rule.window_minutes = ruleWindow;
          break;
        case 'service_freshness':
          rule.service_name = ruleServiceName.trim();
          rule.release_version = ruleReleaseVersion.trim();
          rule.window_minutes = ruleWindow;
          break;
        case 'deploy_failure':
          rule.threshold = ruleThreshold;
          rule.window_minutes = ruleWindow;
          break;
        case 'deploy_acceptance':
          rule.threshold = ruleThreshold;
          rule.window_minutes = ruleWindow;
          break;
        case 'rollback':
          rule.threshold = ruleThreshold;
          rule.window_minutes = ruleWindow;
          break;
        case 'log_severity':
          rule.log_severity = ruleLogSeverity;
          rule.threshold = ruleThreshold;
          rule.window_minutes = ruleWindow;
          break;
        case 'usage_threshold':
          rule.usage_surface = ruleUsageSurface;
          rule.usage_operation = ruleUsageOperation;
          rule.usage_measure = ruleUsageMeasure;
          rule.comparison = ruleComparison;
          rule.threshold = ruleThreshold;
          rule.window_minutes = ruleWindow;
          break;
        case 'api_key_hygiene':
          rule.api_key_hygiene_mode = ruleAPIKeyHygieneMode;
          rule.threshold = ruleThreshold;
          rule.window_minutes = ruleWindow;
          break;
        case 'ai_triage':
          rule.ai_triage_mode = ruleAITriageMode;
          rule.threshold = ruleThreshold;
          rule.window_minutes = ruleWindow;
          break;
        case 'ingest_acceptance':
          rule.threshold = ruleThreshold;
          rule.window_minutes = ruleWindow;
          break;
        case 'new_issue':
        case 'regression':
          break;
      }
      const r = await api.createAlertRule(id, rule);
      rules = [r, ...rules];
      ruleName = '';
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to create rule';
    }
  }

  function applyRuleDefaults() {
    if (ruleType === 'spike') {
      ruleThreshold = 10;
      ruleWindow = 5;
    } else if (ruleType === 'anomaly') {
      ruleThreshold = 3;
      ruleWindow = 60;
    } else if (ruleType === 'silence') {
      ruleThreshold = 20;
      ruleWindow = 30;
    } else if (ruleType === 'metric_threshold') {
      ruleMetricName = '';
      ruleMetricLabels = '';
      ruleComparison = 'gte';
      ruleThresholdValue = 0.95;
      ruleWindow = 5;
    } else if (ruleType === 'trace_percentile') {
      ruleSpanName = '';
      ruleComparison = 'gte';
      ruleThresholdValue = 500;
      rulePercentile = 95;
      ruleWindow = 5;
    } else if (ruleType === 'service_level') {
      ruleSpanName = '';
      ruleThreshold = 1000;
      ruleThresholdValue = 0.999;
      ruleWindow = 5;
    } else if (ruleType === 'business_health') {
      ruleBusinessCheckName = '';
      ruleThresholdValue = 0.1;
      ruleWindow = 5;
    } else if (ruleType === 'connector_error_rate') {
      ruleConnectorProvider = '';
      ruleConnectorOperation = '';
      ruleConnectorChannel = '';
      ruleConnectorErrorCode = '';
      ruleConnectorConnectionID = '';
      ruleComparison = 'gte';
      ruleThresholdValue = 0.1;
      ruleWindow = 5;
    } else if (ruleType === 'connector_quota') {
      ruleConnectorProvider = '';
      ruleConnectorOperation = '';
      ruleConnectorChannel = '';
      ruleConnectorConnectionID = '';
      ruleConnectorQuotaMetric = 'quota_remaining';
      ruleComparison = 'lte';
      ruleThresholdValue = 100;
      ruleWindow = 5;
    } else if (ruleType === 'connector_freshness') {
      ruleConnectorProvider = '';
      ruleConnectorOperation = '';
      ruleConnectorChannel = '';
      ruleConnectorConnectionID = '';
      ruleWindow = 30;
    } else if (ruleType === 'connector_sync_health') {
      ruleConnectorProvider = '';
      ruleConnectorOperation = 'account_details';
      ruleConnectorConnectionID = '';
      ruleConnectorSyncMode = 'overdue';
      ruleThreshold = 1;
      ruleWindow = 15;
    } else if (ruleType === 'release_error_rate') {
      ruleReleaseVersion = '';
      ruleComparison = 'gte';
      ruleThresholdValue = 0.1;
      ruleWindow = 30;
    } else if (ruleType === 'release_freshness') {
      ruleReleaseVersion = '';
      ruleWindow = 30;
    } else if (ruleType === 'service_freshness') {
      ruleServiceName = '';
      ruleReleaseVersion = '';
      ruleWindow = 30;
    } else if (ruleType === 'deploy_failure') {
      ruleThreshold = 1;
      ruleWindow = 30;
    } else if (ruleType === 'deploy_acceptance') {
      ruleThreshold = 1;
      ruleWindow = 15;
    } else if (ruleType === 'rollback') {
      ruleThreshold = 1;
      ruleWindow = 30;
    } else if (ruleType === 'log_severity') {
      ruleLogSeverity = 'error';
      ruleThreshold = 100;
      ruleWindow = 5;
    } else if (ruleType === 'usage_threshold') {
      ruleUsageSurface = 'mcp';
      ruleUsageOperation = 'tools_call';
      ruleUsageMeasure = 'errors';
      ruleComparison = 'gte';
      ruleThreshold = 1;
      ruleWindow = 5;
    } else if (ruleType === 'api_key_hygiene') {
      ruleAPIKeyHygieneMode = 'unused';
      ruleThreshold = 1;
      ruleWindow = 30 * 24 * 60;
    } else if (ruleType === 'ai_triage') {
      ruleAITriageMode = 'failed';
      ruleThreshold = 1;
      ruleWindow = 15;
    } else if (ruleType === 'ingest_acceptance') {
      ruleThreshold = 1;
      ruleWindow = 15;
    }
  }

  function ruleSummary(r: AlertRule) {
    if (r.type === 'spike') return `spike: ${r.threshold} events / ${r.window_minutes}m`;
    if (r.type === 'anomaly') return `anomaly: ${r.threshold}x baseline over ${r.window_minutes}m`;
    if (r.type === 'silence') return `silence: quiet ${r.window_minutes}m (normally >${r.threshold}/24h)`;
    if (r.type === 'regression') return 'regression';
    if (r.type === 'metric_threshold') {
      const labels = r.metric_labels && Object.entries(r.metric_labels).map(([key, value]) => `${key}=${value}`).join(', ');
      return `metric: ${r.metric_name || 'unknown'}${labels ? ` [${labels}]` : ''} ${r.comparison || 'gte'} ${r.threshold_value ?? 0} / ${r.window_minutes}m`;
    }
    if (r.type === 'trace_percentile') {
      return `trace: p${r.percentile || 95} ${r.span_name || 'unknown'} ${r.comparison || 'gte'} ${r.threshold_value ?? 0}ms / ${r.window_minutes}m`;
    }
    if (r.type === 'service_level') {
      return `SLO: ${r.span_name || 'unknown'} availability < ${r.threshold_value ?? 0.999} at ${r.threshold || 1000}ms / ${r.window_minutes}m`;
    }
    if (r.type === 'business_health') {
      return `business: ${r.business_check_name || 'unknown'} error rate ${r.comparison || 'gte'} ${r.threshold_value ?? 0} / ${r.window_minutes}m`;
    }
    if (r.type === 'connector_error_rate') {
      const selector = [r.connector_provider, r.connector_operation, r.connector_channel].filter(Boolean).join('/');
      const code = r.connector_error_code ? ` (${r.connector_error_code})` : '';
      const connection = r.connector_connection_id ? ` [${r.connector_connection_id}]` : '';
      return `connector (${r.connector_source_kind || 'server_observed'}): ${selector || 'unknown'}${connection}${code} ${r.comparison || 'gte'} ${r.threshold_value ?? 0} / ${r.window_minutes}m`;
    }
    if (r.type === 'connector_quota') {
      const selector = [r.connector_provider, r.connector_operation, r.connector_channel].filter(Boolean).join('/');
      const connection = r.connector_connection_id ? ` [${r.connector_connection_id}]` : '';
      return `quota (${r.connector_source_kind || 'server_observed'}): ${selector || 'unknown'}${connection} ${r.connector_quota_metric || 'quota_remaining'} ${r.comparison || 'lte'} ${r.threshold_value ?? 0} / ${r.window_minutes}m`;
    }
    if (r.type === 'connector_freshness') {
      const selector = [r.connector_provider, r.connector_operation, r.connector_channel].filter(Boolean).join('/');
      const connection = r.connector_connection_id ? ` [${r.connector_connection_id}]` : '';
      return `freshness (${r.connector_source_kind || 'server_observed'}): ${selector || 'unknown'}${connection} every ${r.window_minutes}m`;
    }
    if (r.type === 'connector_sync_health') {
      const selector = [r.connector_provider, r.connector_operation].filter(Boolean).join('/');
      const connection = r.connector_connection_id ? ` [${r.connector_connection_id}]` : '';
      return `sync health: ${selector || 'unknown'}${connection} ${r.connector_sync_mode || 'overdue'} >= ${r.threshold} / ${r.window_minutes}m`;
    }
    if (r.type === 'release_error_rate') {
      return `release: ${r.release_version || 'latest'} ${r.comparison || 'gte'} ${r.threshold_value ?? 0} / ${r.window_minutes}m`;
    }
    if (r.type === 'release_freshness') {
      return `release freshness: ${r.release_version || 'unknown'} / ${r.window_minutes}m`;
    }
    if (r.type === 'service_freshness') {
      return `service freshness: ${r.service_name || 'unknown'} / ${r.release_version || 'unknown'} / ${r.window_minutes}m`;
    }
    if (r.type === 'deploy_failure') return `deploy failures: >= ${r.threshold} / ${r.window_minutes}m`;
    if (r.type === 'deploy_acceptance') return `deploy acceptance: >= ${r.threshold} unverified / ${r.window_minutes}m`;
    if (r.type === 'rollback') return `rollbacks: >= ${r.threshold} / ${r.window_minutes}m`;
    if (r.type === 'log_severity') return `logs: ${r.log_severity || 'error'} >= ${r.threshold} / ${r.window_minutes}m`;
    if (r.type === 'usage_threshold') return `usage: ${r.usage_surface || 'mcp'}/${r.usage_operation || 'other'} ${r.usage_measure || 'requests'} ${r.comparison || 'gte'} ${r.threshold} / ${r.window_minutes}m`;
    if (r.type === 'api_key_hygiene') return `API keys: ${r.api_key_hygiene_mode || 'unused'} >= ${r.threshold} / ${r.window_minutes}m`;
    if (r.type === 'ai_triage') return `AI triage: ${r.ai_triage_mode || 'failed'} >= ${r.threshold} / ${r.window_minutes}m`;
    if (r.type === 'ingest_acceptance') return `Ingest acceptance: >= ${r.threshold} incomplete / ${r.window_minutes}m`;
    return 'new issue';
  }

  async function removeRule(ruleId: string) {
    try {
      await api.deleteAlertRule(id, ruleId);
      rules = rules.filter((r) => r.id !== ruleId);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to remove rule';
    }
  }

  $effect(() => {
    for (const m of monitors) {
      monEdits[m.id] ??= { every: Math.round(m.interval_seconds / 60), grace: Math.round(m.grace_seconds / 60) };
    }
  });

  async function addMonitor(e: SubmitEvent) {
    e.preventDefault();
    monErr = null;
    if (!monSlug.trim()) return;
    try {
      const m = await api.createMonitor(id, {
        slug: monSlug.trim(),
        name: monName.trim(),
        interval_seconds: Math.max(0, Math.round(monEveryMin * 60)),
        grace_seconds: Math.max(0, Math.round(monGraceMin * 60))
      });
      monitors = [...monitors.filter((x) => x.id !== m.id), m].sort((a, b) => a.slug.localeCompare(b.slug));
      monSlug = '';
      monName = '';
    } catch (err) {
      monErr = err instanceof ApiError ? err.message : 'Failed to create monitor';
    }
  }

  async function saveMonitor(m: Monitor) {
    const e = monEdits[m.id];
    if (!e) return;
    try {
      const updated = await api.updateMonitor(m.id, {
        name: m.name,
        interval_seconds: Math.max(0, Math.round(e.every * 60)),
        grace_seconds: Math.max(0, Math.round(e.grace * 60))
      });
      monitors = monitors.map((x) => (x.id === updated.id ? updated : x));
    } catch (err) {
      monErr = err instanceof ApiError ? err.message : 'Failed to update monitor';
    }
  }

  async function removeMonitor(mid: string) {
    try {
      await api.deleteMonitor(mid);
      monitors = monitors.filter((m) => m.id !== mid);
    } catch (err) {
      monErr = err instanceof ApiError ? err.message : 'Failed to remove monitor';
    }
  }

  function monitorDot(state: string): string {
    if (state === 'ok') return 'bg-emerald-400';
    if (state === 'missing' || state === 'failed') return 'bg-rose-400';
    return 'bg-zinc-500';
  }

  async function copyText(t: string) {
    try {
      await navigator.clipboard.writeText(t);
    } catch {
      /* clipboard may be blocked; the URL is visible to select manually */
    }
  }

  async function deleteProject() {
    if (!project || confirmName !== project.name) return;
    deleting = true;
    error = null;
    try {
      const res = await api.deleteProject(id);
      // 202 body = partial erasure. Do not navigate away claiming success.
      if (res && res.status === 'partial') {
        partialErasure = res;
        deleting = false;
        return;
      }
      goto('/projects');
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to delete project';
      deleting = false;
    }
  }

  function onSmFile(e: Event) {
    const f = (e.target as HTMLInputElement).files?.[0] ?? null;
    smFile = f;
    if (f && !smName) smName = f.name.replace(/\.map$/, '');
  }

  async function uploadSourceMap(e: SubmitEvent) {
    e.preventDefault();
    if (!smFile || !smRelease.trim() || !smName.trim()) return;
    smBusy = true;
    error = null;
    try {
      const content = await smFile.text();
      const a = await api.uploadSourceMap(id, smRelease.trim(), smName.trim(), content);
      artifacts = [a, ...artifacts.filter((x) => x.id !== a.id)];
      smFile = null;
      smName = '';
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to upload source map';
    } finally {
      smBusy = false;
    }
  }

  async function removeArtifact(artifactId: string) {
    try {
      await api.deleteArtifact(id, artifactId);
      artifacts = artifacts.filter((a) => a.id !== artifactId);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to remove source map';
    }
  }

  function fmtSize(n: number) {
    return n > 1024 * 1024 ? `${(n / 1024 / 1024).toFixed(1)} MB` : `${Math.max(1, Math.round(n / 1024))} KB`;
  }

  const snippet = $derived(
    project ? `Sentry.init({\n  dsn: "${project.dsn}"\n});` : ''
  );
</script>

{#if project}
  <div class="mb-6 flex items-center gap-2 text-sm text-zinc-500">
    <a href="/projects" class="hover:text-zinc-300">Projects</a>
    <span>/</span>
    <span class="text-zinc-300">{project.name}</span>
  </div>

  <div class="mb-6 flex items-end justify-between">
    <h1 class="text-xl font-semibold tracking-tight">{project.name}</h1>
    <button
      onclick={() => (setupOpen = !setupOpen)}
      class="rounded-md border border-zinc-800 px-3 py-1.5 text-sm text-zinc-400 transition-colors hover:border-zinc-700 hover:text-zinc-200"
    >
      {setupOpen ? 'Hide setup' : 'Setup & alerts'}
    </button>
  </div>

  <ProjectTabs projectId={id} active="issues" />

  {#if setupOpen}
    <div class="mb-8 grid gap-6 rounded-lg border border-zinc-800/80 bg-zinc-900/40 p-5 md:grid-cols-2">
      <div>
        <h3 class="text-sm font-medium">Send events</h3>
        <p class="mt-1 text-xs text-zinc-500">
          Any Sentry SDK works. Set the DSN and errors flow in. OTLP endpoint:
          <code class="font-mono text-zinc-400">{project.otlp_endpoint}</code>
        </p>
        <pre class="mt-3 overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950 p-3 font-mono text-[11px] leading-relaxed text-zinc-300">{snippet}</pre>
      </div>
      <div>
        <h3 class="text-sm font-medium">Alert rules</h3>
        <p class="mt-1 text-xs text-zinc-500">
          New issue, regression, spike (N events in M minutes), anomaly (error rate jumps vs the project's own
          baseline), silence, metric thresholds, trace latency, service-level SLOs, business outcome error rates, connector error/quota/sync-lag/freshness/sync-state health, release error rates, release freshness, deploy failures, explicit rollbacks, log volume, MCP/API-key/DSN usage, API-key hygiene, AI triage, or incomplete ingest acceptance.
          Delivery goes to channels you add under <a href="/settings" class="text-amber-400 hover:text-amber-300">Alerts</a>.
        </p>
        {#if !hasChannel}
          <p class="mt-2 rounded-md border border-amber-900/50 bg-amber-950/20 px-3 py-2 text-xs text-amber-300/90">
            No notification channel is configured, so rules will not deliver anything yet. Add one under
            <a href="/settings" class="font-medium underline hover:text-amber-200">Alerts</a>.
          </p>
        {/if}
        <form onsubmit={addRule} class="mt-3 space-y-2">
          <div class="flex gap-2">
            <input
              bind:value={ruleName}
              placeholder="Rule name"
              class="flex-1 rounded-md border border-zinc-800 bg-zinc-950 px-2.5 py-1.5 text-sm outline-none focus:border-amber-400/60"
            />
            <select
              bind:value={ruleType}
              onchange={applyRuleDefaults}
              class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm outline-none focus:border-amber-400/60"
            >
              <option value="new_issue">New issue</option>
              <option value="regression">Regression</option>
              <option value="spike">Spike</option>
              <option value="anomaly">Anomaly</option>
              <option value="silence">Silence</option>
              <option value="metric_threshold">Metric threshold</option>
              <option value="trace_percentile">Trace latency percentile</option>
              <option value="service_level">Service-level SLO</option>
              <option value="business_health">Business outcome error rate</option>
              <option value="connector_error_rate">Connector error rate</option>
              <option value="connector_quota">Connector quota / rate limit</option>
              <option value="connector_freshness">Connector freshness</option>
              <option value="connector_sync_health">Connector sync state / lease health</option>
              <option value="service_freshness">Service heartbeat freshness</option>
              <option value="release_error_rate">Release error rate</option>
              <option value="release_freshness">Release freshness</option>
              <option value="deploy_failure">Deploy failure</option>
              <option value="deploy_acceptance">Deploy acceptance gap</option>
              <option value="rollback">Explicit rollback</option>
              <option value="log_severity">Log severity volume</option>
              <option value="usage_threshold">MCP/API-key/DSN usage</option>
              <option value="api_key_hygiene">API-key hygiene</option>
              <option value="ai_triage">AI triage health</option>
              <option value="ingest_acceptance">Ingest acceptance health</option>
            </select>
          </div>
          {#if ruleType === 'connector_error_rate' || ruleType === 'connector_quota' || ruleType === 'connector_freshness'}
            <label class="flex flex-col gap-1 text-xs text-zinc-500">
              Observation source
              <select
                bind:value={ruleConnectorSourceKind}
                class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              >
                <option value="server_observed">Flare provider adapter</option>
                <option value="client_reported">App or DSN sender assertion</option>
              </select>
            </label>
          {/if}
          {#if ruleType === 'spike'}
            <div class="flex items-center gap-2 text-xs text-zinc-500">
              <span>when</span>
              <input
                type="number"
                min="1"
                bind:value={ruleThreshold}
                class="w-16 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>events in</span>
              <input
                type="number"
                min="1"
                bind:value={ruleWindow}
                class="w-16 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>minutes</span>
            </div>
          {:else if ruleType === 'anomaly'}
            <div class="flex items-center gap-2 text-xs text-zinc-500">
              <span>when error rate exceeds</span>
              <input
                type="number"
                min="2"
                bind:value={ruleThreshold}
                class="w-16 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>x its baseline over</span>
              <input
                type="number"
                min="1"
                bind:value={ruleWindow}
                class="w-16 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>minutes</span>
            </div>
          {:else if ruleType === 'silence'}
            <div class="flex items-center gap-2 text-xs text-zinc-500">
              <span>when quiet for</span>
              <input
                type="number"
                min="1"
                bind:value={ruleWindow}
                class="w-16 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>min and normally &gt;</span>
              <input
                type="number"
                min="1"
                bind:value={ruleThreshold}
                class="w-16 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>events/24h</span>
            </div>
          {:else if ruleType === 'metric_threshold'}
            <div class="grid gap-2 sm:grid-cols-2">
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Metric name
                <input
                  bind:value={ruleMetricName}
                  required
                  maxlength="128"
                  placeholder="client.web.lcp_ms"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Label selectors (JSON, optional)
                <input
                  bind:value={ruleMetricLabels}
                  maxlength="1024"
                  placeholder="&#123;&quot;environment&quot;:&quot;production&quot;,&quot;route&quot;:&quot;/checkout&quot;&#125;"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Comparison
                <select
                  bind:value={ruleComparison}
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                >
                  <option value="gte">at least</option>
                  <option value="gt">greater than</option>
                  <option value="lte">at most</option>
                  <option value="lt">less than</option>
                </select>
              </label>
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                Threshold
                <input
                  type="number"
                  step="any"
                  bind:value={ruleThresholdValue}
                  class="w-28 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>over</span>
                <input
                  type="number"
                  min="1"
                  max="720"
                  bind:value={ruleWindow}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>min</span>
              </label>
            </div>
          {:else if ruleType === 'trace_percentile'}
            <div class="grid gap-2 sm:grid-cols-2">
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Span operation
                <input
                  bind:value={ruleSpanName}
                  required
                  maxlength="128"
                  placeholder="GET /checkout"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Percentile
                <select
                  bind:value={rulePercentile}
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                >
                  <option value={50}>p50</option>
                  <option value={75}>p75</option>
                  <option value={90}>p90</option>
                  <option value={95}>p95</option>
                  <option value={99}>p99</option>
                </select>
              </label>
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                Latency
                <input
                  type="number"
                  min="0"
                  max="86400000"
                  step="any"
                  bind:value={ruleThresholdValue}
                  class="w-28 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>ms over</span>
                <input
                  type="number"
                  min="1"
                  max="720"
                  bind:value={ruleWindow}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>min</span>
              </label>
            </div>
          {:else if ruleType === 'service_level'}
            <div class="grid gap-2 sm:grid-cols-2">
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Span operation
                <input
                  bind:value={ruleSpanName}
                  required
                  maxlength="128"
                  placeholder="GET /checkout"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                Availability target
                <input
                  type="number"
                  min="0.000001"
                  max="0.999999"
                  step="0.000001"
                  bind:value={ruleThresholdValue}
                  class="w-28 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                Good span latency at most
                <input
                  type="number"
                  min="1"
                  max="86400000"
                  bind:value={ruleThreshold}
                  class="w-28 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>ms over</span>
                <input
                  type="number"
                  min="1"
                  max="720"
                  bind:value={ruleWindow}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>min</span>
              </label>
            </div>
          {:else if ruleType === 'business_health'}
            <div class="grid gap-2 sm:grid-cols-2">
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Business check name
                <input
                  bind:value={ruleBusinessCheckName}
                  required
                  maxlength="64"
                  pattern="[a-z][a-z0-9._:-]*"
                  placeholder="checkout.completed"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                Error rate at least
                <input
                  type="number"
                  min="0"
                  max="1"
                  step="0.001"
                  bind:value={ruleThresholdValue}
                  class="w-24 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>over</span>
                <input
                  type="number"
                  min="1"
                  max="720"
                  bind:value={ruleWindow}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>min</span>
              </label>
            </div>
          {:else if ruleType === 'connector_error_rate'}
            <div class="grid gap-2 sm:grid-cols-3">
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Provider
                <input
                  bind:value={ruleConnectorProvider}
                  required
                  maxlength="32"
                  placeholder="hubspot"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Operation (optional)
                <input
                  bind:value={ruleConnectorOperation}
                  maxlength="64"
                  placeholder="contacts.list"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Channel (optional)
                <input
                  bind:value={ruleConnectorChannel}
                  maxlength="32"
                  placeholder="backend"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Error code (optional)
                <input
                  bind:value={ruleConnectorErrorCode}
                  maxlength="64"
                  pattern="[A-Za-z0-9][A-Za-z0-9._:-]*"
                  placeholder="auth_expired"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              {#if ruleConnectorSourceKind === 'server_observed'}
                <label class="flex flex-col gap-1 text-xs text-zinc-500">
                  Connection ID (optional)
                  <input
                    bind:value={ruleConnectorConnectionID}
                    maxlength="128"
                    pattern="[A-Za-z0-9][A-Za-z0-9._:-]*"
                    placeholder="registered installation id"
                    class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                  />
                </label>
              {/if}
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                Error rate
                <input
                  type="number"
                  min="0"
                  max="1"
                  step="0.01"
                  bind:value={ruleThresholdValue}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>over</span>
                <input
                  type="number"
                  min="1"
                  max="720"
                  bind:value={ruleWindow}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>min</span>
              </label>
            </div>
          {:else if ruleType === 'connector_quota'}
            <div class="grid gap-2 sm:grid-cols-3">
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Provider
                <input
                  bind:value={ruleConnectorProvider}
                  required
                  maxlength="32"
                  placeholder="hubspot"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Operation (optional)
                <input
                  bind:value={ruleConnectorOperation}
                  maxlength="64"
                  placeholder="contacts.list"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Channel (optional)
                <input
                  bind:value={ruleConnectorChannel}
                  maxlength="32"
                  placeholder="backend"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              {#if ruleConnectorSourceKind === 'server_observed'}
                <label class="flex flex-col gap-1 text-xs text-zinc-500">
                  Connection ID (optional)
                  <input
                    bind:value={ruleConnectorConnectionID}
                    maxlength="128"
                    pattern="[A-Za-z0-9][A-Za-z0-9._:-]*"
                    placeholder="registered installation id"
                    class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                  />
                </label>
              {/if}
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Provider signal
                <select
                  bind:value={ruleConnectorQuotaMetric}
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                >
                  <option value="quota_remaining">Quota remaining</option>
                  <option value="retry_after_ms">Retry-after delay (ms)</option>
                  <option value="sync_lag_ms">Sync lag (ms)</option>
                </select>
              </label>
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                Trigger when latest is
                <select
                  bind:value={ruleComparison}
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                >
                  <option value="lte">at most</option>
                  <option value="lt">less than</option>
                  <option value="gte">at least</option>
                  <option value="gt">greater than</option>
                </select>
                <input
                  type="number"
                  min="0"
                  max="1000000000000000"
                  step="1"
                  bind:value={ruleThresholdValue}
                  class="w-28 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                observed over
                <input
                  type="number"
                  min="1"
                  max="720"
                  bind:value={ruleWindow}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>min</span>
              </label>
            </div>
          {:else if ruleType === 'connector_freshness'}
            <div class="grid gap-2 sm:grid-cols-3">
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Provider
                <input
                  bind:value={ruleConnectorProvider}
                  required
                  maxlength="32"
                  placeholder="hubspot"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Operation (optional)
                <input
                  bind:value={ruleConnectorOperation}
                  maxlength="64"
                  placeholder="contacts.list"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Channel (optional)
                <input
                  bind:value={ruleConnectorChannel}
                  maxlength="32"
                  placeholder="backend"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              {#if ruleConnectorSourceKind === 'server_observed'}
                <label class="flex flex-col gap-1 text-xs text-zinc-500">
                  Connection ID (optional)
                  <input
                    bind:value={ruleConnectorConnectionID}
                    maxlength="128"
                    pattern="[A-Za-z0-9][A-Za-z0-9._:-]*"
                    placeholder="registered installation id"
                    class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                  />
                </label>
              {/if}
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                Require a completed attempt every
                <input
                  type="number"
                  min="1"
                  max="720"
                  bind:value={ruleWindow}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>min</span>
              </label>
            </div>
          {:else if ruleType === 'connector_sync_health'}
            <div class="grid gap-2 sm:grid-cols-4">
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Provider
                <input
                  bind:value={ruleConnectorProvider}
                  required
                  maxlength="32"
                  placeholder="hubspot"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Sync operation
                <input
                  bind:value={ruleConnectorOperation}
                  required
                  maxlength="64"
                  placeholder="account_details"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Registered connection ID
                <input
                  bind:value={ruleConnectorConnectionID}
                  required
                  maxlength="128"
                  pattern="[A-Za-z0-9][A-Za-z0-9._:-]*"
                  placeholder="connector installation id"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Condition
                <select
                  bind:value={ruleConnectorSyncMode}
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                >
                  <option value="overdue">Overdue schedule or lease</option>
                  <option value="failed">Recent failed state</option>
                </select>
              </label>
              <label class="flex items-center gap-2 text-xs text-zinc-500 sm:col-span-2">
                Alert at
                <input
                  type="number"
                  min="1"
                  max="1000000"
                  bind:value={ruleThreshold}
                  class="w-24 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                matching state(s), after
                <input
                  type="number"
                  min="1"
                  max="720"
                  bind:value={ruleWindow}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>min</span>
              </label>
            </div>
          {:else if ruleType === 'release_error_rate'}
            <div class="grid gap-2 sm:grid-cols-2">
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Release (optional; blank uses latest)
                <input
                  bind:value={ruleReleaseVersion}
                  maxlength="128"
                  placeholder="2026.10.02"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                Error rate
                <input
                  type="number"
                  min="0"
                  max="1"
                  step="0.01"
                  bind:value={ruleThresholdValue}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>over</span>
                <input
                  type="number"
                  min="1"
                  max="720"
                  bind:value={ruleWindow}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>min</span>
              </label>
            </div>
          {:else if ruleType === 'release_freshness'}
            <div class="grid gap-2 sm:grid-cols-2">
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Deployed release (required)
                <input
                  bind:value={ruleReleaseVersion}
                  required
                  maxlength="128"
                  placeholder="2026.10.02"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                Alert when heartbeat is older than
                <input
                  type="number"
                  min="1"
                  max="720"
                  bind:value={ruleWindow}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>minutes</span>
              </label>
            </div>
          {:else if ruleType === 'service_freshness'}
            <div class="grid gap-2 sm:grid-cols-3">
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Service name (exact heartbeat)
                <input
                  bind:value={ruleServiceName}
                  required
                  maxlength="64"
                  placeholder="api-web"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Release (exact)
                <input
                  bind:value={ruleReleaseVersion}
                  required
                  maxlength="128"
                  placeholder="2026.10.03"
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
              </label>
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                Alert when heartbeat is older than
                <input
                  type="number"
                  min="1"
                  max="720"
                  bind:value={ruleWindow}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>minutes</span>
              </label>
            </div>
          {:else if ruleType === 'deploy_failure'}
            <div class="flex flex-wrap items-center gap-2 text-xs text-zinc-500">
              <span>when failed or blocked deploys reach</span>
              <input
                type="number"
                min="1"
                max="1000000"
                bind:value={ruleThreshold}
                class="w-24 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>in</span>
              <input
                type="number"
                min="1"
                max="720"
                bind:value={ruleWindow}
                class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>minutes</span>
            </div>
          {:else if ruleType === 'deploy_acceptance'}
            <div class="flex flex-wrap items-center gap-2 text-xs text-zinc-500">
              <span>when passed deploys without synthetic verification reach</span>
              <input
                type="number"
                min="1"
                max="1000000"
                bind:value={ruleThreshold}
                class="w-24 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>after</span>
              <input
                type="number"
                min="1"
                max="720"
                bind:value={ruleWindow}
                class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>minutes</span>
            </div>
          {:else if ruleType === 'rollback'}
            <div class="flex flex-wrap items-center gap-2 text-xs text-zinc-500">
              <span>when explicit rollback outcomes reach</span>
              <input
                type="number"
                min="1"
                max="1000000"
                bind:value={ruleThreshold}
                class="w-24 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>in</span>
              <input
                type="number"
                min="1"
                max="720"
                bind:value={ruleWindow}
                class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>minutes</span>
            </div>
          {:else if ruleType === 'log_severity'}
            <div class="flex flex-wrap items-center gap-2 text-xs text-zinc-500">
              <span>when</span>
              <select
                bind:value={ruleLogSeverity}
                class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              >
                <option value="trace">trace</option>
                <option value="debug">debug</option>
                <option value="info">info</option>
                <option value="warn">warn</option>
                <option value="error">error</option>
                <option value="fatal">fatal</option>
              </select>
              <span>reaches</span>
              <input
                type="number"
                min="1"
                max="1000000"
                bind:value={ruleThreshold}
                class="w-24 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>records in</span>
              <input
                type="number"
                min="1"
                max="720"
                bind:value={ruleWindow}
                class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>minutes</span>
            </div>
          {:else if ruleType === 'usage_threshold'}
            <div class="grid gap-2 sm:grid-cols-3">
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Surface
                <select
                  bind:value={ruleUsageSurface}
                  onchange={() => (ruleUsageOperation = usageOperations[ruleUsageSurface][0])}
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                >
                  <option value="mcp">MCP (org-wide)</option>
                  <option value="api_key">API key (org-wide)</option>
                  <option value="dsn">DSN (project)</option>
                  <option value="business_key">Business key (project)</option>
                </select>
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Operation
                <select
                  bind:value={ruleUsageOperation}
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                >
                  {#each usageOperations[ruleUsageSurface] as operation}
                    <option value={operation}>{operation}</option>
                  {/each}
                </select>
              </label>
              <label class="flex flex-col gap-1 text-xs text-zinc-500">
                Counter
                <select
                  bind:value={ruleUsageMeasure}
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                >
                  <option value="requests">requests</option>
                  <option value="errors">errors</option>
                  <option value="denied">denied</option>
                </select>
              </label>
              <label class="flex items-center gap-2 text-xs text-zinc-500">
                When
                <select
                  bind:value={ruleComparison}
                  class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                >
                  <option value="gte">at least</option>
                  <option value="gt">greater than</option>
                  <option value="lte">at most</option>
                  <option value="lt">less than</option>
                </select>
                <input
                  type="number"
                  min="1"
                  max="1000000000"
                  bind:value={ruleThreshold}
                  class="w-24 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>in</span>
                <input
                  type="number"
                  min="1"
                  max="720"
                  bind:value={ruleWindow}
                  class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
                />
                <span>min</span>
              </label>
            </div>
          {:else if ruleType === 'api_key_hygiene'}
            <div class="flex flex-wrap items-center gap-2 text-xs text-zinc-500">
              <span>when</span>
              <select
                bind:value={ruleAPIKeyHygieneMode}
                class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              >
                <option value="unused">inactive/unused keys are older than</option>
                <option value="expiring">keys expire within</option>
              </select>
              <input
                type="number"
                min="1"
                max="129600"
                bind:value={ruleWindow}
                class="w-24 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>minutes and count reaches</span>
              <input
                type="number"
                min="1"
                max="1000000"
                bind:value={ruleThreshold}
                class="w-24 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>keys</span>
            </div>
          {:else if ruleType === 'ai_triage'}
            <div class="flex flex-wrap items-center gap-2 text-xs text-zinc-500">
              <span>when</span>
              <select
                bind:value={ruleAITriageMode}
                class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              >
                <option value="failed">failed triage calls reach</option>
                <option value="stuck">triage calls stay pending beyond</option>
              </select>
              <input
                type="number"
                min="1"
                max="1000000"
                bind:value={ruleThreshold}
                class="w-24 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              {#if ruleAITriageMode === 'failed'}
                <span>calls in</span>
              {:else}
                <span>calls older than</span>
              {/if}
              <input
                type="number"
                min="1"
                max="720"
                bind:value={ruleWindow}
                class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>minutes</span>
            </div>
          {:else if ruleType === 'ingest_acceptance'}
            <div class="flex flex-wrap items-center gap-2 text-xs text-zinc-500">
              <span>when incomplete ingest receipts reach</span>
              <input
                type="number"
                min="1"
                max="1000000"
                bind:value={ruleThreshold}
                class="w-24 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>older than</span>
              <input
                type="number"
                min="1"
                max="720"
                bind:value={ruleWindow}
                class="w-20 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
              />
              <span>minutes</span>
            </div>
          {/if}
          <button class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm text-zinc-200 transition-colors hover:bg-zinc-800 active:translate-y-px">Add rule</button>
        </form>
        <ul class="mt-3 space-y-1.5">
          {#each rules as r (r.id)}
            <li class="flex items-center gap-2 text-sm text-zinc-300">
              <span class="inline-block h-1.5 w-1.5 rounded-full bg-emerald-400"></span>
              {r.name}
              <span class="font-mono text-xs text-zinc-500">{ruleSummary(r)}</span>
              <button
                onclick={() => removeRule(r.id)}
                title="Remove rule"
                class="ml-auto text-xs text-zinc-600 transition-colors hover:text-rose-400"
              >
                Remove
              </button>
            </li>
          {:else}
            <li class="text-xs text-zinc-500">No rules yet.</li>
          {/each}
        </ul>
      </div>
    </div>

    <div class="mb-8 rounded-lg border border-zinc-800/80 bg-zinc-900/40 p-5">
      <h3 class="text-sm font-medium">Source maps</h3>
      <p class="mt-1 max-w-2xl text-xs text-zinc-500">
        Upload a <code class="font-mono text-zinc-400">.map</code> for each minified file in a release. Stack traces on matching errors are then resolved back to your original source, with context. Match by file name (e.g. <code class="font-mono text-zinc-400">app.min.js</code>) and the release your SDK reports.
      </p>
      <form onsubmit={uploadSourceMap} class="mt-3 flex flex-wrap items-end gap-2">
        <div class="flex flex-col gap-1.5">
          <label for="smrel" class="text-xs font-medium text-zinc-400">Release</label>
          <input
            id="smrel"
            bind:value={smRelease}
            placeholder="1.4.2"
            class="w-32 rounded-md border border-zinc-800 bg-zinc-950 px-2.5 py-1.5 text-sm outline-none focus:border-amber-400/60"
          />
        </div>
        <div class="flex flex-col gap-1.5">
          <label for="smname" class="text-xs font-medium text-zinc-400">Minified file</label>
          <input
            id="smname"
            bind:value={smName}
            placeholder="app.min.js"
            class="w-40 rounded-md border border-zinc-800 bg-zinc-950 px-2.5 py-1.5 text-sm outline-none focus:border-amber-400/60"
          />
        </div>
        <div class="flex flex-col gap-1.5">
          <label for="smfile" class="text-xs font-medium text-zinc-400">.map file</label>
          <input
            id="smfile"
            type="file"
            accept=".map,.json,application/json"
            onchange={onSmFile}
            class="w-56 text-xs text-zinc-400 file:mr-2 file:rounded-md file:border-0 file:bg-zinc-800 file:px-2.5 file:py-1.5 file:text-xs file:text-zinc-200 hover:file:bg-zinc-700"
          />
        </div>
        <button
          disabled={smBusy || !smFile}
          class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm text-zinc-200 transition-colors hover:bg-zinc-800 active:translate-y-px disabled:opacity-40"
        >
          {smBusy ? 'Uploading...' : 'Upload'}
        </button>
      </form>
      {#if artifacts.length}
        <ul class="mt-4 divide-y divide-zinc-800/60 text-sm">
          {#each artifacts as a (a.id)}
            <li class="flex items-center gap-3 py-2">
              <span class="rounded bg-zinc-800/80 px-1.5 py-0.5 font-mono text-[11px] text-zinc-400">{a.release}</span>
              <span class="truncate font-mono text-[13px] text-zinc-200">{a.name}</span>
              <span class="text-xs text-zinc-600">{fmtSize(a.size)}</span>
              <span class={a.parse_status === 'valid' ? 'text-xs text-emerald-400' : 'text-xs text-amber-400'} title="Source-map parser acceptance outcome">
                {a.parse_status === 'valid' ? 'parse ok' : a.parse_status.replace('_', ' ')}
              </span>
              <button
                onclick={() => removeArtifact(a.id)}
                class="ml-auto shrink-0 text-xs text-zinc-600 transition-colors hover:text-rose-400"
              >
                Remove
              </button>
            </li>
          {/each}
        </ul>
      {:else}
        <p class="mt-3 text-xs text-zinc-600">No source maps uploaded yet.</p>
      {/if}
    </div>

    <div class="mb-8 rounded-lg border border-zinc-800/80 bg-zinc-900/40 p-5">
      <h3 class="text-sm font-medium">Cron monitors</h3>
      <p class="mt-1 max-w-2xl text-xs text-zinc-500">
        Have a scheduled job ping its check-in URL each run. If it stops pinging past its expected interval
        plus grace, the watchdog fires a missing alert; a run that pings with
        <code class="font-mono text-zinc-400">?status=error</code> pages immediately. Delivery goes to your
        <a href="/settings" class="text-amber-400 hover:text-amber-300">channels</a>.
      </p>
      {#if monErr}
        <p class="mt-2 text-xs text-rose-400">{monErr}</p>
      {/if}
      <form onsubmit={addMonitor} class="mt-3 flex flex-wrap items-end gap-2">
        <div class="flex flex-col gap-1.5">
          <label for="monslug" class="text-xs font-medium text-zinc-400">Slug</label>
          <input
            id="monslug"
            bind:value={monSlug}
            placeholder="nightly-backup"
            class="w-40 rounded-md border border-zinc-800 bg-zinc-950 px-2.5 py-1.5 text-sm outline-none focus:border-amber-400/60"
          />
        </div>
        <div class="flex flex-col gap-1.5">
          <label for="monname" class="text-xs font-medium text-zinc-400">Name (optional)</label>
          <input
            id="monname"
            bind:value={monName}
            placeholder="Nightly DB backup"
            class="w-48 rounded-md border border-zinc-800 bg-zinc-950 px-2.5 py-1.5 text-sm outline-none focus:border-amber-400/60"
          />
        </div>
        <div class="flex items-center gap-2 text-xs text-zinc-500">
          <span>every</span>
          <input
            type="number"
            min="0"
            bind:value={monEveryMin}
            class="w-16 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
          />
          <span>min, grace</span>
          <input
            type="number"
            min="0"
            bind:value={monGraceMin}
            class="w-16 rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
          />
          <span>min</span>
        </div>
        <button class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm text-zinc-200 transition-colors hover:bg-zinc-800 active:translate-y-px">Add monitor</button>
      </form>
      {#if monitors.length}
        <ul class="mt-4 divide-y divide-zinc-800/60">
          {#each monitors as m (m.id)}
            <li class="py-3">
              <div class="flex flex-wrap items-center gap-3">
                <span class="inline-block h-2 w-2 shrink-0 rounded-full {monitorDot(m.state)}" title={m.state}></span>
                <span class="font-mono text-sm text-zinc-200">{m.slug}</span>
                {#if m.name}<span class="text-xs text-zinc-500">{m.name}</span>{/if}
                <span class="text-xs text-zinc-600">
                  {m.state === 'missing'
                    ? 'overdue'
                    : m.state === 'failed'
                      ? 'last run failed'
                      : m.state === 'ok'
                        ? 'ok'
                        : 'no pings yet'}
                  {#if m.last_ping_at}· last ping {relativeTime(m.last_ping_at)}{/if}
                </span>
                <button
                  onclick={() => removeMonitor(m.id)}
                  class="ml-auto text-xs text-zinc-600 transition-colors hover:text-rose-400"
                >
                  Remove
                </button>
              </div>
              {#if monEdits[m.id]}
                <div class="mt-2 flex flex-wrap items-center gap-2 pl-5 text-xs text-zinc-500">
                  <span>every</span>
                  <input
                    type="number"
                    min="0"
                    bind:value={monEdits[m.id].every}
                    class="w-14 rounded-md border border-zinc-800 bg-zinc-950 px-1.5 py-1 text-xs text-zinc-200 outline-none focus:border-amber-400/60"
                  />
                  <span>min, grace</span>
                  <input
                    type="number"
                    min="0"
                    bind:value={monEdits[m.id].grace}
                    class="w-14 rounded-md border border-zinc-800 bg-zinc-950 px-1.5 py-1 text-xs text-zinc-200 outline-none focus:border-amber-400/60"
                  />
                  <span>min</span>
                  <button
                    onclick={() => saveMonitor(m)}
                    class="rounded border border-zinc-800 px-2 py-0.5 text-xs text-zinc-400 hover:border-zinc-700 hover:text-zinc-200"
                  >
                    Save
                  </button>
                  {#if m.interval_seconds === 0}
                    <span class="text-amber-400/80">set an interval to enable missing-detection</span>
                  {/if}
                </div>
              {/if}
              <div class="mt-2 flex items-center gap-2 pl-5">
                <code class="truncate rounded bg-zinc-950 px-2 py-1 font-mono text-[11px] text-zinc-500">{m.checkin_url}</code>
                <button onclick={() => copyText(m.checkin_url)} class="shrink-0 text-xs text-zinc-500 hover:text-amber-400">Copy</button>
              </div>
            </li>
          {/each}
        </ul>
      {:else}
        <p class="mt-3 text-xs text-zinc-600">No monitors yet. Add one, then have your cron curl the check-in URL each run.</p>
      {/if}
    </div>

    <div class="mb-8 rounded-lg border border-rose-900/50 bg-rose-950/20 p-5">
      <h3 class="text-sm font-medium text-rose-300">Danger zone</h3>
      <p class="mt-1 max-w-2xl text-xs text-zinc-500">
        Deleting <span class="font-medium text-zinc-300">{project.name}</span> permanently removes the project, its DSN, and every issue, log, and trace it holds. This cannot be undone.
      </p>
      <div class="mt-3 flex flex-wrap items-center gap-2">
        <input
          bind:value={confirmName}
          placeholder="Type {project.name} to confirm"
          class="min-w-[14rem] flex-1 rounded-md border border-zinc-800 bg-zinc-950 px-2.5 py-1.5 text-sm outline-none focus:border-rose-500/60"
        />
        <button
          onclick={deleteProject}
          disabled={confirmName !== project.name || deleting}
          class="rounded-md border border-rose-700/70 bg-rose-600/10 px-3 py-1.5 text-sm text-rose-300 transition-colors hover:bg-rose-600/20 active:translate-y-px disabled:cursor-not-allowed disabled:opacity-40"
        >
          {deleting ? 'Deleting...' : 'Delete project'}
        </button>

      {#if partialErasure}
        <!-- Shown instead of navigating to /projects. Aged telemetry for this
             project is still in object storage; saying nothing would report a
             completed erasure that did not happen. -->
        <div class="mt-4 rounded-md border border-amber-400/40 bg-amber-400/10 p-4">
          <p class="text-sm font-medium text-amber-200">Project data was only partly erased</p>
          <p class="mt-2 text-xs text-amber-100/90">
            Everything in the live database is gone. Older telemetry already archived to object
            storage was <strong>not</strong> erased, because Flare cannot rewrite those files
            automatically. Ask whoever operates this instance to remove the archived copies; the
            request has been recorded on the server.
          </p>
          <p class="mt-2 text-xs text-amber-100/70">Reason: {partialErasure.cold_tier_note}</p>
          <button
            onclick={() => { partialErasure = null; goto('/projects'); }}
            class="mt-3 rounded-md border border-amber-400/40 px-3 py-1.5 text-xs text-amber-100 transition-colors hover:bg-amber-400/10"
          >
            I understand
          </button>
        </div>
      {/if}
      </div>
    </div>
  {/if}

  <div class="mb-3 flex items-center gap-1 border-b border-zinc-800/80">
    {#each filters as f (f)}
      <button
        onclick={() => setStatus(f)}
        class="-mb-px border-b-2 px-3 py-2 text-sm capitalize transition-colors {status === f
          ? 'border-amber-400 text-zinc-100'
          : 'border-transparent text-zinc-500 hover:text-zinc-300'}"
      >
        {f}
      </button>
    {/each}
    <div class="ml-auto flex items-center gap-3 pb-1.5">
      <input
        bind:value={search}
        oninput={onSearchInput}
        type="search"
        placeholder="Search title or culprit"
        aria-label="Search issues"
        class="w-48 rounded-md border border-zinc-800 bg-zinc-900/50 px-2.5 py-1 text-xs text-zinc-200 placeholder:text-zinc-600 focus:border-amber-400/50 focus:outline-none"
      />
      <span class="font-mono text-xs text-zinc-600">{total} total</span>
    </div>
  </div>

  {#if issues === null}
    <div class="divide-y divide-zinc-800/60">
      {#each Array(4) as _, i (i)}
        <div class="h-14 animate-pulse bg-zinc-900/30"></div>
      {/each}
    </div>
  {:else if issues.length === 0}
    <div class="rounded-lg border border-dashed border-zinc-800 px-6 py-14 text-center">
      <p class="text-sm font-medium text-zinc-300">Nothing here</p>
      <p class="mx-auto mt-1 max-w-sm text-sm text-zinc-500">
        {#if search.trim()}
          Nothing matches "{search.trim()}". Try a shorter search, or clear it.
        {:else}
          No {status === 'all' ? '' : status} issues. Send a test error with your DSN and it shows up here.
        {/if}
      </p>
    </div>
  {:else}
    <ul class="divide-y divide-zinc-800/60">
      {#each issues as issue (issue.id)}
        <li>
          <a href="/issues/{issue.id}" class="flex items-center gap-4 py-3 transition-colors hover:bg-zinc-900/40">
            <span class="inline-block h-2 w-2 shrink-0 rounded-full {issue.status === 'resolved' ? 'bg-emerald-400' : issue.status === 'ignored' ? 'bg-zinc-600' : 'bg-amber-400'}"></span>
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2">
                <div class="truncate text-sm font-medium text-zinc-100">{issue.title}</div>
                {#if issue.sensitive}
                  <span
                    class="shrink-0 rounded border border-rose-800/70 bg-rose-950/40 px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-rose-300"
                    title="Payload may contain: {issue.sensitive}"
                  >
                    Sensitive
                  </span>
                {/if}
              </div>
              <div class="mt-0.5 flex items-center gap-2 text-xs text-zinc-500">
                <span class="uppercase {levelColor(issue.level)}">{issue.level}</span>
                {#if issue.culprit}<span class="truncate font-mono">{issue.culprit}</span>{/if}
              </div>
            </div>
            <div class="shrink-0 text-right">
              <div class="font-mono text-sm text-zinc-300">{issue.event_count}</div>
              <div class="text-xs text-zinc-600">{relativeTime(issue.last_seen)}</div>
            </div>
          </a>
        </li>
      {/each}
    </ul>
    {#if total > perPage}
      <div class="mt-4 flex items-center justify-between text-xs text-zinc-500">
        <span class="font-mono">
          {page_ * perPage + 1}-{Math.min((page_ + 1) * perPage, total)} of {total}
        </span>
        <div class="flex items-center gap-2">
          <button
            onclick={() => goPage(-1)}
            disabled={page_ === 0}
            class="rounded-md border border-zinc-800 px-2.5 py-1 transition-colors hover:border-zinc-700 hover:text-zinc-300 active:translate-y-px disabled:cursor-not-allowed disabled:opacity-40"
          >
            Previous
          </button>
          <span class="font-mono text-zinc-600">{page_ + 1} / {pageCount}</span>
          <button
            onclick={() => goPage(1)}
            disabled={(page_ + 1) * perPage >= total}
            class="rounded-md border border-zinc-800 px-2.5 py-1 transition-colors hover:border-zinc-700 hover:text-zinc-300 active:translate-y-px disabled:cursor-not-allowed disabled:opacity-40"
          >
            Next
          </button>
        </div>
      </div>
    {/if}
  {/if}
{:else if error}
  <p class="rounded-md border border-rose-900/60 bg-rose-950/40 px-3 py-2 text-sm text-rose-300">{error}</p>
{:else}
  <div class="flex min-h-[30vh] items-center justify-center text-sm text-zinc-500">Loading...</div>
{/if}
