<script lang="ts">
  import { onMount } from 'svelte';
  import ProjectTabs from '$lib/components/ProjectTabs.svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { relativeTime } from '$lib/format';
  import type { Project, Release } from '$lib/types';

  const id = $derived(page.params.id ?? '');

  let project = $state<Project | null>(null);
  let releases = $state<Release[] | null>(null);
  let error = $state<string | null>(null);
  let version = $state('');
  let busy = $state(false);

  const canWrite = $derived(session.user?.role !== 'viewer');

  $effect(() => {
    if (session.loaded && !session.user) goto('/login', { replaceState: true });
  });

  onMount(async () => {
    try {
      project = await api.project(id);
      releases = await api.releases(id);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load releases';
      releases = [];
    }
  });

  async function markRelease(e: SubmitEvent) {
    e.preventDefault();
    if (!version.trim()) return;
    busy = true;
    error = null;
    try {
      const rel = await api.createRelease(id, version.trim());
      releases = [rel, ...(releases ?? []).filter((r) => r.version !== rel.version)];
      version = '';
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to mark release';
    } finally {
      busy = false;
    }
  }
</script>

{#if project}
  <div class="mb-6 flex items-center gap-2 text-sm text-zinc-500">
    <a href="/projects" class="hover:text-zinc-300">Projects</a>
    <span>/</span>
    <span class="text-zinc-300">{project.name}</span>
  </div>

  <ProjectTabs projectId={id} active="releases" />

  <p class="mb-5 max-w-2xl text-sm text-zinc-500">
    Releases are tracked automatically from the <code class="font-mono text-zinc-400">release</code> your SDK reports. Mark a deploy here (or from CI) to record its time even before any errors arrive.
  </p>

  {#if canWrite}
    <form onsubmit={markRelease} class="mb-8 flex flex-wrap items-end gap-3 border-b border-zinc-800/80 pb-8">
      <div class="flex flex-col gap-1.5">
        <label for="ver" class="text-xs font-medium text-zinc-400">Mark a deploy</label>
        <input
          id="ver"
          bind:value={version}
          placeholder="1.4.2"
          class="w-40 rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
        />
      </div>
      <button
        type="submit"
        disabled={busy}
        class="rounded-md bg-amber-400 px-3.5 py-2 text-sm font-medium text-zinc-950 transition-colors hover:bg-amber-300 active:translate-y-px disabled:opacity-60"
      >
        {busy ? 'Marking...' : 'Mark release'}
      </button>
    </form>
  {/if}

  {#if error}
    <p class="mb-6 rounded-md border border-rose-900/60 bg-rose-950/40 px-3 py-2 text-sm text-rose-300">{error}</p>
  {/if}

  {#if releases === null}
    <div class="space-y-2">
      {#each Array(3) as _, i (i)}
        <div class="h-12 animate-pulse rounded-md border border-zinc-800/60 bg-zinc-900/40"></div>
      {/each}
    </div>
  {:else if releases.length === 0}
    <div class="rounded-lg border border-dashed border-zinc-800 px-6 py-14 text-center">
      <p class="text-sm font-medium text-zinc-300">No releases yet</p>
      <p class="mx-auto mt-1 max-w-sm text-sm text-zinc-500">Send an event with a <code class="font-mono">release</code> set, or mark a deploy above.</p>
    </div>
  {:else}
    <ul class="divide-y divide-zinc-800/60">
      {#each releases as r (r.version)}
        <li class="flex items-center gap-3 py-3">
          <span class="font-mono text-sm text-zinc-100">{r.version}</span>
          <span class="text-xs text-zinc-600">{relativeTime(r.created_at)}</span>
          <span class="ml-auto font-mono text-sm {r.new_issues > 0 ? 'text-amber-300' : 'text-zinc-600'}">
            {r.new_issues} new {r.new_issues === 1 ? 'issue' : 'issues'}
          </span>
        </li>
      {/each}
    </ul>
  {/if}
{:else if error}
  <p class="rounded-md border border-rose-900/60 bg-rose-950/40 px-3 py-2 text-sm text-rose-300">{error}</p>
{:else}
  <div class="flex min-h-[30vh] items-center justify-center text-sm text-zinc-500">Loading...</div>
{/if}
