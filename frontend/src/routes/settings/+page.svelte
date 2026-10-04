<script lang="ts">
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { relativeTime } from '$lib/format';
  import type {
    AiConfig,
    ApiKey,
    ApiKeyRole,
    AuditEntry,
    PartialErasure,
    Channel,
    Project,
    GithubConfig,
    OidcConfig,
    ConnectorConnection,
    ConnectorCheckResult,
    ConnectorSyncRun
  } from '$lib/types';

  let channels = $state<Channel[] | null>(null);
  let projects = $state<Project[] | null>(null);
  let testResult = $state<Record<string, { ok: boolean; error?: string } | 'busy'>>({});
  let error = $state<string | null>(null);
  let type = $state('log');
  let url = $state('');
  let automationSigningSecret = $state('');
  let emailTo = $state('');
  let slackUrl = $state('');
  let busy = $state(false);

  let apiKeys = $state<ApiKey[] | null>(null);
  let keyName = $state('');
  // Read-only is the default so the easy path is the safe one. A member key can
  // delete a project and everything it ever recorded, so that has to be chosen
  // on purpose rather than inherited by clicking Create.
  let keyRole = $state<ApiKeyRole>('viewer');
  let newKey = $state<string | null>(null);
  let keyBusy = $state(false);

  let github = $state<GithubConfig | null>(null);
  let ghRepo = $state('');
  let ghToken = $state('');
  let ghBusy = $state(false);
  let connectorConnections = $state<ConnectorConnection[] | null>(null);
  let connectorProvider = $state('hubspot');
  let connectorInstallationId = $state('');
  let connectorInstallationName = $state('');
  let connectorScopes = $state('crm.objects.contacts.read');
  let connectorSecret = $state('');
  let connectorBusy = $state(false);
  let connectorChecks = $state<Record<string, ConnectorCheckResult | 'busy'>>({});
  let connectorRuns = $state<Record<string, ConnectorSyncRun | 'busy'>>({});
  const isAdmin = $derived(session.user?.role === 'admin' || session.user?.role === 'owner');
  const isOwner = $derived(session.user?.role === 'owner');

  let audit = $state<AuditEntry[]>([]);
  let exporting = $state(false);
  let confirmDelete = $state('');
  let deletingOrg = $state(false);
  let partialErasure = $state<PartialErasure | null>(null);

  // Change password. Lives in Settings because that is where a signed-in user
  // looks for account actions; before this the ONLY way to change a password was
  // the emailed reset link, which does not exist at all on an instance without
  // SMTP.
  let pwCurrent = $state('');
  let pwNew = $state('');
  let pwConfirm = $state('');
  let pwBusy = $state(false);
  let pwError = $state<string | null>(null);
  let pwDone = $state(false);

  // Mute without deleting. The enabled column has always existed and dispatch
  // has always filtered on it; nothing could set it, so silencing a channel that
  // started paging at 3am meant deleting it and re-entering a config the API
  // redacts and will not read back.
  async function toggleChannel(ch: Channel) {
    const next = !ch.enabled;
    try {
      await api.updateChannel(ch.id, { enabled: next });
      channels = (channels ?? []).map((c) => (c.id === ch.id ? { ...c, enabled: next } : c));
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Could not update channel';
    }
  }

  // Routing. An empty selection means every project, which is the default and
  // matches how channels behaved before routing existed.
  async function setChannelProjects(ch: Channel, ids: string[]) {
    try {
      await api.updateChannel(ch.id, { project_ids: ids });
      channels = (channels ?? []).map((c) => (c.id === ch.id ? { ...c, project_ids: ids } : c));
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Could not update routing';
    }
  }

  async function changePassword(e: SubmitEvent) {
    e.preventDefault();
    pwError = null;
    pwDone = false;
    if (pwNew.length < 8) {
      pwError = 'New password must be at least 8 characters.';
      return;
    }
    if (pwNew !== pwConfirm) {
      pwError = 'The two new passwords do not match.';
      return;
    }
    pwBusy = true;
    try {
      await api.changePassword(pwCurrent, pwNew);
      pwCurrent = pwNew = pwConfirm = '';
      pwDone = true;
    } catch (err) {
      pwError = err instanceof ApiError ? err.message : 'Could not change password';
    } finally {
      pwBusy = false;
    }
  }

  let ai = $state<AiConfig | null>(null);
  let aiBase = $state('');
  let aiKey = $state('');
  let aiModel = $state('');
  let aiFormat = $state('openai');
  let aiEnabled = $state(false);
  let aiAutoTriage = $state(false);
  let aiBudget = $state(50);
  let aiBusy = $state(false);

  let oidc = $state<OidcConfig | null>(null);
  let ssoIssuer = $state('');
  let ssoClientId = $state('');
  let ssoSecret = $state('');
  let ssoRole = $state('member');
  let ssoEnabled = $state(false);
  let ssoBusy = $state(false);
  let copiedRedirect = $state(false);

  // AI operator (MCP): everything a tenant needs to connect an MCP client. The
  // endpoint is this deployment's own origin; the key is the one just created
  // (shown once) or a placeholder until the tenant makes one above.
  let mcpUrl = $state('');
  let copiedMcp = $state<string | null>(null);
  const mcpKey = $derived(newKey ?? '<YOUR_API_KEY>');
  const mcpCommand = $derived(
    `claude mcp add --transport http flare ${mcpUrl} --header "Authorization: Bearer ${mcpKey}"`
  );
  const mcpJsonConfig = $derived(
    JSON.stringify(
      { mcpServers: { flare: { type: 'http', url: mcpUrl, headers: { Authorization: `Bearer ${mcpKey}` } } } },
      null,
      2
    )
  );

  $effect(() => {
    if (session.loaded && !session.user) goto('/login', { replaceState: true });
  });

  onMount(load);

  async function load() {
    mcpUrl = `${location.origin}/api/mcp`;
    try {
      channels = await api.channels();
      // Needed to render channel routing. Any authenticated role can list
      // projects, so this does not narrow who can see the Alerts page.
      projects = await api.projects();
      apiKeys = await api.apiKeys();
      if (isAdmin) {
        github = await api.githubConfig();
        ghRepo = github.repo;
        audit = await api.auditLog();
        oidc = await api.oidcConfig();
        ssoIssuer = oidc.issuer;
        ssoClientId = oidc.client_id;
        ssoRole = oidc.default_role || 'member';
        ssoEnabled = oidc.enabled;
        ai = await api.aiConfig();
        aiBase = ai.base_url;
        aiModel = ai.model;
        aiFormat = ai.format || 'openai';
        aiEnabled = ai.enabled;
        aiAutoTriage = ai.auto_triage;
        aiBudget = ai.triage_daily_budget;
        connectorConnections = await api.connectorConnections();
      }
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load settings';
    }
  }

  async function exportData() {
    exporting = true;
    error = null;
    try {
      const bundle = await api.exportData();
      const blob = new Blob([JSON.stringify(bundle, null, 2)], { type: 'application/json' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = 'flare-export.json';
      a.click();
      URL.revokeObjectURL(url);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Export failed';
    } finally {
      exporting = false;
    }
  }

  async function deleteOrg() {
    if (confirmDelete !== 'DELETE') return;
    deletingOrg = true;
    error = null;
    try {
      const res = await api.deleteOrg();
      // A 202 body means the erasure was PARTIAL. Navigating away here would
      // tell someone exercising a right to erasure that their data is gone when
      // aged telemetry is still in the object store, and after this navigation
      // there is no workspace left to come back to and ask.
      if (res && res.status === 'partial') {
        partialErasure = res;
        deletingOrg = false;
        return;
      }
      session.user = null;
      goto('/login', { replaceState: true });
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to delete workspace';
      deletingOrg = false;
    }
  }

  function fmtAction(a: string) {
    return a.replace(/\./g, ' ');
  }

  async function saveOidc(e: SubmitEvent) {
    e.preventDefault();
    ssoBusy = true;
    error = null;
    try {
      oidc = await api.setOidcConfig({
        issuer: ssoIssuer.trim(),
        client_id: ssoClientId.trim(),
        client_secret: ssoSecret.trim(), // blank keeps the stored secret (server-side)
        default_role: ssoRole,
        enabled: ssoEnabled
      });
      ssoSecret = '';
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to save SSO config';
    } finally {
      ssoBusy = false;
    }
  }

  async function disconnectOidc() {
    if (!confirm('Disconnect SSO? Members will sign in with email + password again.')) return;
    try {
      await api.deleteOidcConfig();
      oidc = await api.oidcConfig();
      ssoIssuer = '';
      ssoClientId = '';
      ssoEnabled = false;
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to disconnect SSO';
    }
  }

  async function copyRedirect() {
    if (!oidc) return;
    await navigator.clipboard.writeText(oidc.redirect_uri);
    copiedRedirect = true;
    setTimeout(() => (copiedRedirect = false), 1500);
  }

  async function copyMcp(text: string, id: string) {
    await navigator.clipboard.writeText(text);
    copiedMcp = id;
    setTimeout(() => {
      if (copiedMcp === id) copiedMcp = null;
    }, 1500);
  }

  async function saveAi(e: SubmitEvent) {
    e.preventDefault();
    aiBusy = true;
    error = null;
    try {
      ai = await api.setAiConfig({
        base_url: aiBase.trim(),
        api_key: aiKey.trim(),
        model: aiModel.trim(),
        format: aiFormat,
        enabled: aiEnabled,
        auto_triage: aiAutoTriage,
        triage_daily_budget: Number.isFinite(aiBudget) ? aiBudget : 50
      });
      aiKey = '';
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to save AI config';
    } finally {
      aiBusy = false;
    }
  }

  async function disconnectAi() {
    if (!confirm('Disconnect AI triage?')) return;
    try {
      await api.deleteAiConfig();
      ai = await api.aiConfig();
      aiBase = '';
      aiModel = '';
      aiAutoTriage = false;
      aiBudget = ai.triage_daily_budget;
      aiEnabled = false;
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to disconnect AI';
    }
  }

  async function saveGithub(e: SubmitEvent) {
    e.preventDefault();
    if (!ghRepo.trim() || !ghToken.trim()) return;
    ghBusy = true;
    error = null;
    try {
      github = await api.setGithubConfig(ghRepo.trim(), ghToken.trim());
      ghToken = '';
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to save GitHub config';
    } finally {
      ghBusy = false;
    }
  }

  async function clearGithub() {
    if (!confirm('Disconnect GitHub?')) return;
    try {
      await api.deleteGithubConfig();
      github = { configured: false, repo: '' };
      ghRepo = '';
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to disconnect GitHub';
    }
  }

  async function connectConnector(e: SubmitEvent) {
    e.preventDefault();
    const provider = connectorProvider.trim();
    const installationId = connectorInstallationId.trim();
    const secret = connectorSecret.trim();
    if (!provider || !installationId || !secret) {
      error = 'Provider, installation ID, and credential are required.';
      return;
    }
    connectorBusy = true;
    error = null;
    try {
      const connection = await api.createConnectorConnection({
        provider,
        installation_id: installationId,
        installation_name: connectorInstallationName.trim() || undefined,
        status: 'active',
        scopes: connectorScopes
          .split(',')
          .map((scope) => scope.trim())
          .filter(Boolean),
        secret
      });
      connectorConnections = [connection, ...(connectorConnections ?? [])];
      connectorInstallationId = '';
      connectorInstallationName = '';
      connectorSecret = '';
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to connect provider';
    } finally {
      connectorBusy = false;
    }
  }

  async function checkConnector(connection: ConnectorConnection) {
    connectorChecks[connection.id] = 'busy';
    try {
      connectorChecks[connection.id] = await api.checkConnectorConnection(connection.id);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Provider check failed';
      delete connectorChecks[connection.id];
    }
  }

  async function syncConnector(connection: ConnectorConnection) {
    connectorRuns[connection.id] = 'busy';
    try {
      connectorRuns[connection.id] = await api.runConnectorSync(connection.id);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Connector sync failed';
      delete connectorRuns[connection.id];
    }
  }

  async function disconnectConnector(connection: ConnectorConnection) {
    if (!confirm(`Disconnect ${connection.provider} installation ${connection.installation_name || connection.installation_id}?`)) return;
    try {
      await api.deleteConnectorConnection(connection.id);
      connectorConnections = (connectorConnections ?? []).filter((item) => item.id !== connection.id);
      delete connectorChecks[connection.id];
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to disconnect provider';
    }
  }

  async function createKey(e: SubmitEvent) {
    e.preventDefault();
    keyBusy = true;
    error = null;
    try {
      const k = await api.createApiKey(keyName.trim() || 'API key', keyRole);
      newKey = k.key;
      apiKeys = [
        {
          id: k.id,
          name: k.name,
          prefix: k.prefix,
          role: k.role,
          created_by: session.user?.email ?? '',
          created_at: new Date().toISOString(),
          last_used_at: null
        },
        ...(apiKeys ?? [])
      ];
      keyName = '';
      keyRole = 'viewer';
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to create key';
    } finally {
      keyBusy = false;
    }
  }

  async function add(e: SubmitEvent) {
    e.preventDefault();
    busy = true;
    error = null;
    try {
      const config =
        type === 'automation_webhook'
          ? { url, signing_secret: automationSigningSecret }
          : type === 'webhook'
          ? { url }
          : type === 'email'
            ? { to: emailTo }
            : type === 'slack'
              ? { webhook_url: slackUrl }
              : {};
      const ch = await api.createChannel(type, config);
      channels = [ch, ...(channels ?? [])];
      url = '';
      automationSigningSecret = '';
      emailTo = '';
      slackUrl = '';
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to create channel';
    } finally {
      busy = false;
    }
  }

  async function removeChannel(chId: string) {
    if (!confirm('Remove this alert channel? Rules pointing at it will stop delivering.')) return;
    try {
      await api.deleteChannel(chId);
      channels = (channels ?? []).filter((c) => c.id !== chId);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to remove channel';
    }
  }

  async function testChannel(chId: string) {
    testResult[chId] = 'busy';
    try {
      testResult[chId] = await api.testChannel(chId);
      // Refresh so the persistent delivery status (dot + last error) updates.
      channels = await api.channels();
    } catch (err) {
      testResult[chId] = { ok: false, error: err instanceof ApiError ? err.message : 'Test failed' };
    }
  }

  // channelHealth maps a channel's stored delivery columns to a status dot.
  function channelHealth(ch: Channel): { color: string; label: string } {
    if (!ch.enabled) return { color: 'bg-zinc-600', label: 'disabled' };
    if (ch.last_error) return { color: 'bg-rose-400', label: 'last delivery failed' };
    if (ch.last_ok_at) return { color: 'bg-emerald-400', label: 'delivering' };
    return { color: 'bg-zinc-500', label: 'never tested' };
  }

  async function revokeKey(keyId: string) {
    if (!confirm('Revoke this API key? Anything using it stops working immediately.')) return;
    try {
      await api.deleteApiKey(keyId);
      apiKeys = (apiKeys ?? []).filter((k) => k.id !== keyId);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to revoke key';
    }
  }
</script>

<h1 class="text-xl font-semibold tracking-tight">Alert channels</h1>
<p class="mt-1 mb-8 text-sm text-zinc-500">
  Where new-issue alerts are delivered. Attach rules to a project from its Setup panel.
</p>

<form onsubmit={add} class="mb-8 flex flex-wrap items-end gap-3 border-b border-zinc-800/80 pb-8">
  <div class="flex flex-col gap-1.5">
    <label for="type" class="text-xs font-medium text-zinc-400">Channel type</label>
    <select
      id="type"
      bind:value={type}
      class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
    >
      <option value="log">Server log</option>
      <option value="email">Email</option>
      <option value="slack">Slack</option>
      <option value="webhook">Webhook</option>
      <option value="automation_webhook">Automation webhook</option>
    </select>
  </div>
  {#if type === 'slack'}
    <div class="flex flex-1 flex-col gap-1.5">
      <label for="slackurl" class="text-xs font-medium text-zinc-400">Slack incoming webhook URL</label>
      <input
        id="slackurl"
        bind:value={slackUrl}
        placeholder="https://hooks.slack.com/services/..."
        class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
      />
      <span class="text-xs text-zinc-600">Create one under Slack apps &rarr; Incoming Webhooks.</span>
    </div>
  {:else if type === 'automation_webhook'}
    <div class="flex flex-1 flex-col gap-1.5">
      <label for="automationurl" class="text-xs font-medium text-zinc-400">Worker webhook URL</label>
      <input
        id="automationurl"
        bind:value={url}
        placeholder="https://worker.example.com/flare"
        class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
      />
      <label for="automationsigningsecret" class="mt-1 text-xs font-medium text-zinc-400">Signing secret</label>
      <input
        id="automationsigningsecret"
        type="password"
        autocomplete="new-password"
        bind:value={automationSigningSecret}
        placeholder="16–256 printable characters"
        class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
      />
      <span class="text-xs text-zinc-600">Receives signed, payload-free notices. The worker fetches evidence and keeps approval and execution in its own guarded workflow.</span>
    </div>
  {:else if type === 'webhook'}
    <div class="flex flex-1 flex-col gap-1.5">
      <label for="url" class="text-xs font-medium text-zinc-400">Webhook URL</label>
      <input
        id="url"
        bind:value={url}
        placeholder="https://hooks.example.com/flare"
        class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
      />
      <span class="text-xs text-zinc-600">Private and loopback addresses are rejected.</span>
    </div>
  {:else if type === 'email'}
    <div class="flex flex-1 flex-col gap-1.5">
      <label for="emailto" class="text-xs font-medium text-zinc-400">Send to</label>
      <input
        id="emailto"
        type="email"
        bind:value={emailTo}
        placeholder="alerts@yourteam.com"
        class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
      />
      <span class="text-xs text-zinc-600">New-issue alerts are emailed here.</span>
    </div>
  {/if}
  <button
    type="submit"
    disabled={busy}
    class="rounded-md bg-amber-400 px-3.5 py-2 text-sm font-medium text-zinc-950 transition-colors hover:bg-amber-300 active:translate-y-px disabled:opacity-60"
  >
    {busy ? 'Adding...' : 'Add channel'}
  </button>
</form>

{#if error}
  <p class="mb-6 rounded-md border border-rose-900/60 bg-rose-950/40 px-3 py-2 text-sm text-rose-300">{error}</p>
{/if}

{#if channels === null}
  <div class="space-y-2">
    {#each Array(2) as _, i (i)}
      <div class="h-12 animate-pulse rounded-md border border-zinc-800/60 bg-zinc-900/40"></div>
    {/each}
  </div>
{:else if channels.length === 0}
  <div class="rounded-lg border border-dashed border-zinc-800 px-6 py-12 text-center">
    <p class="text-sm font-medium text-zinc-300">No channels yet</p>
    <p class="mt-1 text-sm text-zinc-500">Add one above. "Server log" is the quickest way to verify alerts fire.</p>
  </div>
{:else}
  <ul class="divide-y divide-zinc-800/60">
    {#each channels as ch (ch.id)}
      {@const health = channelHealth(ch)}
      {@const res = testResult[ch.id]}
      <li class="flex flex-wrap items-center gap-3 py-3">
        <span class="inline-block h-1.5 w-1.5 rounded-full {health.color}" title={health.label}></span>
        <span class="text-sm font-medium capitalize text-zinc-200">{ch.type === 'automation_webhook' ? 'Automation webhook' : ch.type}</span>
        {#if ch.config?.url}<span class="truncate font-mono text-xs text-zinc-500">{String(ch.config.url)}</span>{/if}
        {#if ch.config?.to}<span class="truncate font-mono text-xs text-zinc-500">{String(ch.config.to)}</span>{/if}
        {#if ch.config?.webhook_url}<span class="truncate font-mono text-xs text-zinc-500">{String(ch.config.webhook_url)}</span>{/if}
        <div class="ml-auto flex shrink-0 items-center gap-3">
          {#if res === 'busy'}
            <span class="text-xs text-zinc-500">Sending...</span>
          {:else if res?.ok}
            <span class="text-xs text-emerald-400">Delivered</span>
          {:else if res}
            <span class="max-w-56 truncate text-xs text-rose-400" title={res.error}>Failed: {res.error}</span>
          {/if}
          <button
            onclick={() => testChannel(ch.id)}
            disabled={res === 'busy'}
            class="text-xs text-zinc-500 transition-colors hover:text-amber-400 disabled:opacity-50"
          >
            Send test
          </button>
          <button
            onclick={() => toggleChannel(ch)}
            class="text-xs transition-colors {ch.enabled
              ? 'text-zinc-500 hover:text-amber-400'
              : 'text-amber-400 hover:text-amber-300'}"
            title={ch.enabled
              ? 'Stop sending to this channel without deleting it'
              : 'Resume sending to this channel'}
          >
            {ch.enabled ? 'Mute' : 'Unmute'}
          </button>
          <button
            onclick={() => removeChannel(ch.id)}
            class="text-xs text-zinc-600 transition-colors hover:text-rose-400"
          >
            Remove
          </button>
        </div>
        {#if !ch.enabled}
          <p class="w-full pl-5 text-xs text-amber-300/80">
            Muted. Alerts are not delivered to this channel.
          </p>
        {/if}
        <!-- Routing. No selection = every project, which is the default and what
             every channel did before routing existed. -->
        <div class="flex w-full flex-wrap items-center gap-1.5 pl-5">
          <span class="text-[11px] text-zinc-600">Sends for:</span>
          <button
            onclick={() => setChannelProjects(ch, [])}
            class="rounded border px-1.5 py-0.5 text-[11px] transition-colors {ch.project_ids
              ?.length
              ? 'border-zinc-800 text-zinc-500 hover:border-zinc-700'
              : 'border-amber-400/40 bg-amber-400/10 text-amber-200'}"
          >
            All projects
          </button>
          {#each projects ?? [] as p (p.id)}
            {@const on = (ch.project_ids ?? []).includes(p.id)}
            <button
              onclick={() => {
                const next = on
                  ? (ch.project_ids ?? []).filter((x) => x !== p.id)
                  : [...(ch.project_ids ?? []), p.id];
                // Unchecking the last scoped project does not leave the channel with
                // zero projects, it falls back to "every project" (see the note
                // above). That is a silent scope expansion, so make it an explicit
                // choice instead of a side effect of a checkbox.
                if (on && next.length === 0) {
                  if (
                    !confirm(
                      `Removing the last project will make this ${ch.type} channel send alerts for ALL projects, not none. Continue?`
                    )
                  )
                    return;
                }
                setChannelProjects(ch, next);
              }}
              class="rounded border px-1.5 py-0.5 text-[11px] transition-colors {on
                ? 'border-amber-400/40 bg-amber-400/10 text-amber-200'
                : 'border-zinc-800 text-zinc-500 hover:border-zinc-700'}"
            >
              {p.name}
            </button>
          {/each}
        </div>
        {#if ch.last_error}
          <p class="w-full pl-5 font-mono text-xs text-rose-400/80">
            Last delivery failed: {ch.last_error}
          </p>
        {/if}
      </li>
    {/each}
  </ul>
{/if}

<h2 class="mt-14 text-xl font-semibold tracking-tight">API keys</h2>
<p class="mt-1 mb-8 text-sm text-zinc-500">
  Org-scoped keys for the management API (Bearer auth): auto-provisioning a project per service,
  uploading source maps, reading issues. <strong class="text-zinc-400">Not for sending telemetry.</strong>
  Client telemetry (OTLP included) authenticates with the per-project DSN key from the project page, sent as
  <code class="font-mono">x-flare-key</code>. Business checks use a separate confidential backend key created on the project's Business page; keep that key out of browser and mobile builds.
</p>

<form onsubmit={createKey} class="mb-6 flex flex-wrap items-end gap-3 border-b border-zinc-800/80 pb-8">
  <div class="flex flex-1 flex-col gap-1.5">
    <label for="keyname" class="text-xs font-medium text-zinc-400">Key name</label>
    <input
      id="keyname"
      bind:value={keyName}
      placeholder="gopile"
      class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
    />
  </div>
  <div class="flex flex-col gap-1.5">
    <label for="keyrole" class="text-xs font-medium text-zinc-400">Access</label>
    <select
      id="keyrole"
      bind:value={keyRole}
      class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
    >
      <option value="viewer">Read-only</option>
      <option value="member">Read and write</option>
    </select>
  </div>
  <button
    type="submit"
    disabled={keyBusy}
    class="rounded-md bg-amber-400 px-3.5 py-2 text-sm font-medium text-zinc-950 transition-colors hover:bg-amber-300 active:translate-y-px disabled:opacity-60"
  >
    {keyBusy ? 'Creating...' : 'Create key'}
  </button>
</form>

{#if newKey}
  <div class="mb-6 rounded-md border border-amber-400/40 bg-amber-400/10 px-4 py-3">
    <p class="text-sm font-medium text-amber-200">Copy this key now. It will not be shown again.</p>
    <code class="mt-2 block break-all font-mono text-xs text-amber-100">{newKey}</code>
  </div>
{/if}

{#if apiKeys && apiKeys.length}
  <ul class="divide-y divide-zinc-800/60">
    {#each apiKeys as k (k.id)}
      <li class="flex items-center gap-3 py-3">
        <span class="text-sm font-medium text-zinc-200">{k.name}</span>
        <span class="font-mono text-xs text-zinc-500">{k.prefix}...</span>
        <span
          class="rounded border px-1.5 py-0.5 text-[11px] font-medium {k.role === 'member'
            ? 'border-amber-400/30 bg-amber-400/10 text-amber-200'
            : 'border-zinc-700 bg-zinc-800/60 text-zinc-400'}"
        >
          {k.role === 'member' ? 'read + write' : 'read-only'}
        </span>
        {#if k.created_by}
          <span class="text-xs text-zinc-600">by {k.created_by}</span>
        {:else}
          <span
            class="rounded border border-amber-400/30 bg-amber-400/10 px-1.5 py-0.5 text-[11px] text-amber-200"
            title="Created before Flare recorded key owners. A password reset will NOT revoke this key. Rotate it if you are unsure who holds it."
          >
            unknown owner
          </span>
        {/if}
        <span class="ml-auto text-xs text-zinc-600">
          {k.last_used_at ? 'used' : 'never used'}
        </span>
        <button
          onclick={() => revokeKey(k.id)}
          class="shrink-0 text-xs text-zinc-600 transition-colors hover:text-rose-400"
        >
          Revoke
        </button>
      </li>
    {/each}
  </ul>
{:else if apiKeys}
  <p class="text-sm text-zinc-500">No keys yet.</p>
{/if}

<h2 class="mt-14 text-xl font-semibold tracking-tight">AI operator (MCP)</h2>
<p class="mt-1 mb-6 max-w-2xl text-sm text-zinc-500">
  Flare speaks the
  <a href="https://modelcontextprotocol.io" target="_blank" rel="noopener" class="text-amber-400 transition-colors hover:text-amber-300">Model Context Protocol</a>,
  so an AI client (Claude Code, Cursor, and others) can investigate and act on your errors directly:
  pull an overview, read a stack trace, search logs, run AI triage, and resolve issues. Reads work for any
  member; the two write tools (resolve and triage) need a member key. It authenticates with an org API key,
  created above, and stays scoped to this workspace.
</p>

<div class="mb-4">
  <div class="mb-1.5 text-xs font-medium text-zinc-400">Endpoint</div>
  <div class="flex items-center gap-2 rounded-md border border-zinc-800 bg-zinc-900/60 px-3 py-2">
    <code class="flex-1 truncate font-mono text-[12px] text-zinc-300">{mcpUrl}</code>
    <button
      onclick={() => copyMcp(mcpUrl, 'url')}
      class="shrink-0 rounded-md border border-zinc-800 px-2 py-1 text-xs text-zinc-400 transition-colors hover:border-zinc-700 hover:text-zinc-200"
    >
      {copiedMcp === 'url' ? 'Copied' : 'Copy'}
    </button>
  </div>
</div>

<div class="mb-4">
  <div class="mb-1.5 flex items-center gap-2">
    <span class="text-xs font-medium text-zinc-400">Connect Claude Code</span>
    <button
      onclick={() => copyMcp(mcpCommand, 'cmd')}
      class="rounded-md border border-zinc-800 px-2 py-1 text-xs text-zinc-400 transition-colors hover:border-zinc-700 hover:text-zinc-200"
    >
      {copiedMcp === 'cmd' ? 'Copied' : 'Copy'}
    </button>
  </div>
  <pre class="overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950 px-3 py-2.5 font-mono text-[12px] leading-relaxed text-zinc-300">{mcpCommand}</pre>
  {#if newKey}
    <p class="mt-1.5 text-sm text-amber-200/80">Your new key is filled in above. Run this in your terminal, then restart the client. The key is shown only once.</p>
  {:else}
    <p class="mt-1.5 text-xs text-zinc-600">Create a key above and it drops into this command automatically. Run it in your terminal, then restart the client.</p>
  {/if}
</div>

<details class="mb-2">
  <summary class="cursor-pointer text-xs text-zinc-500 transition-colors hover:text-zinc-300">Other MCP clients (raw config)</summary>
  <pre class="mt-2 overflow-x-auto rounded-md border border-zinc-800 bg-zinc-950 px-3 py-2.5 font-mono text-[12px] leading-relaxed text-zinc-400">{mcpJsonConfig}</pre>
</details>

{#if isAdmin}
  <h2 class="mt-14 text-xl font-semibold tracking-tight">GitHub</h2>
  <p class="mt-1 mb-6 text-sm text-zinc-500">
    Connect a repo to open a GitHub issue straight from any Flare issue. The token is stored for this
    workspace and never shown again. A fine-grained token with <code class="font-mono">Issues: write</code> is enough.
  </p>

  {#if github?.configured}
    <div class="mb-4 flex items-center gap-3 rounded-md border border-zinc-800/80 bg-zinc-900/40 px-4 py-3">
      <span class="inline-block h-1.5 w-1.5 rounded-full bg-emerald-400"></span>
      <span class="text-sm text-zinc-200">Connected to <span class="font-mono">{github.repo}</span></span>
      <button onclick={clearGithub} class="ml-auto text-xs text-zinc-600 transition-colors hover:text-rose-400">Disconnect</button>
    </div>
  {/if}

  <form onsubmit={saveGithub} class="flex flex-wrap items-end gap-3">
    <div class="flex flex-col gap-1.5">
      <label for="ghrepo" class="text-xs font-medium text-zinc-400">Repository</label>
      <input
        id="ghrepo"
        bind:value={ghRepo}
        placeholder="owner/repo"
        class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
      />
    </div>
    <div class="flex flex-1 flex-col gap-1.5">
      <label for="ghtoken" class="text-xs font-medium text-zinc-400">{github?.configured ? 'Replace token' : 'Token'}</label>
      <input
        id="ghtoken"
        type="password"
        bind:value={ghToken}
        placeholder="ghp_..."
        class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
      />
    </div>
    <button
      type="submit"
      disabled={ghBusy}
      class="rounded-md bg-amber-400 px-3.5 py-2 text-sm font-medium text-zinc-950 transition-colors hover:bg-amber-300 active:translate-y-px disabled:opacity-60"
    >
      {ghBusy ? 'Saving...' : github?.configured ? 'Update' : 'Connect'}
    </button>
  </form>

  <h2 class="mt-14 text-xl font-semibold tracking-tight">SaaS connectors</h2>
  <p class="mt-1 mb-6 max-w-2xl text-sm text-zinc-500">
    Register a provider installation for bounded health checks. Credentials are write-only and encrypted at rest;
    Flare stores only connection metadata and payload-free health receipts. Supported checks are HubSpot account health,
    GitHub <code>user</code>, Slack <code>auth.test</code>, and OpenAI <code>models</code>; synchronization remains HubSpot-only.
  </p>

  <form onsubmit={connectConnector} class="mb-6 flex flex-wrap items-end gap-3 border-b border-zinc-800/80 pb-8">
    <div class="flex flex-col gap-1.5">
      <label for="connector-provider" class="text-xs font-medium text-zinc-400">Provider</label>
      <input
        id="connector-provider"
        bind:value={connectorProvider}
        placeholder="hubspot, github, slack, or openai"
        class="w-32 rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
      />
    </div>
    <div class="flex flex-1 flex-col gap-1.5">
      <label for="connector-installation" class="text-xs font-medium text-zinc-400">Installation ID</label>
      <input
        id="connector-installation"
        bind:value={connectorInstallationId}
        placeholder="production"
        class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
      />
    </div>
    <div class="flex flex-1 flex-col gap-1.5">
      <label for="connector-name" class="text-xs font-medium text-zinc-400">Display name</label>
      <input
        id="connector-name"
        bind:value={connectorInstallationName}
        placeholder="Production provider"
        class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
      />
    </div>
    <div class="flex flex-1 flex-col gap-1.5">
      <label for="connector-scopes" class="text-xs font-medium text-zinc-400">Scopes</label>
      <input
        id="connector-scopes"
        bind:value={connectorScopes}
        placeholder="crm.objects.contacts.read"
        class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
      />
    </div>
    <div class="flex flex-1 flex-col gap-1.5">
      <label for="connector-secret" class="text-xs font-medium text-zinc-400">Credential</label>
      <input
        id="connector-secret"
        type="password"
        autocomplete="new-password"
        bind:value={connectorSecret}
        placeholder="write-only"
        class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
      />
    </div>
    <button
      type="submit"
      disabled={connectorBusy}
      class="rounded-md bg-amber-400 px-3.5 py-2 text-sm font-medium text-zinc-950 transition-colors hover:bg-amber-300 active:translate-y-px disabled:opacity-60"
    >
      {connectorBusy ? 'Connecting...' : 'Connect'}
    </button>
  </form>

  {#if connectorConnections?.length}
    <ul class="divide-y divide-zinc-800/60">
      {#each connectorConnections as connection (connection.id)}
        {@const check = connectorChecks[connection.id]}
        {@const run = connectorRuns[connection.id]}
        <li class="flex flex-wrap items-center gap-3 py-3">
          <span class="inline-block h-1.5 w-1.5 rounded-full {connection.status === 'active' ? 'bg-emerald-400' : 'bg-zinc-500'}"></span>
          <span class="text-sm font-medium text-zinc-200">{connection.installation_name || connection.installation_id}</span>
          <span class="rounded border border-zinc-800 px-1.5 py-0.5 text-[11px] text-zinc-400">{connection.provider}</span>
          <span class="text-xs text-zinc-600">{connection.credential_configured ? 'credential configured' : 'missing credential'}</span>
          <div class="ml-auto flex items-center gap-3">
            {#if check === 'busy'}
              <span class="text-xs text-zinc-500">Checking...</span>
            {:else if check}
              <div class="flex items-center gap-2">
                <span class="text-xs {check.outcome === 'ok' ? 'text-emerald-400' : 'text-rose-400'}">
                  {check.outcome === 'ok' ? 'Healthy' : `Failed${check.error_code ? `: ${check.error_code}` : ''}`}
                </span>
                {#if check.receipt_status !== 'accepted'}
                  <span class="text-[11px] text-amber-400" title="The provider result is shown, but Flare could not durably record its health receipt.">Receipt {check.receipt_status}</span>
                {/if}
              </div>
            {/if}
            <button onclick={() => checkConnector(connection)} disabled={check === 'busy'} class="text-xs text-zinc-500 transition-colors hover:text-amber-400 disabled:opacity-50">Check</button>
            {#if run === 'busy'}
              <span class="text-xs text-zinc-500">Syncing...</span>
            {:else if run?.skipped}
              <span class="text-xs text-zinc-500">{run.skip_reason === 'lease_busy' ? 'Busy' : 'Skipped'}</span>
            {:else if run}
              <span class="text-xs {run.outcome === 'ok' ? 'text-emerald-400' : 'text-rose-400'}">{run.outcome === 'ok' ? 'Synced' : `Sync failed${run.error_code ? `: ${run.error_code}` : ''}`}</span>
            {/if}
            {#if connection.provider === 'hubspot'}
              <button onclick={() => syncConnector(connection)} disabled={run === 'busy' || connection.status !== 'active'} class="text-xs text-zinc-500 transition-colors hover:text-amber-400 disabled:opacity-50">Sync</button>
            {/if}
            <button onclick={() => disconnectConnector(connection)} class="text-xs text-zinc-600 transition-colors hover:text-rose-400">Disconnect</button>
          </div>
        </li>
      {/each}
    </ul>
  {:else if connectorConnections}
    <p class="text-sm text-zinc-500">No SaaS connectors yet.</p>
  {/if}

  <h2 class="mt-14 text-xl font-semibold tracking-tight">Single sign-on (OIDC)</h2>
  <p class="mt-1 mb-4 text-sm text-zinc-500">
    Let members sign in through your identity provider (Zitadel, Okta, Auth0, Keycloak, Entra). Register an
    OIDC app there with the redirect URI below, then paste its client id + secret.
  </p>

  {#if oidc}
    <div class="mb-4 flex flex-wrap items-center gap-2 rounded-md border border-zinc-800/80 bg-zinc-900/40 px-4 py-3 text-sm">
      <span class="text-zinc-400">Redirect URI</span>
      <code class="truncate font-mono text-[12px] text-zinc-300">{oidc.redirect_uri}</code>
      <button onclick={copyRedirect} class="rounded-md border border-zinc-800 px-2 py-1 text-xs text-zinc-400 hover:border-zinc-700 hover:text-zinc-200">
        {copiedRedirect ? 'Copied' : 'Copy'}
      </button>
      {#if oidc.enabled}
        <span class="ml-auto inline-flex items-center gap-1.5 text-xs text-emerald-400"><span class="inline-block h-1.5 w-1.5 rounded-full bg-emerald-400"></span>enabled</span>
      {/if}
    </div>
  {/if}

  {#if !isOwner}
    <!-- Reads stay at admin+, but writing the SSO config decides who can
         authenticate as whom, so the endpoint is owner-only. Showing an
         editable form to an admin would just 403 on save. -->
    <p class="rounded-md border border-zinc-800/80 bg-zinc-900/40 px-4 py-3 text-sm text-zinc-400">
      Only the workspace owner can change single sign-on. Ask them to update it.
    </p>
  {:else}
  <form onsubmit={saveOidc} class="space-y-3">
    <div class="flex flex-wrap gap-3">
      <div class="flex flex-1 flex-col gap-1.5">
        <label for="ssoissuer" class="text-xs font-medium text-zinc-400">Issuer URL</label>
        <input id="ssoissuer" bind:value={ssoIssuer} placeholder="https://auth.example.com" class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60" />
      </div>
      <div class="flex flex-col gap-1.5">
        <label for="ssorole" class="text-xs font-medium text-zinc-400">Default role for new members</label>
        <select id="ssorole" bind:value={ssoRole} class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm capitalize outline-none focus:border-amber-400/60">
          <option value="viewer">viewer</option>
          <option value="member">member</option>
          <option value="admin">admin</option>
        </select>
      </div>
    </div>
    <div class="flex flex-wrap gap-3">
      <div class="flex flex-1 flex-col gap-1.5">
        <label for="ssocid" class="text-xs font-medium text-zinc-400">Client ID</label>
        <input id="ssocid" bind:value={ssoClientId} placeholder="client id" class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60" />
      </div>
      <div class="flex flex-1 flex-col gap-1.5">
        <label for="ssosecret" class="text-xs font-medium text-zinc-400">{oidc?.issuer ? 'Replace client secret' : 'Client secret'}</label>
        <input id="ssosecret" type="password" bind:value={ssoSecret} placeholder={oidc?.issuer ? 'leave blank to keep' : 'client secret'} class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60" />
      </div>
    </div>
    <label class="flex items-center gap-2 text-sm text-zinc-300">
      <input type="checkbox" bind:checked={ssoEnabled} class="h-4 w-4 rounded border-zinc-700 bg-zinc-900 accent-amber-400" />
      Enable SSO sign-in for this workspace
    </label>
    <div class="flex items-center gap-3">
      <button type="submit" disabled={ssoBusy} class="rounded-md bg-amber-400 px-3.5 py-2 text-sm font-medium text-zinc-950 transition-colors hover:bg-amber-300 active:translate-y-px disabled:opacity-60">
        {ssoBusy ? 'Saving...' : 'Save SSO'}
      </button>
      {#if oidc?.issuer}
        <button type="button" onclick={disconnectOidc} class="text-xs text-zinc-600 transition-colors hover:text-rose-400">Disconnect</button>
      {/if}
    </div>
  </form>
  {/if}

  <h2 class="mt-14 text-xl font-semibold tracking-tight">AI triage</h2>
  <p class="mt-1 mb-4 text-sm text-zinc-500">
    Point Flare at your own model endpoint (OpenAI-compatible or Anthropic Messages - OpenAI, a self-hosted
    vLLM/Ollama, an EU provider). Triage runs on <strong>your</strong> endpoint and PII is scrubbed from the issue
    before it is sent - so personal data never leaves your boundary.
  </p>

  {#if ai?.enabled}
    <div class="mb-4 flex items-center gap-3 rounded-md border border-zinc-800/80 bg-zinc-900/40 px-4 py-3">
      <span class="inline-block h-1.5 w-1.5 rounded-full bg-emerald-400"></span>
      <span class="text-sm text-zinc-200"><span class="font-mono">{ai.model}</span> via <span class="font-mono">{ai.base_url}</span></span>
      <button onclick={disconnectAi} class="ml-auto text-xs text-zinc-600 transition-colors hover:text-rose-400">Disconnect</button>
    </div>
  {/if}

  <form onsubmit={saveAi} class="space-y-3">
    <div class="flex flex-wrap gap-3">
      <div class="flex flex-1 flex-col gap-1.5">
        <label for="aibase" class="text-xs font-medium text-zinc-400">Endpoint base URL</label>
        <input id="aibase" bind:value={aiBase} placeholder="https://api.openai.com/v1" class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60" />
      </div>
      <div class="flex flex-col gap-1.5">
        <label for="aifmt" class="text-xs font-medium text-zinc-400">API format</label>
        <select id="aifmt" bind:value={aiFormat} class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60">
          <option value="openai">OpenAI-compatible</option>
          <option value="anthropic">Anthropic Messages</option>
        </select>
      </div>
    </div>
    <div class="flex flex-wrap gap-3">
      <div class="flex flex-col gap-1.5">
        <label for="aimodel" class="text-xs font-medium text-zinc-400">Model</label>
        <input id="aimodel" bind:value={aiModel} placeholder="gpt-4o-mini" class="w-48 rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60" />
      </div>
      <div class="flex flex-1 flex-col gap-1.5">
        <label for="aikey" class="text-xs font-medium text-zinc-400">{ai?.base_url ? 'Replace API key' : 'API key'}</label>
        <input id="aikey" type="password" bind:value={aiKey} placeholder={ai?.base_url ? 'leave blank to keep' : 'sk-...'} class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60" />
      </div>
    </div>
    <label class="flex items-center gap-2 text-sm text-zinc-300">
      <input type="checkbox" bind:checked={aiEnabled} class="h-4 w-4 rounded border-zinc-700 bg-zinc-900 accent-amber-400" />
      Enable AI triage on issues
    </label>
    <label class="flex items-center gap-2 text-sm text-zinc-300">
      <input type="checkbox" bind:checked={aiAutoTriage} class="h-4 w-4 rounded border-zinc-700 bg-zinc-900 accent-amber-400" />
      Auto-triage every new issue on ingest
    </label>
    <div class="flex items-center gap-2 text-sm text-zinc-400">
      <span>Daily triage budget</span>
      <input
        type="number"
        min="0"
        max="10000"
        bind:value={aiBudget}
        class="w-24 rounded-md border border-zinc-800 bg-zinc-900 px-2.5 py-1.5 text-sm text-zinc-200 outline-none focus:border-amber-400/60"
      />
      <span class="text-xs text-zinc-600">completions per day, caps BYOAI spend</span>
    </div>
    <div class="flex items-center gap-3">
      <button type="submit" disabled={aiBusy} class="rounded-md bg-amber-400 px-3.5 py-2 text-sm font-medium text-zinc-950 transition-colors hover:bg-amber-300 active:translate-y-px disabled:opacity-60">
        {aiBusy ? 'Saving...' : 'Save AI'}
      </button>
      {#if ai?.base_url}
        <button type="button" onclick={disconnectAi} class="text-xs text-zinc-600 transition-colors hover:text-rose-400">Disconnect</button>
      {/if}
    </div>
  </form>

  <h2 class="mt-14 text-xl font-semibold tracking-tight">Audit log</h2>
  <p class="mt-1 mb-6 text-sm text-zinc-500">Sensitive actions across the workspace, newest first.</p>
  {#if audit.length}
    <ul class="divide-y divide-zinc-800/60 text-sm">
      {#each audit as a (a.created_at + a.action + a.target)}
        <li class="flex items-center gap-3 py-2.5">
          <span class="font-mono text-xs capitalize text-zinc-200">{fmtAction(a.action)}</span>
          {#if a.target}<span class="truncate text-zinc-400">{a.target}</span>{/if}
          <span class="ml-auto shrink-0 text-xs text-zinc-600">{a.actor}</span>
          <span class="shrink-0 text-xs text-zinc-600">{relativeTime(a.created_at)}</span>
        </li>
      {/each}
    </ul>
  {:else}
    <p class="text-sm text-zinc-500">No audited actions yet.</p>
  {/if}

  <h2 class="mt-14 text-xl font-semibold tracking-tight">Data</h2>
  <p class="mt-1 mb-4 text-sm text-zinc-500">
    Download the workspace's structured data (members, projects, issues, alert rules, channels, releases) as JSON.
  </p>
  <button
    onclick={exportData}
    disabled={exporting}
    class="rounded-md border border-zinc-700 px-3.5 py-2 text-sm text-zinc-200 transition-colors hover:bg-zinc-800 active:translate-y-px disabled:opacity-60"
  >
    {exporting ? 'Preparing...' : 'Export workspace data'}
  </button>

  <!-- Account: available to every signed-in role, including viewer. Changing
       your own password is an account action, not a workspace one. -->
  <div class="mt-14 rounded-lg border border-zinc-800/80 p-5">
    <h3 class="text-sm font-medium text-zinc-200">Change password</h3>
    <p class="mt-1 max-w-2xl text-xs text-zinc-500">
      Changing your password signs you out everywhere else. Your API keys keep working.
    </p>
    <form onsubmit={changePassword} class="mt-3 flex flex-wrap items-end gap-3">
      <div class="flex flex-col gap-1.5">
        <label for="pw-cur" class="text-xs font-medium text-zinc-400">Current password</label>
        <input
          id="pw-cur"
          type="password"
          autocomplete="current-password"
          bind:value={pwCurrent}
          class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
        />
      </div>
      <div class="flex flex-col gap-1.5">
        <label for="pw-new" class="text-xs font-medium text-zinc-400">New password</label>
        <input
          id="pw-new"
          type="password"
          autocomplete="new-password"
          bind:value={pwNew}
          class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
        />
      </div>
      <div class="flex flex-col gap-1.5">
        <label for="pw-confirm" class="text-xs font-medium text-zinc-400">Repeat new password</label>
        <input
          id="pw-confirm"
          type="password"
          autocomplete="new-password"
          bind:value={pwConfirm}
          class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
        />
      </div>
      <button
        type="submit"
        disabled={pwBusy || !pwCurrent || !pwNew}
        class="rounded-md bg-amber-400 px-3.5 py-2 text-sm font-medium text-zinc-950 transition-colors hover:bg-amber-300 active:translate-y-px disabled:opacity-60"
      >
        {pwBusy ? 'Changing...' : 'Change password'}
      </button>
    </form>
    {#if pwError}
      <p class="mt-3 text-sm text-rose-400">{pwError}</p>
    {/if}
    {#if pwDone}
      <p class="mt-3 text-sm text-emerald-400">
        Password changed. Any other sessions have been signed out.
      </p>
    {/if}
  </div>

  {#if isOwner}
    <div class="mt-14 rounded-lg border border-rose-900/50 bg-rose-950/20 p-5">
      <h3 class="text-sm font-medium text-rose-300">Delete workspace</h3>
      <p class="mt-1 max-w-2xl text-xs text-zinc-500">
        Permanently erase this workspace and everything in it - every project, issue, log, trace, member, key and setting. This cannot be undone.
      </p>
      <div class="mt-3 flex flex-wrap items-center gap-2">
        <input
          bind:value={confirmDelete}
          placeholder="Type DELETE to confirm"
          class="min-w-[14rem] flex-1 rounded-md border border-zinc-800 bg-zinc-950 px-2.5 py-1.5 text-sm outline-none focus:border-rose-500/60"
        />
        <button
          onclick={deleteOrg}
          disabled={confirmDelete !== 'DELETE' || deletingOrg}
          class="rounded-md border border-rose-700/70 bg-rose-600/10 px-3 py-1.5 text-sm text-rose-300 transition-colors hover:bg-rose-600/20 active:translate-y-px disabled:cursor-not-allowed disabled:opacity-40"
        >
          {deletingOrg ? 'Deleting...' : 'Delete workspace'}
        </button>
      </div>

      {#if partialErasure}
        <!-- Shown INSTEAD of navigating away. Telling someone their data is gone
             when it is not is the one outcome this flow must never produce, and
             once we navigate there is no workspace left to come back and ask. -->
        <div class="mt-4 rounded-md border border-amber-400/40 bg-amber-400/10 p-4">
          <p class="text-sm font-medium text-amber-200">Workspace data was only partly erased</p>
          <p class="mt-2 text-xs text-amber-100/90">
            Everything in the live database has been erased. Older telemetry that had already been
            archived to object storage was <strong>not</strong> erased, because Flare cannot rewrite
            those files automatically.
          </p>
          <p class="mt-2 text-xs text-amber-100/70">
            Reason: {partialErasure.cold_tier_note}
          </p>
          <p class="mt-2 text-xs text-amber-100/90">
            Ask whoever operates this Flare instance to erase the archived copies. The request has
            been recorded on the server so it is not lost.
          </p>
          <button
            onclick={() => {
              partialErasure = null;
              session.user = null;
              goto('/login', { replaceState: true });
            }}
            class="mt-3 rounded-md border border-amber-400/40 px-3 py-1.5 text-xs text-amber-100 transition-colors hover:bg-amber-400/10"
          >
            I understand, sign me out
          </button>
        </div>
      {/if}
    </div>
  {/if}
{/if}
