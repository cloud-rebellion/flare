<script lang="ts">
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { relativeUnix, levelColor } from '$lib/format';
  import Sparkline from '$lib/components/Sparkline.svelte';
  import VolumeChart from '$lib/components/VolumeChart.svelte';
  import type { Overview, OverviewProject } from '$lib/types';

  let data = $state<Overview | null>(null);
  let error = $state<string | null>(null);
  let lastRefreshed = $state<number>(0);

  $effect(() => {
    if (session.loaded && !session.user) goto('/login', { replaceState: true });
  });

  onMount(() => {
    load();
    const timer = setInterval(load, 60_000);
    return () => clearInterval(timer);
  });

  async function load() {
    try {
      data = await api.overview();
      error = null;
      lastRefreshed = Math.floor(Date.now() / 1000);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load the overview';
    }
  }

  const nf = new Intl.NumberFormat();

  // Group cards by health so the panel reads as "what needs me / what is fine /
  // what is not reporting" instead of one long undifferentiated list. Attention
  // is open by default; the healthy and silent tails collapse to a count.
  let showAttention = $state(true);
  let showHealthy = $state(false);
  let showSilent = $state(false);

  const groups = $derived.by(() => {
    const attention: OverviewProject[] = [];
    const healthy: OverviewProject[] = [];
    const silent: OverviewProject[] = [];
    for (const p of data?.projects ?? []) {
      if (p.status === 'spike' || p.status === 'alert') attention.push(p);
      else if (p.status === 'silent') silent.push(p);
      else healthy.push(p);
    }
    return { attention, healthy, silent };
  });

  // Rejected intake comes first, followed by projects with no accepted
  // telemetry. This is an investigation queue, not an application health score.
  const coverageRows = $derived(
    [...(data?.projects ?? [])].sort((a, b) =>
      Number(b.intake.rejected_24h > 0) - Number(a.intake.rejected_24h > 0) ||
      Number(a.intake.accepted_24h > 0) - Number(b.intake.accepted_24h > 0) ||
      b.intake.rejected_24h - a.intake.rejected_24h ||
      a.name.localeCompare(b.name)
    )
  );

  // Dot color per health state: rose (surging) / amber (erroring) / grey
  // (no pulse) / emerald (alive and clean).
  function dotColor(s: string): string {
    switch (s) {
      case 'spike':
        return 'bg-rose-400 shadow-[0_0_8px_2px_rgba(251,113,133,0.55)]';
      case 'alert':
        return 'bg-amber-400';
      case 'silent':
        return 'bg-zinc-600';
      default:
        return 'bg-emerald-400';
    }
  }

  function sparkAccent(s: string): string {
    return s === 'spike' ? '#fb7185' : s === 'alert' ? '#fbbf24' : '#34d399';
  }

  // Second line under the project name: liveness first for silent/healthy (the
  // pulse is the answer to "is it up?"), error counts for anything erroring.
  function subline(p: OverviewProject): string {
    if (p.status === 'silent') {
      return p.last_seen_unix ? `no event pulse · last seen ${relativeUnix(p.last_seen_unix)}` : 'no event pulse observed';
    }
    if (p.status === 'healthy') {
      return p.last_seen_unix ? `event pulse ${relativeUnix(p.last_seen_unix)} · no recent errors` : 'no recent errors';
    }
    return `${nf.format(p.events_24h)} error${p.events_24h === 1 ? '' : 's'} · ${nf.format(p.unresolved)} open`;
  }
</script>

