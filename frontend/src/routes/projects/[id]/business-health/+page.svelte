<script lang="ts">
  import { onMount } from 'svelte';
  import ProjectTabs from '$lib/components/ProjectTabs.svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { relativeTime } from '$lib/format';
  import type { BusinessCheckKey, BusinessHealth, Project } from '$lib/types';

  const id = $derived(page.params.id ?? '');
  let project = $state<Project | null>(null);
  let rows = $state<BusinessHealth[]>([]);
  let error = $state<string | null>(null);
  let loading = $state(true);
  let hours = $state(24);
  let businessKeys = $state<BusinessCheckKey[] | null>(null);
  let keyLoadStarted = false;
  let keyName = $state('');
  let keyExpiryDays = $state(90);
  let newBusinessKey = $state<string | null>(null);
  let keyError = $state<string | null>(null);
  let keyBusy = $state(false);
  const isAdmin = $derived(session.user?.role === 'admin' || session.user?.role === 'owner');

  $effect(() => {
    if (session.loaded && !session.user) goto('/login', { replaceState: true });
  });

  $effect(() => {
    if (project && isAdmin && !keyLoadStarted) {
      keyLoadStarted = true;
      void loadKeys();
    }
  });

  async function loadKeys() {
    try {
      businessKeys = await api.businessKeys(id);
      keyError = null;
    } catch (err) {
      keyError = err instanceof ApiError ? err.message : 'Failed to load business keys';
    }
  }

  async function createKey(event: SubmitEvent) {
    event.preventDefault();
    keyBusy = true;
    keyError = null;
    newBusinessKey = null;
    try {
      const created = await api.createBusinessKey(id, keyName, keyExpiryDays);
      newBusinessKey = created.key;
      const { key: _secret, ...metadata } = created;
      businessKeys = [metadata, ...(businessKeys ?? [])];
      keyName = '';
    } catch (err) {
      keyError = err instanceof ApiError ? err.message : 'Failed to create business key';
    } finally {
      keyBusy = false;
    }
  }

  async function revokeKey(keyID: string) {
    if (!window.confirm('Revoke this business key? Its backend will stop reporting immediately.')) return;
    keyBusy = true;
    keyError = null;
    try {
      await api.revokeBusinessKey(id, keyID);
      newBusinessKey = null;
      await loadKeys();
    } catch (err) {
      keyError = err instanceof ApiError ? err.message : 'Failed to revoke business key';
    } finally {
      keyBusy = false;
    }
  }

  async function load() {
    loading = true;
    try {
      rows = await api.businessHealth(id, hours);
      error = null;
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load business health';
      rows = [];
    } finally {
      loading = false;
    }
  }

  onMount(async () => {
    try {
      project = await api.project(id);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load project';
      loading = false;
      return;
    }
    await load();
  });
</script>

