<script lang="ts">
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import ProjectTabs from '$lib/components/ProjectTabs.svelte';
  import type { AutomationDeliveryReceipt, Project } from '$lib/types';

  const id = $derived(page.params.id ?? '');
  const issueId = $derived(page.url.searchParams.get('issue_id') ?? '');
  let issueInput = $state('');
  let project = $state<Project | null>(null);
  let deliveries = $state<AutomationDeliveryReceipt[] | null>(null);
  let nextCursor = $state<string | null>(null);
  let loading = $state(false);
  let loadingOlder = $state(false);
  let olderLoaded = $state(false);
  let error = $state<string | null>(null);
  let updatedAt = $state<string | null>(null);
  let requestVersion = 0;
  const nf = new Intl.NumberFormat();

  $effect(() => {
    if (session.loaded && !session.user) goto('/login', { replaceState: true });
    if (session.loaded && session.user && id) {
      issueInput = issueId;
      load(id, issueId, true);
    }
  });

  onMount(() => {
    const timer = setInterval(() => {
      if (session.user && id && !loading && !loadingOlder && !olderLoaded) load(id, issueId);
    }, 60_000);
    return () => { clearInterval(timer); requestVersion++; };
  });

  async function load(projectId: string, filter: string, clear = false) {
    const version = ++requestVersion;
    loading = true;
    loadingOlder = false;
    if (clear) {
      project = null;
      deliveries = null;
      nextCursor = null;
      updatedAt = null;
      error = null;
      olderLoaded = false;
    }
    try {
      const [nextProject, result] = await Promise.all([
        api.project(projectId), api.automationDeliveries(projectId, { issueId: filter })
      ]);
      if (version !== requestVersion) return;
      project = nextProject;
      deliveries = result.items;
      nextCursor = result.next_cursor ?? null;
      olderLoaded = false;
      updatedAt = new Date().toLocaleTimeString();
      error = null;
    } catch (err) {
      if (version === requestVersion) error = err instanceof ApiError ? err.message : 'Failed to load automation deliveries';
    } finally {
      if (version === requestVersion) loading = false;
    }
  }

  async function loadOlder() {
    if (!nextCursor || loading || loadingOlder) return;
    const version = requestVersion;
    loadingOlder = true;
    try {
      const result = await api.automationDeliveries(id, { issueId, cursor: nextCursor });
      if (version !== requestVersion) return;
      const known = new Set((deliveries ?? []).map(row => row.notice_id));
      deliveries = [...(deliveries ?? []), ...result.items.filter(row => !known.has(row.notice_id))];
      nextCursor = result.next_cursor ?? null;
      olderLoaded = true;
      error = null;
    } catch (err) {
      if (version === requestVersion) error = err instanceof ApiError ? err.message : 'Failed to load older deliveries';
    } finally {
      if (version === requestVersion) loadingOlder = false;
    }
  }

  function filter(event: SubmitEvent) {
    event.preventDefault();
    const query = new URLSearchParams();
    if (issueInput.trim()) query.set('issue_id', issueInput.trim());
    goto(`/projects/${encodeURIComponent(id)}/automation${query.size ? `?${query}` : ''}`);
  }

  function deliveryStatus(row: AutomationDeliveryReceipt): { label: string; style: string } {
    if (row.state === 'pending') return { label: 'Queued', style: 'text-amber-300 border-amber-700/50' };
    if (row.state === 'leased') return { label: 'Sending', style: 'text-amber-300 border-amber-700/50' };
    if (row.state === 'dead') return { label: 'Retries exhausted', style: 'text-rose-300 border-rose-800/60' };
    if (row.state === 'cancelled') return { label: 'Cancelled', style: 'text-zinc-400 border-zinc-700' };
    if (row.state === 'delivered') {
      const labels = { http_delivery: 'HTTP delivered', receiver_enqueued: 'Workflow accepted', test_noop: 'Test accepted', unrecorded: 'Delivered · no receipt' };
      return { label: labels[row.acknowledgement] ?? 'Delivered · no receipt', style: row.acknowledgement === 'unrecorded' ? 'text-zinc-400 border-zinc-700' : 'text-emerald-300 border-emerald-800/60' };
    }
    return { label: 'Unknown delivery state', style: 'text-zinc-400 border-zinc-700' };
  }

  function timestamp(value: string): string { return new Date(value).toLocaleString(); }
  function issueLink(value: string): string | null {
    return /^[A-Za-z0-9_-]{1,128}$/.test(value) ? `/issues/${encodeURIComponent(value)}` : null;
  }
</script>

<div class="mb-6 flex flex-wrap items-center gap-2 text-sm text-zinc-400">
  <a href="/projects" class="hover:text-zinc-200">Projects</a><span>/</span>
  <a href={`/projects/${encodeURIComponent(id)}`} class="hover:text-zinc-200">{project?.name ?? 'Project'}</a>
</div>