<div class="mb-8 flex flex-wrap items-end justify-between gap-4">
  <div>
    <p class="mb-1 text-xs font-semibold uppercase tracking-[0.18em] text-amber-500">Flare operations</p>
    <h1 class="text-2xl font-semibold tracking-tight">Overview</h1>
    <p class="mt-1 text-sm text-zinc-400">Incoming telemetry, gaps, and errors across your projects.</p>
  </div>
  <div class="flex items-center gap-3">
    {#if lastRefreshed}<span class="text-xs text-zinc-400">Updated at {new Date(lastRefreshed * 1000).toLocaleTimeString()}</span>{/if}
    <button type="button" onclick={load} class="rounded-md border border-zinc-700 px-3 py-1.5 text-xs text-zinc-300 transition-colors hover:bg-zinc-800">Refresh</button>
  </div>
</div>

{#if error && !data}
  <div class="rounded-lg border border-rose-900/50 bg-rose-950/20 p-5 text-sm text-rose-300">{error}</div>
{:else if !data}
  <!-- Loading: skeleton matching the real layout -->
  <div class="animate-pulse space-y-6">
    <div class="grid grid-cols-3 overflow-hidden rounded-lg border border-zinc-800/80 bg-zinc-900/40">
      {#each Array(3) as _, i (i)}
        <div class="border-r border-zinc-800/80 p-5 last:border-r-0">
          <div class="h-3 w-20 rounded bg-zinc-800"></div>
          <div class="mt-3 h-7 w-16 rounded bg-zinc-800"></div>
        </div>
      {/each}
    </div>
    <div class="h-[210px] rounded-lg border border-zinc-800/80 bg-zinc-900/40"></div>
    <div class="grid gap-6 md:grid-cols-2">
      <div class="h-64 rounded-lg border border-zinc-800/80 bg-zinc-900/40"></div>
      <div class="h-64 rounded-lg border border-zinc-800/80 bg-zinc-900/40"></div>
    </div>
  </div>
{:else}
  {#if error}
    <div class="mb-6 rounded-lg border border-rose-900/50 bg-rose-950/20 p-4 text-sm text-rose-300">Refresh failed: {error}. Showing the last loaded snapshot.</div>
  {/if}
  {#if data.alerting_blind}
    <div class="mb-6 flex items-start gap-3 rounded-lg border border-amber-500/40 bg-amber-500/10 p-4">
      <span class="mt-1 inline-block h-2 w-2 shrink-0 rounded-full bg-amber-400"></span>
      <div class="text-sm">
        <p class="font-medium text-amber-300">Alerts have nowhere to go</p>
        <p class="mt-0.5 text-amber-200/70">
          You have active alert rules but no enabled notification channel, so alerts fire into the void and
          reach no one. <a href="/settings" class="underline underline-offset-2 hover:text-amber-100">Add a
          channel</a> in settings and send a test to confirm it delivers.
        </p>
      </div>
    </div>
  {/if}
  <!-- Acceptance is the first question: how many projects actually reported? -->
  <div class="mb-6 grid grid-cols-2 overflow-hidden rounded-lg border border-zinc-800/80 bg-zinc-900/40 md:grid-cols-3 xl:grid-cols-6">
    <div class="border-b border-r border-zinc-800/80 p-5 md:border-b-0">
      <div class="text-xs font-medium uppercase tracking-wide text-zinc-400">Reporting (24h)</div>
      <div class="mt-2 font-mono text-3xl font-semibold tracking-tight text-zinc-100">{data.reporting_projects_24h}<span class="text-lg text-zinc-400"> / {data.projects.length}</span></div>
      <div class="mt-1 text-xs text-zinc-400">Projects with accepted runtime intake</div>
    </div>
    <div class="border-b border-zinc-800/80 p-5 md:border-b-0 md:border-r">
      <div class="text-xs font-medium uppercase tracking-wide text-zinc-400">Rejected intake</div>
      <div class="mt-2 font-mono text-3xl font-semibold tracking-tight {data.rejected_24h > 0 ? 'text-rose-400' : 'text-zinc-100'}">{nf.format(data.rejected_24h)}</div>
      <div class="mt-1 text-xs text-zinc-400">Items in completed receipts</div>
    </div>
    <div class="border-r border-zinc-800/80 p-5">
      <div class="text-xs font-medium uppercase tracking-wide text-zinc-400">Error events (24h)</div>
      <div class="mt-2 font-mono text-3xl font-semibold tracking-tight text-zinc-100">{nf.format(data.events_24h)}</div>
    </div>
    <div class="border-r border-zinc-800/80 p-5">
      <div class="text-xs font-medium uppercase tracking-wide text-zinc-400">Unresolved</div>
      <div class="mt-2 font-mono text-3xl font-semibold tracking-tight text-zinc-100">{nf.format(data.unresolved)}</div>
    </div>
    <div class="border-r border-zinc-800/80 p-5">
      <div class="text-xs font-medium uppercase tracking-wide text-zinc-400">New today</div>
      <div class="mt-2 font-mono text-3xl font-semibold tracking-tight {data.new_today > 0 ? 'text-amber-400' : 'text-zinc-100'}">
        {nf.format(data.new_today)}
      </div>
    </div>
    <div class="p-5">
      <div class="text-xs font-medium uppercase tracking-wide text-zinc-400">Dropped intake</div>
      <div class="mt-2 font-mono text-3xl font-semibold tracking-tight {data.dropped_24h > 0 ? 'text-amber-400' : 'text-zinc-100'}">{nf.format(data.dropped_24h)}</div>
      <div class="mt-1 text-xs text-zinc-400">Items excluded by intake</div>
    </div>
  </div>

  <section class="mb-6 overflow-hidden rounded-lg border border-zinc-800/80 bg-zinc-900/40">
    <div class="flex flex-wrap items-start justify-between gap-3 border-b border-zinc-800/80 px-5 py-4">
      <div>
        <h2 class="text-sm font-semibold text-zinc-100">Intake coverage</h2>
        <p class="mt-1 text-xs text-zinc-400">Accepted runtime signals in the last 24 hours. A dash means no accepted intake observed; it does not prove the integration was expected or failed.</p>
      </div>
      <span class="rounded-full border border-zinc-700 px-2.5 py-1 text-[11px] text-zinc-400">Completed receipts · 24h</span>
    </div>
    {#if coverageRows.length === 0}
      <div class="px-5 py-8 text-sm text-zinc-400">No projects configured yet.</div>
    {:else}
      <div class="max-h-[560px] overflow-auto">
        <table class="w-full min-w-[850px] text-left text-xs">
          <thead class="sticky top-0 bg-zinc-900 text-zinc-400">
            <tr class="border-b border-zinc-800">
              <th class="px-5 py-3 font-medium">Project</th>
              <th class="px-2 py-3 text-center font-medium">Sentry</th>
              <th class="px-2 py-3 text-center font-medium">OTLP traces</th>
              <th class="px-2 py-3 text-center font-medium">Logs</th>
              <th class="px-2 py-3 text-center font-medium">Metrics</th>
              <th class="px-2 py-3 text-center font-medium">Business</th>
              <th class="px-2 py-3 text-center font-medium">Connectors</th>
              <th class="px-4 py-3 text-right font-medium">Rejected</th>
              <th class="px-5 py-3 text-right font-medium">Last accepted</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-zinc-800/60">
            {#each coverageRows as p (p.id)}
              <tr class="transition-colors hover:bg-zinc-800/30">
                <td class="px-5 py-3">
                  <a href={`/projects/${p.id}`} class="font-medium text-zinc-200 hover:text-amber-300">{p.name}</a>
                  <div class="mt-0.5 text-[11px] text-zinc-400">{nf.format(p.intake.accepted_24h)} accepted</div>
                </td>
                {#each [p.intake.sentry, p.intake.traces, p.intake.logs, p.intake.metrics, p.intake.business, p.intake.connectors] as seen, i (i)}
                  <td class="px-2 py-3 text-center"><span class={seen ? 'font-semibold text-emerald-400' : 'text-zinc-500'} aria-label={seen ? 'accepted intake observed' : 'no accepted intake observed'}>{seen ? '●' : '-'}</span></td>
                {/each}
                <td class="px-4 py-3 text-right font-mono {p.intake.rejected_24h > 0 ? 'text-rose-400' : 'text-zinc-400'}">{nf.format(p.intake.rejected_24h)}</td>
                <td class="px-5 py-3 text-right text-zinc-400">{p.intake.last_accepted_unix ? relativeUnix(p.intake.last_accepted_unix) : 'Not observed'}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>

  <!-- Volume chart: gallery-style label outside the surface -->
  <div class="mb-6 rounded-lg border border-zinc-800/80 bg-zinc-900/40 p-5">
    <div class="mb-3 text-xs font-medium uppercase tracking-wide text-zinc-500">Error volume</div>
    <VolumeChart buckets={data.volume} />
  </div>

  <div class="grid gap-6 md:grid-cols-2">
    <!-- Top issues -->
    <section class="rounded-lg border border-zinc-800/80 bg-zinc-900/40">
      <div class="border-b border-zinc-800/80 px-5 py-3 text-xs font-medium uppercase tracking-wide text-zinc-500">
        Top issues
      </div>
      {#if data.top_issues.length === 0}
        <div class="px-5 py-10 text-center text-sm text-zinc-600">No unresolved issues in this view.</div>
      {:else}
        <ul class="divide-y divide-zinc-800/60">
          {#each data.top_issues as issue (issue.id)}
            <li>
              <a
                href={`/issues/${issue.id}`}
                class="flex items-center gap-3 px-5 py-3 transition-colors hover:bg-zinc-800/30"
              >
                <span class="text-[10px] font-medium uppercase {levelColor(issue.level)}">{issue.level}</span>
                <div class="min-w-0 flex-1">
                  <div class="truncate text-sm text-zinc-200">{issue.title}</div>
                  <div class="truncate text-xs text-zinc-600">{issue.project_name}</div>
                </div>
                <span class="font-mono text-sm text-zinc-400">{nf.format(issue.event_count)}</span>
              </a>
            </li>
          {/each}
        </ul>
      {/if}
    </section>

    <!-- Project health: grouped by pulse + error state, collapsible tails -->
    <section class="rounded-lg border border-zinc-800/80 bg-zinc-900/40">
      <div class="flex items-center justify-between gap-3 border-b border-zinc-800/80 px-5 py-3">
        <span class="text-xs font-medium uppercase tracking-wide text-zinc-500">Projects</span>
        <span class="hidden text-[11px] text-zinc-600 sm:flex sm:items-center sm:gap-3">
          <span class="flex items-center gap-1"><span class="h-1.5 w-1.5 rounded-full bg-emerald-400"></span>event pulse</span>
          <span class="flex items-center gap-1"><span class="h-1.5 w-1.5 rounded-full bg-amber-400"></span>errors</span>
          <span class="flex items-center gap-1"><span class="h-1.5 w-1.5 rounded-full bg-zinc-600"></span>no event pulse</span>
        </span>
      </div>
      {#if data.projects.length === 0}
        <div class="px-5 py-10 text-center text-sm text-zinc-600">
          No projects yet. <a href="/projects" class="text-amber-400 hover:text-amber-300">Create one</a> to start.
        </div>
      {:else}
        {#snippet projectRow(p: OverviewProject)}
          <li>
            <a
              href={`/projects/${p.id}`}
              class="flex items-center gap-3 px-5 py-2.5 transition-colors hover:bg-zinc-800/30"
            >
              <span class="h-2 w-2 shrink-0 rounded-full {dotColor(p.status)}" title={p.status}></span>
              <div class="min-w-0 flex-1">
                <div class="truncate text-sm text-zinc-200">{p.name}</div>
                <div class="truncate text-xs text-zinc-600">{subline(p)}</div>
              </div>
              {#if p.status === 'silent'}
                <span class="shrink-0 text-xs text-zinc-700" aria-hidden="true">-</span>
              {:else}
                <div class="h-7 w-20 shrink-0 text-zinc-500">
                  <Sparkline values={p.volume} accent={sparkAccent(p.status)} />
                </div>
              {/if}
            </a>
          </li>
        {/snippet}

        {#snippet groupHeader(label: string, count: number, dot: string, open: boolean, toggle: () => void, first: boolean)}
          <button
            type="button"
            onclick={toggle}
            class="flex w-full items-center gap-2.5 px-5 py-2.5 text-left transition-colors hover:bg-zinc-800/20 {first
              ? ''
              : 'border-t border-zinc-800/60'}"
          >
            <span class="h-2 w-2 shrink-0 rounded-full {dot}"></span>
            <span class="text-xs font-medium uppercase tracking-wide text-zinc-300">{label}</span>
            <span class="rounded-full bg-zinc-800 px-1.5 text-[11px] text-zinc-400">{count}</span>
            <svg
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linecap="round"
              stroke-linejoin="round"
              class="ml-auto h-3.5 w-3.5 text-zinc-600 transition-transform {open ? 'rotate-90' : ''}"
              aria-hidden="true"
            >
              <path d="M9 6l6 6-6 6" />
            </svg>
          </button>
        {/snippet}

        <!-- Needs attention: alive but erroring or spiking. Open by default. -->
        {#if groups.attention.length}
          {@render groupHeader('Needs attention', groups.attention.length, 'bg-rose-400', showAttention, () => (showAttention = !showAttention), true)}
          {#if showAttention}
            <ul class="divide-y divide-zinc-800/60 border-t border-zinc-800/40">
              {#each groups.attention as p (p.id)}{@render projectRow(p)}{/each}
            </ul>
          {/if}
        {:else}
          <div class="flex items-center gap-2 px-5 py-2.5 text-xs text-zinc-600">
            <span class="h-2 w-2 shrink-0 rounded-full bg-emerald-400/70"></span>
            Nothing needs attention.
          </div>
        {/if}

        <!-- Recent event pulse and no recent errors. -->
        {#if groups.healthy.length}
          {@render groupHeader('No recent errors', groups.healthy.length, 'bg-emerald-400', showHealthy, () => (showHealthy = !showHealthy), false)}
          {#if showHealthy}
            <ul class="divide-y divide-zinc-800/60 border-t border-zinc-800/40">
              {#each groups.healthy as p (p.id)}{@render projectRow(p)}{/each}
            </ul>
          {/if}
        {/if}

        <!-- No event pulse: may still have accepted logs, traces, or metrics. -->
        {#if groups.silent.length}
          {@render groupHeader('No event pulse', groups.silent.length, 'bg-zinc-600', showSilent, () => (showSilent = !showSilent), false)}
          {#if showSilent}
            <ul class="divide-y divide-zinc-800/60 border-t border-zinc-800/40">
              {#each groups.silent as p (p.id)}{@render projectRow(p)}{/each}
            </ul>
          {/if}
        {/if}
      {/if}
    </section>
  </div>
{/if}
