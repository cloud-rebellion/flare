<script lang="ts">
  import { onMount } from 'svelte';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { relativeTime } from '$lib/format';
  import type { ApiKey, ApiKeyRole } from '$lib/types';

  let { keys = $bindable(null) }: { keys?: ApiKey[] | null } = $props();
  const isAdmin = $derived(session.user?.role === 'admin' || session.user?.role === 'owner');
  const mcpKeys = $derived((keys ?? []).filter((key) => key.purpose === 'mcp'));
  let endpoint = $state('');
  let name = $state('Codex');
  let role = $state<ApiKeyRole>('viewer');
  let days = $state(30);
  let busy = $state(false);
  let error = $state<string | null>(null);
  let created = $state<{ id: string; key: string; role: ApiKeyRole } | null>(null);
  let testing = $state(false);
  let tested = $state<string | null>(null);
  let copied = $state<string | null>(null);
  const command = $derived(`codex mcp add flare --url ${JSON.stringify(endpoint)} --bearer-token-env-var FLARE_MCP_TOKEN`);
  const config = $derived(`[mcp_servers.flare]\nurl = ${JSON.stringify(endpoint)}\nbearer_token_env_var = "FLARE_MCP_TOKEN"\nstartup_timeout_sec = 20\ntool_timeout_sec = 60`);

  onMount(() => { endpoint = `${location.origin}/api/mcp`; });

  async function copy(value: string, label: string) {
    try {
      await navigator.clipboard.writeText(value);
      copied = label;
    } catch {
      error = 'Could not copy. Select and copy the text manually.';
    }
  }

  async function create(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = tested = null;
    created = null;
    try {
      const key = await api.createApiKey(name.trim() || 'MCP client', role, { purpose: 'mcp', expires_in_days: days });
      created = { id: key.id, key: key.key, role: key.role };
      keys = [{
        id: key.id, name: key.name, prefix: key.prefix, role: key.role,
        purpose: key.purpose, expires_at: key.expires_at,
        created_by: session.user?.email ?? '',
        created_at: new Date().toISOString(), last_used_at: null
      }, ...(keys ?? [])];
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Could not create an MCP token.';
    } finally { busy = false; }
  }

  async function revoke(key: ApiKey) {
    busy = true;
    error = null;
    try {
      await api.deleteApiKey(key.id);
      keys = (keys ?? []).filter((item) => item.id !== key.id);
      if (created?.id === key.id) created = null;
      tested = null;
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Could not revoke the MCP token.';
    } finally { busy = false; }
  }

  async function testToken() {
    if (!created) return;
    const token = created.key;
    testing = true;
    error = tested = null;
    try {
      // Omit cookies: a valid dashboard session must not make a broken token
      // appear to work. Only read-only protocol methods are used here.
      const call = async (method: string, params: unknown, id: number) => {
        const response = await fetch('/api/mcp', {
          method: 'POST', credentials: 'omit', redirect: 'error',
          headers: { 'Content-Type': 'application/json', 'Accept': 'application/json', 'Authorization': `Bearer ${token}` },
          body: JSON.stringify({ jsonrpc: '2.0', id, method, params }),
          signal: AbortSignal.timeout(10_000)
        });
        if (!response.ok) throw new Error(`Token check failed (HTTP ${response.status}).`);
        const data = await response.json();
        if (data.jsonrpc !== '2.0' || data.id !== id || data.error) throw new Error('Flare did not return a valid MCP response.');
        return data.result;
      };
      const initialized = await call('initialize', {
        protocolVersion: '2025-03-26', capabilities: {}, clientInfo: { name: 'flare-setup', version: '1' }
      }, 1);
      if (initialized?.serverInfo?.name !== 'flare') throw new Error('The endpoint did not identify itself as Flare.');
      const notification = await fetch('/api/mcp', {
        method: 'POST', credentials: 'omit', redirect: 'error',
        headers: { 'Content-Type': 'application/json', 'Authorization': `Bearer ${token}` },
        body: JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }),
        signal: AbortSignal.timeout(10_000)
      });
      if (!notification.ok) throw new Error('Flare did not accept MCP initialization.');
      const catalog = await call('tools/list', {}, 2);
      if (!Array.isArray(catalog?.tools)) throw new Error('Flare did not return its tool catalog.');
      tested = `Token accepted over HTTP. ${catalog.tools.length} tools advertised. Finish connecting your client below.`;
      keys = await api.apiKeys();
    } catch (err) {
      error = err instanceof Error ? err.message : 'Could not test this token.';
    } finally { testing = false; }
  }
