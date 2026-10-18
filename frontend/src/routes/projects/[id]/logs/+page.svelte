<script lang="ts">
  import { onMount } from 'svelte';
  import ProjectTabs from '$lib/components/ProjectTabs.svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { relativeTime, levelColor } from '$lib/format';
  import type { LogRow, Project } from '$lib/types';

  const id = $derived(page.params.id ?? '');

  let project = $state<Project | null>(null);
  let logs = $state<LogRow[] | null>(null);
  // Default to 24h rather than "everything". Before this the query scanned the
  // whole retained history and returned the newest 100, so an operator looking
  // at Tuesday's incident had no way to ask for Tuesday.
  let hours = $state(24);
  let nextBefore = $state<string | null>(null);
  let loadingOlder = $state(false);
  let error = $state<string | null>(null);
  let q = $state('');
  let severity = $state('');
  // A trace filter arrives via ?trace=<id> (the "view logs for this trace"
  // pivot from a trace or an error). Shown as a removable chip.
  let trace = $state(page.url.searchParams.get('trace') ?? '');

  $effect(() => {
    if (session.loaded && !session.user) goto('/login', { replaceState: true });
  });

  onMount(async () => {
    try {
      project = await api.project(id);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load project';
    }
    load();
  });

  async function load() {
    logs = null;
    nextBefore = null;
    try {
      const page = await api.logs(id, {
        q: q.trim() || undefined,
        severity: severity || undefined,
        trace: trace || undefined,
        hours: hours || undefined
      });
      logs = page.logs;
      nextBefore = page.next_before ?? null;
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load logs';
      logs = [];
    }
  }

  // Appends the next page instead of replacing, so scrolling back through an
  // incident keeps what is already on screen. Paging is keyset (from the oldest
  // row shown), so rows arriving during the read cannot shift the window and
  // cause skips or repeats the way OFFSET would.
  async function loadOlder() {
    if (!nextBefore || loadingOlder) return;
    loadingOlder = true;
    try {
      const page = await api.logs(id, {
        q: q.trim() || undefined,
        severity: severity || undefined,
        trace: trace || undefined,
        hours: hours || undefined,
        before: nextBefore
      });
      logs = [...(logs ?? []), ...page.logs];
      nextBefore = page.next_before ?? null;
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load older logs';
    } finally {
      loadingOlder = false;
    }
  }

  function clearTrace() {
    trace = '';
    goto(`/projects/${id}/logs`, { replaceState: true, keepFocus: true, noScroll: true });
    load();
  }
</script>

{#if project}
  <div class="mb-6 flex items-center gap-2 text-sm text-zinc-500">
    <a href="/projects" class="hover:text-zinc-300">Projects</a>
    <span>/</span>
    <span class="text-zinc-300">{project.name}</span>
  </div>

  <ProjectTabs projectId={id} active="logs" />

  <div class="mb-4 flex flex-wrap items-center gap-2">
    <input
      bind:value={q}
      onkeydown={(e) => e.key === 'Enter' && load()}
      placeholder="Search log body..."
      class="min-w-[12rem] flex-1 rounded-md border border-zinc-800 bg-zinc-950 px-2.5 py-1.5 text-sm outline-none focus:border-amber-400/60"
    />
    <select
      bind:value={severity}
      onchange={load}
      class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm outline-none focus:border-amber-400/60"
    >
      <option value="">All levels</option>
      <option value="fatal">Fatal</option>
      <option value="error">Error</option>
      <option value="warn">Warn</option>
      <option value="info">Info</option>
      <option value="debug">Debug</option>
    </select>
    <!-- Time range. The store and SQL always supported a `since` filter; no
         control ever set it, so every search silently meant "newest 100 of
         everything ever retained". -->
    <select
      bind:value={hours}
      onchange={load}
      aria-label="Time range"
      class="rounded-md border border-zinc-800 bg-zinc-950 px-2 py-1.5 text-sm outline-none focus:border-amber-400/60"
    >
      <option value={1}>Last hour</option>
      <option value={6}>Last 6 hours</option>
      <option value={24}>Last 24 hours</option>
      <option value={24 * 7}>Last 7 days</option>
      <option value={24 * 30}>Last 30 days</option>
      <option value={0}>All retained</option>
    </select>
    <button
      onclick={load}
      class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm text-zinc-200 transition-colors hover:bg-zinc-800 active:translate-y-px"
    >
      Search
    </button>
    {#if trace}
      <span class="inline-flex items-center gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/10 px-2 py-1 text-xs text-amber-300">
        trace {trace.slice(0, 16)}
        <button onclick={clearTrace} aria-label="Clear trace filter" class="text-amber-300/70 hover:text-amber-100">&times;</button>
      </span>
    {/if}
  </div>

  {#if logs === null}
    <div class="divide-y divide-zinc-800/60">
      {#each Array(6) as _, i (i)}<div class="h-10 animate-pulse bg-zinc-900/30"></div>{/each}
    </div>
  {:else if logs.length === 0}
    <div class="rounded-lg border border-dashed border-zinc-800 px-6 py-14 text-center">
      <p class="text-sm font-medium text-zinc-300">No logs</p>
      <p class="mx-auto mt-1 max-w-md text-sm text-zinc-500">
        {#if trace}
          No logs carry this trace id.
        {:else}
          Ship structured logs via OTLP to <code class="font-mono">{project.otlp_endpoint}/v1/logs</code>
          or the native <code class="font-mono">/logs</code> endpoint with header <code class="font-mono">x-flare-key</code>.
        {/if}
      </p>
    </div>
  {:else}
    <ul class="divide-y divide-zinc-800/60 font-mono text-[13px]">
      {#each logs as l (l.id)}
        <li class="flex items-start gap-3 py-2">
          <span class="mt-0.5 w-12 shrink-0 text-right text-[11px] uppercase {levelColor(l.severity)}">{l.severity}</span>
          <span class="min-w-0 flex-1 break-words text-zinc-300">{l.body}</span>
          {#if l.trace_id}
            <a
              href="/projects/{id}/traces/{l.trace_id}"
              title="View trace {l.trace_id}"
              class="shrink-0 text-[11px] text-zinc-600 transition-colors hover:text-amber-300"
            >
              trace
            </a>
          {/if}
          <span class="shrink-0 text-[11px] text-zinc-600">{relativeTime(l.observed_at)}</span>
        </li>
      {/each}
    </ul>
    {#if nextBefore}
      <!-- Without this the list simply stopped at 100 rows with nothing saying
           more existed, so an operator scrolling to the bottom concluded the
           incident had no earlier logs. -->
      <div class="mt-4 flex justify-center">
        <button
          onclick={loadOlder}
          disabled={loadingOlder}
          class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm text-zinc-300 transition-colors hover:bg-zinc-800 active:translate-y-px disabled:opacity-50"
        >
          {loadingOlder ? 'Loading...' : 'Load older'}
        </button>
      </div>
    {:else if logs.length > 0}
      <p class="mt-4 text-center text-xs text-zinc-600">No older records in this range.</p>
    {/if}
  {/if}
{:else if error}
  <p class="rounded-md border border-rose-900/60 bg-rose-950/40 px-3 py-2 text-sm text-rose-300">{error}</p>
{:else}
  <div class="flex min-h-[30vh] items-center justify-center text-sm text-zinc-500">Loading...</div>
{/if}
