<script lang="ts">
  import { onMount } from 'svelte';
  import ProjectTabs from '$lib/components/ProjectTabs.svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { relativeTime } from '$lib/format';
  import type { Project, TraceSummary } from '$lib/types';

  const id = $derived(page.params.id ?? '');

  let project = $state<Project | null>(null);
  let traces = $state<TraceSummary[] | null>(null);
  let error = $state<string | null>(null);

  $effect(() => {
    if (session.loaded && !session.user) goto('/login', { replaceState: true });
  });

  onMount(async () => {
    try {
      project = await api.project(id);
      traces = await api.traces(id);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load traces';
      traces = [];
    }
  });

  function fmtDur(ms: number): string {
    if (ms < 1) return '<1ms';
    if (ms < 1000) return `${Math.round(ms)}ms`;
    return `${(ms / 1000).toFixed(2)}s`;
  }
</script>

{#if project}
  <div class="mb-6 flex items-center gap-2 text-sm text-zinc-500">
    <a href="/projects" class="hover:text-zinc-300">Projects</a>
    <span>/</span>
    <span class="text-zinc-300">{project.name}</span>
  </div>

  <ProjectTabs projectId={id} active="traces" />

  {#if traces === null}
    <div class="divide-y divide-zinc-800/60">
      {#each Array(5) as _, i (i)}<div class="h-12 animate-pulse bg-zinc-900/30"></div>{/each}
    </div>
  {:else if traces.length === 0}
    <div class="rounded-lg border border-dashed border-zinc-800 px-6 py-14 text-center">
      <p class="text-sm font-medium text-zinc-300">No traces</p>
      <p class="mx-auto mt-1 max-w-md text-sm text-zinc-500">
        Send spans via OTLP to <code class="font-mono">{project.otlp_endpoint}/v1/traces</code>
        with header <code class="font-mono">x-flare-key</code>.
      </p>
    </div>
  {:else}
    <ul class="divide-y divide-zinc-800/60">
      {#each traces as t (t.trace_id)}
        <li>
          <a href="/projects/{id}/traces/{t.trace_id}" class="flex items-center gap-4 py-3 transition-colors hover:bg-zinc-900/40">
            <span class="inline-block h-2 w-2 shrink-0 rounded-full {t.has_error ? 'bg-rose-400' : 'bg-emerald-400'}"></span>
            <div class="min-w-0 flex-1">
              <div class="truncate text-sm font-medium text-zinc-100">{t.root_name || '(root)'}</div>
              <div class="mt-0.5 font-mono text-xs text-zinc-600">{t.trace_id.slice(0, 16)}</div>
            </div>
            <div class="shrink-0 text-right">
              <div class="font-mono text-sm text-zinc-300">{fmtDur(t.duration_ms)}</div>
              <div class="text-xs text-zinc-600">{t.span_count} spans &middot; {relativeTime(t.started)}</div>
            </div>
          </a>
        </li>
      {/each}
    </ul>
  {/if}
{:else if error}
  <p class="rounded-md border border-rose-900/60 bg-rose-950/40 px-3 py-2 text-sm text-rose-300">{error}</p>
{:else}
  <div class="flex min-h-[30vh] items-center justify-center text-sm text-zinc-500">Loading...</div>
{/if}
