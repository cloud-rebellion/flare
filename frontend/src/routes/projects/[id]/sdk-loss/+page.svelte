<script lang="ts">
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { formatExactQuantity } from '$lib/format';
  import ProjectTabs from '$lib/components/ProjectTabs.svelte';
  import type { Project, SdkDiscards } from '$lib/types';

  const id = $derived(page.params.id ?? '');
  let project = $state<Project | null>(null);
  let data = $state<SdkDiscards | null>(null);
  let hours = $state(24);
  let loading = $state(false);
  let error = $state<string | null>(null);
  let updatedAt = $state<string | null>(null);
  let requestVersion = 0;
  const nf = new Intl.NumberFormat();

  $effect(() => {
    if (session.loaded && !session.user) goto('/login', { replaceState: true });
    if (session.loaded && session.user && id) load(id, hours);
  });

  onMount(() => {
    const timer = setInterval(() => {
      if (session.user && id) load(id, hours);
    }, 60_000);
    return () => clearInterval(timer);
  });

  async function load(projectId: string, windowHours: number) {
    const version = ++requestVersion;
    loading = true;
    try {
      const [nextProject, nextData] = await Promise.all([
        api.project(projectId),
        api.sdkDiscards(projectId, windowHours)
      ]);
      if (version !== requestVersion) return;
      project = nextProject;
      data = nextData;
      updatedAt = new Date().toLocaleTimeString();
      error = null;
    } catch (err) {
      if (version !== requestVersion) return;
      error = err instanceof ApiError ? err.message : 'Failed to load SDK loss reports';
    } finally {
      if (version === requestVersion) loading = false;
    }
  }

  function timestamp(value: string): string {
    return new Date(value).toLocaleString();
  }
</script>

<div class="mb-6 flex flex-wrap items-center gap-2 text-sm text-zinc-400">
  <a href="/projects" class="hover:text-zinc-200">Projects</a>
  <span>/</span>
  <a href={`/projects/${id}`} class="hover:text-zinc-200">{project?.name ?? 'Project'}</a>
</div>

<div class="mb-6 flex flex-wrap items-end justify-between gap-4">
  <div>
    <h1 class="text-xl font-semibold tracking-tight">Client SDK loss</h1>
    <p class="mt-1 text-sm text-zinc-400">Telemetry your app SDK reports it sampled, filtered, or could not send.</p>
  </div>
  <div class="flex flex-wrap items-center gap-3">
    {#if updatedAt}<span class="text-xs text-zinc-400">Updated at {updatedAt}</span>{/if}
    <select aria-label="Report window" bind:value={hours} class="rounded-md border border-zinc-700 bg-zinc-950 px-2 py-1.5 text-sm">
      <option value={1}>Last hour</option>
      <option value={6}>Last 6 hours</option>
      <option value={24}>Last 24 hours</option>
      <option value={168}>Last 7 days</option>
    </select>
    <button type="button" disabled={loading} onclick={() => load(id, hours)} class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm text-zinc-300 hover:bg-zinc-800 disabled:opacity-50">{loading ? 'Loading…' : 'Refresh'}</button>
  </div>
</div>

<ProjectTabs projectId={id} active="sdk-loss" />

{#if error}
  <div role="alert" class="mb-5 rounded-lg border border-rose-900/50 bg-rose-950/20 p-4 text-sm text-rose-300">
    {error}{data ? '. Showing the last loaded snapshot.' : ''}
  </div>
{/if}

<div class="mb-5 rounded-lg border border-amber-500/30 bg-amber-500/5 p-4 text-sm text-zinc-300">
  <p>These are reports from client SDKs. Sampling and filters can be intentional; delivery failures can reveal a monitoring gap.</p>
  <p class="mt-1 text-xs text-zinc-400">Counts use each category's own units. Report counts can overlap across rows, so neither column should be added into a single total. No report observed does not prove that every event arrived.</p>
</div>

{#if !data && !error}
  <div class="rounded-lg border border-zinc-800 p-8 text-sm text-zinc-400">Loading SDK reports…</div>
{:else if data}
  <section class="overflow-hidden rounded-lg border border-zinc-800/80 bg-zinc-900/40">
    <div class="flex flex-wrap items-center justify-between gap-2 border-b border-zinc-800/80 px-5 py-4">
      <h2 class="text-sm font-semibold">Reported losses by reason and category</h2>
      <span class="text-xs text-zinc-400">Client reported · received in last {data.window_hours === 1 ? 'hour' : `${data.window_hours} hours`}</span>
    </div>
    {#if data.breakdown.length === 0}
      <p class="px-5 py-10 text-sm text-zinc-400">No accepted SDK loss counters observed in this window. SDK reporting may be disabled, delayed, or unable to reach Flare.</p>
    {:else}
      <div class="overflow-x-auto">
        <table class="w-full min-w-[760px] text-left text-sm">
          <thead class="bg-zinc-900/60 text-xs text-zinc-400">
            <tr>
              <th class="px-5 py-3 font-medium">Reason</th>
              <th class="px-4 py-3 font-medium">Category</th>
              <th class="px-4 py-3 font-medium">Kind</th>
              <th class="px-4 py-3 text-right font-medium">Quantity</th>
              <th class="px-4 py-3 text-right font-medium">Reports</th>
              <th class="px-5 py-3 font-medium">Last received</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-zinc-800/60">
            {#each data.breakdown as row (row.reason + ':' + row.category)}
              <tr>
                <td class="px-5 py-3 font-mono text-xs text-zinc-200">{row.reason}</td>
                <td class="px-4 py-3 text-zinc-300">{row.category}</td>
                <td class="px-4 py-3 text-xs {['delivery', 'sdk_error'].includes(row.kind) ? 'text-amber-400' : 'text-zinc-400'}">{row.kind.replaceAll('_', ' ')}</td>
                <td class="px-4 py-3 text-right font-mono text-zinc-100">{formatExactQuantity(row.quantity)}</td>
                <td class="px-4 py-3 text-right font-mono text-zinc-400">{nf.format(row.reports)}</td>
                <td class="px-5 py-3 text-xs text-zinc-400" title={row.last_received_at}>{timestamp(row.last_received_at)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>
{/if}