<div class="mb-6 flex flex-wrap items-end justify-between gap-4">
  <div>
    <h1 class="text-xl font-semibold tracking-tight">Automation</h1>
    <p class="mt-1 text-sm text-zinc-400">Follow alert delivery to your connected workflows.</p>
  </div>
  <div class="flex flex-wrap items-center gap-3">
    {#if updatedAt}<span class="text-xs text-zinc-400">Updated at {updatedAt}</span>{/if}
    <button type="button" disabled={loading || loadingOlder} onclick={() => load(id, issueId)} class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm text-zinc-300 hover:bg-zinc-800 disabled:opacity-50">{loading ? 'Loading…' : 'Refresh'}</button>
  </div>
</div>

<ProjectTabs projectId={id} active="automation" />

<div class="mb-5 rounded-lg border border-amber-500/30 bg-amber-500/5 p-4 text-sm text-zinc-300">
  <p>HTTP delivery confirms a response. Workflow acceptance records the receiver's run reference. Execution, human approval and deployment verification have their own evidence.</p>
  <p class="mt-1 text-xs text-zinc-400">A test confirms the connection without starting a workflow. Historical deliveries can have no recorded acknowledgement.</p>
</div>

<form onsubmit={filter} class="mb-5 flex flex-wrap items-end gap-3">
  <div class="min-w-0 flex-1">
    <label for="automation-issue" class="mb-1.5 block text-xs font-medium text-zinc-400">Filter by issue ID</label>
    <input id="automation-issue" bind:value={issueInput} maxlength={128} placeholder="All project alerts" class="w-full rounded-md border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm outline-none focus:border-amber-400/60" />
  </div>
  <button type="submit" class="rounded-md border border-zinc-700 px-3 py-2 text-sm text-zinc-300 hover:bg-zinc-800">Apply filter</button>
</form>

{#if error}
  <div role="alert" class="mb-5 rounded-lg border border-rose-900/50 bg-rose-950/20 p-4 text-sm text-rose-300">{error}{deliveries !== null ? '. Showing the last loaded snapshot.' : ''}</div>
{/if}

{#if deliveries === null && !error}
  <div class="rounded-lg border border-zinc-800 p-8 text-sm text-zinc-400">Loading automation deliveries…</div>
{:else if deliveries}
  {#if deliveries.length === 0}
    <div class="rounded-lg border border-dashed border-zinc-800 px-6 py-10">
      <h2 class="text-sm font-medium text-zinc-200">No automation deliveries{issueId ? ' for this issue' : ' in this project'}</h2>
      <p class="mt-2 text-sm text-zinc-400">Deliveries appear when an alert is routed to an automation webhook. An empty history does not verify the connection.</p>
      <a href="/settings" class="mt-4 inline-block text-sm text-amber-400 hover:text-amber-300">View alert channels →</a>
    </div>
  {:else}
    <p class="mb-3 text-xs text-zinc-400">{nf.format(deliveries.length)} deliveries loaded · newest queued first{olderLoaded ? ' · automatic refresh paused while viewing older deliveries' : ''}</p>
    <ul class="space-y-3" aria-label="Automation deliveries">
      {#each deliveries as row (row.notice_id)}
        {@const status = deliveryStatus(row)}
        {@const href = issueLink(row.issue_id)}
        <li class="min-w-0 rounded-lg border border-zinc-800/80 bg-zinc-900/40 p-4 sm:p-5">
          <div class="flex flex-wrap items-center gap-2">
            <span class="rounded-full border px-2 py-0.5 text-xs {status.style}">{status.label}</span>
            <h2 class="text-sm font-medium text-zinc-200">{row.event.replaceAll('_', ' ')}</h2>
            <span class="text-xs text-zinc-400">{nf.format(row.attempts)} {row.attempts === 1 ? 'attempt' : 'attempts'}</span>
          </div>
          <dl class="mt-4 grid min-w-0 gap-4 text-xs sm:grid-cols-2">
            <div class="min-w-0"><dt class="text-zinc-500">Issue</dt><dd class="mt-1 break-all">{#if href}<a href={href} class="text-amber-400 hover:text-amber-300">{row.issue_id}</a>{:else}{row.issue_id || 'Project alert'}{/if}</dd></div>
            <div><dt class="text-zinc-500">Alert observed</dt><dd class="mt-1 text-zinc-300" title={row.observed_at}>{timestamp(row.observed_at)}</dd></div>
            {#if row.receiver_run_id}<div class="min-w-0"><dt class="text-zinc-500">Receiver run</dt><dd class="mt-1 break-all font-mono text-zinc-200">{row.receiver_run_id}</dd></div>{/if}
            {#if row.acknowledged_at}<div><dt class="text-zinc-500">Acknowledged</dt><dd class="mt-1 text-zinc-300" title={row.acknowledged_at}>{timestamp(row.acknowledged_at)}{row.http_status ? ` · HTTP ${row.http_status}` : ''}</dd></div>{/if}
          </dl>
          {#if row.receiver_deduped}<p class="mt-3 text-xs text-zinc-400">The receiver confirmed a previously accepted run.</p>{/if}
          {#if row.acknowledgement === 'test_noop'}<p class="mt-3 text-xs text-zinc-400">Connection test accepted without dispatch.</p>{/if}
          <details class="mt-4 border-t border-zinc-800 pt-3 text-xs">
            <summary class="cursor-pointer text-zinc-400 hover:text-zinc-200">Delivery reference</summary>
            <dl class="mt-3 space-y-3">
              <div><dt class="text-zinc-500">Notice ID</dt><dd class="mt-1 break-all font-mono text-zinc-300">{row.notice_id}</dd></div>
              {#if row.request_sha256}<div><dt class="text-zinc-500">Request SHA-256</dt><dd class="mt-1 break-all font-mono text-zinc-300">{row.request_sha256}</dd></div>{/if}
              <div><dt class="text-zinc-500">Event count at observation</dt><dd class="mt-1 text-zinc-300">{nf.format(row.event_count)}</dd></div>
            </dl>
          </details>
        </li>
      {/each}
    </ul>
    {#if nextCursor}<button type="button" disabled={loading || loadingOlder} onclick={loadOlder} class="mt-5 rounded-md border border-zinc-700 px-4 py-2 text-sm text-zinc-300 hover:bg-zinc-800 disabled:opacity-50">{loadingOlder ? 'Loading older…' : 'Load older'}</button>{/if}
  {/if}
{/if}