{#if error}
  <p class="mb-4 rounded-md border border-red-900/50 bg-red-950/20 px-3 py-2 text-sm text-red-300">{error}</p>
{/if}

{#if project}
  <div class="mb-6 flex items-center gap-2 text-sm text-zinc-500">
    <a href="/projects" class="hover:text-zinc-300">Projects</a>
    <span>/</span>
    <a href="/projects/{id}" class="hover:text-zinc-300">{project.name}</a>
    <span>/</span>
    <span class="text-zinc-300">Business health</span>
  </div>

  <div class="mb-5 flex items-end justify-between">
    <div>
      <h1 class="text-xl font-semibold tracking-tight">Business health</h1>
      <p class="mt-1 text-sm text-zinc-500">Payload-free outcomes reported by your apps and integrations.</p>
    </div>
    <select bind:value={hours} onchange={load} class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm text-zinc-300">
      <option value={1}>Last hour</option>
      <option value={24}>Last 24 hours</option>
      <option value={168}>Last 7 days</option>
      <option value={720}>Last 30 days</option>
    </select>
  </div>

  <ProjectTabs projectId={id} active="business" />

  {#if loading}
    <p class="text-sm text-zinc-500">Loading business health…</p>
  {:else if error}
    <button type="button" onclick={load} class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm text-zinc-300 hover:text-white">Retry</button>
  {:else if rows.length === 0}
    <div class="rounded-lg border border-zinc-800 bg-zinc-900/30 p-8 text-center text-sm text-zinc-500">
      No business checks have been accepted in this window.
    </div>
  {:else}
    <div class="overflow-x-auto rounded-lg border border-zinc-800">
      <table class="w-full text-left text-sm">
        <thead class="border-b border-zinc-800 bg-zinc-900/60 text-xs uppercase tracking-wide text-zinc-500">
          <tr><th class="px-4 py-3">Check</th><th class="px-4 py-3">Attempts</th><th class="px-4 py-3">Error rate</th><th class="px-4 py-3">Avg latency</th><th class="px-4 py-3">Last seen</th></tr>
        </thead>
        <tbody class="divide-y divide-zinc-800/70">
          {#each rows as row}
            <tr class="text-zinc-300">
              <td class="px-4 py-3 font-mono text-xs">{row.check_name}</td>
              <td class="px-4 py-3">{row.attempts} <span class="text-xs text-zinc-500">({row.ok_count} ok / {row.error_count} errors)</span></td>
              <td class="px-4 py-3 {row.error_rate > 0 ? 'text-amber-300' : 'text-emerald-300'}">{(row.error_rate * 100).toFixed(2)}%</td>
              <td class="px-4 py-3">{row.avg_elapsed_ms.toFixed(0)} ms</td>
              <td class="px-4 py-3 text-xs text-zinc-500">{row.last_completed_at ? relativeTime(row.last_completed_at) : '—'}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}

  {#if isAdmin}
    <section class="mt-8 rounded-lg border border-zinc-800 bg-zinc-900/30 p-5">
      <h2 class="text-base font-semibold text-zinc-200">Backend reporter keys</h2>
      <p class="mt-1 max-w-3xl text-sm text-zinc-500">
        Business checks can trigger alerts. Create a confidential key for a trusted backend or server-side integration. Keep it out of browser and Flutter builds. Create a replacement and verify a check before revoking an old key. A project can have eight active keys; leave one slot free for rotation.
      </p>
      <p class="mt-2 text-xs text-zinc-500">
        Send checks to <code class="font-mono text-zinc-300">POST /api/{id}/business-checks</code> with <code class="font-mono text-zinc-300">Authorization: Bearer &lt;key&gt;</code> and a unique <code class="font-mono text-zinc-300">Idempotency-Key</code> per operation.
      </p>
      <form onsubmit={createKey} class="mt-5 flex flex-wrap items-end gap-3">
        <div class="min-w-48 flex-1">
          <label for="business-key-name" class="mb-1 block text-xs text-zinc-400">Key name</label>
          <input id="business-key-name" bind:value={keyName} maxlength="64" placeholder="Production backend" class="w-full rounded-md border border-zinc-800 bg-zinc-950 px-3 py-2 text-sm text-zinc-200 outline-none focus:border-amber-400/60" />
        </div>
        <div>
          <label for="business-key-expiry" class="mb-1 block text-xs text-zinc-400">Expires in</label>
          <select id="business-key-expiry" bind:value={keyExpiryDays} class="rounded-md border border-zinc-800 bg-zinc-950 px-3 py-2 text-sm text-zinc-200">
            <option value={30}>30 days</option>
            <option value={90}>90 days</option>
            <option value={365}>365 days</option>
          </select>
        </div>
        <button type="submit" disabled={keyBusy} class="rounded-md bg-amber-400 px-3 py-2 text-sm font-medium text-zinc-950 disabled:opacity-60">{keyBusy ? 'Working…' : 'Create key'}</button>
      </form>
      {#if keyError}
        <p class="mt-4 rounded-md border border-red-900/50 bg-red-950/20 px-3 py-2 text-sm text-red-300">{keyError}</p>
        <button type="button" onclick={loadKeys} class="mt-2 text-xs text-zinc-400 hover:text-white">Retry loading keys</button>
      {/if}
      {#if newBusinessKey}
        <div class="mt-4 rounded-md border border-amber-400/40 bg-amber-400/10 px-4 py-3">
          <p class="text-sm font-medium text-amber-200">Copy this key now. Flare will not show it again.</p>
          <code class="mt-2 block select-all break-all font-mono text-xs text-amber-100">{newBusinessKey}</code>
          <button type="button" onclick={() => (newBusinessKey = null)} class="mt-2 text-xs text-amber-200 hover:text-white">Hide key</button>
        </div>
      {/if}
      {#if businessKeys}
        {#if businessKeys.length === 0}
          <p class="mt-5 text-sm text-zinc-500">No backend reporter keys yet.</p>
        {:else}
          <ul class="mt-5 divide-y divide-zinc-800/70">
            {#each businessKeys as key (key.id)}
              <li class="flex flex-wrap items-center gap-3 py-2 text-sm">
                <span class="font-medium text-zinc-200">{key.name}</span>
                <code class="text-xs text-zinc-500">{key.prefix}…</code>
                <span class="text-xs text-zinc-500">Expires {new Date(key.expires_at).toLocaleDateString()}</span>
                {#if key.revoked_at}
                  <span class="text-xs text-zinc-500">Revoked</span>
                {:else if new Date(key.expires_at).getTime() <= Date.now()}
                  <span class="text-xs text-zinc-500">Expired</span>
                {:else}
                  <button type="button" disabled={keyBusy} onclick={() => revokeKey(key.id)} class="ml-auto text-xs text-zinc-500 hover:text-rose-300 disabled:opacity-60">Revoke</button>
                {/if}
              </li>
            {/each}
          </ul>
        {/if}
      {/if}
    </section>
  {/if}
{/if}