</script>

<section aria-labelledby="mcp-heading" class="mt-14 scroll-mt-24" id="mcp">
  <div class="flex flex-wrap items-center gap-3">
    <h2 id="mcp-heading" class="text-xl font-semibold tracking-tight">Connect your AI to Flare</h2>
    <span class="rounded-full border border-emerald-400/30 bg-emerald-400/10 px-2.5 py-1 text-[11px] font-medium text-emerald-300">HTTP MCP</span>
  </div>
  <p class="mt-2 max-w-2xl text-sm leading-relaxed text-zinc-500">
    Let your AI investigate issues, stack traces, logs, and service health in this workspace.
    Create a token for each client. Tokens cover all projects in this workspace, work only with the MCP endpoint, and cannot access the management REST API.
    Read-only is the default; read and triage also allows issue status updates and AI triage.
  </p>
  {#if error}<p role="alert" class="mt-4 rounded-lg border border-rose-400/30 bg-rose-400/10 px-4 py-3 text-sm text-rose-300">{error}</p>{/if}
  {#if tested}<p role="status" class="mt-4 rounded-lg border border-emerald-400/30 bg-emerald-400/10 px-4 py-3 text-sm text-emerald-300">{tested}</p>{/if}

  <div class="mt-6 rounded-xl border border-zinc-800 bg-zinc-900/30 p-4 sm:p-6">
    <h3 class="text-sm font-semibold text-zinc-200">1. Create a workspace token</h3>
    {#if isAdmin}
      <form onsubmit={create} class="mt-4 flex flex-wrap items-end gap-3">
        <label class="min-w-0 flex-1 text-xs text-zinc-400">Client name
          <input aria-label="MCP client name" bind:value={name} maxlength="100" required class="mt-1.5 block w-full rounded-md border border-zinc-800 bg-zinc-950 px-3 py-2 text-sm text-zinc-200" />
        </label>
        <label class="text-xs text-zinc-400">Access
          <select aria-label="MCP token access" bind:value={role} class="mt-1.5 block rounded-md border border-zinc-800 bg-zinc-950 px-3 py-2 text-sm text-zinc-200">
            <option value="viewer">Read-only</option><option value="member">Read and triage</option>
          </select>
        </label>
        <label class="text-xs text-zinc-400">Expires after
          <select aria-label="MCP token expiry" bind:value={days} class="mt-1.5 block rounded-md border border-zinc-800 bg-zinc-950 px-3 py-2 text-sm text-zinc-200">
            <option value={7}>7 days</option><option value={30}>30 days</option><option value={90}>90 days</option>
          </select>
        </label>
        <button disabled={busy || testing} class="rounded-md bg-amber-400 px-3.5 py-2 text-sm font-medium text-zinc-950 disabled:opacity-50">{busy ? 'Saving…' : 'Create MCP token'}</button>
      </form>
    {:else}
      <p class="mt-3 text-sm text-zinc-500">Ask a workspace administrator to create a token for your client.</p>
    {/if}
    <p class="mt-3 text-xs leading-relaxed text-zinc-500">Workspace administrators manage these credentials. Tokens belong to the workspace, expire within 90 days, and can be revoked at any time. Keep them out of browser bundles, repositories, and chat.</p>
    {#if created}
      <div class="mt-4 rounded-lg border border-amber-400/30 bg-amber-400/5 p-4">
        <p class="text-sm font-medium text-amber-200">Save this token now. It is shown only once.</p>
        <code class="mt-2 block break-all font-mono text-xs text-amber-100">{created.key}</code>
        <div class="mt-3 flex flex-wrap gap-3">
          <button type="button" onclick={() => copy(created!.key, 'token')} class="text-xs text-amber-300">{copied === 'token' ? 'Copied' : 'Copy token'}</button>
          <button type="button" disabled={testing || busy} onclick={testToken} class="text-xs text-zinc-300 disabled:opacity-50">{testing ? 'Testing…' : 'Test token'}</button>
          <button type="button" disabled={testing} onclick={() => { created = null; }} class="text-xs text-zinc-500">Dismiss token</button>
        </div>
      </div>
    {/if}
    {#if mcpKeys.length}
      <ul class="mt-4 divide-y divide-zinc-800">
        {#each mcpKeys as key (key.id)}
          <li class="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 py-3 text-xs text-zinc-500">
            <span class="break-all font-medium text-zinc-200">{key.name}</span>
            <span>{key.role === 'member' ? 'Read and triage' : 'Read-only'}</span>
            <span>{key.expires_at && new Date(key.expires_at) <= new Date() ? 'Expired' : 'Expires'} {key.expires_at ? new Date(key.expires_at).toLocaleDateString() : 'not recorded'}</span>
            <span>{key.last_used_at ? `Used ${relativeTime(key.last_used_at)}` : 'Never used'}</span>
            {#if isAdmin}<button type="button" aria-label={`Revoke MCP token ${key.name}`} disabled={busy || testing} onclick={() => revoke(key)} class="ml-auto text-rose-400 disabled:opacity-50">Revoke</button>{/if}
          </li>
        {/each}
      </ul>
    {/if}
  </div>

  <div class="mt-4 rounded-xl border border-zinc-800 p-4 sm:p-6">
    <h3 class="text-sm font-semibold text-zinc-200">2. Connect Codex</h3>
    <p class="mt-2 text-sm leading-relaxed text-zinc-500">Make <code class="font-mono text-zinc-300">FLARE_MCP_TOKEN</code> available in the environment that launches Codex, using your secret manager or a hidden terminal prompt. Then run:</p>
    <pre class="mt-3 overflow-x-auto rounded-lg bg-zinc-950 p-3 font-mono text-xs leading-relaxed text-zinc-300">{command}</pre>
    <button type="button" onclick={() => copy(command, 'command')} class="mt-2 text-xs text-amber-400">{copied === 'command' ? 'Copied' : 'Copy command'}</button>
    <p class="mt-3 text-xs leading-relaxed text-zinc-500">Start a new client connection and check its MCP tool list. An already running desktop or IDE process may need to be relaunched from that environment to receive the variable. Configuration examples contain no token value.</p>
    <details class="mt-4">
      <summary class="cursor-pointer text-xs text-zinc-400">Codex configuration file</summary>
      <pre class="mt-3 overflow-x-auto rounded-lg bg-zinc-950 p-3 font-mono text-xs leading-relaxed text-zinc-300">{config}</pre>
      <button type="button" onclick={() => copy(config, 'config')} class="mt-2 text-xs text-amber-400">{copied === 'config' ? 'Copied' : 'Copy configuration'}</button>
    </details>
    <details class="mt-4">
      <summary class="cursor-pointer text-xs text-zinc-400">Other HTTP MCP clients</summary>
      <p class="mt-3 text-sm text-zinc-500">Choose Streamable HTTP and use this endpoint. Set the Authorization header to Bearer followed by your MCP token, using the client's secret storage or environment support.</p>
      <code class="mt-2 block break-all font-mono text-xs text-zinc-300">{endpoint}</code>
      <button type="button" onclick={() => copy(endpoint, 'endpoint')} class="mt-2 text-xs text-amber-400">{copied === 'endpoint' ? 'Copied' : 'Copy endpoint'}</button>
    </details>
  </div>
</section>
