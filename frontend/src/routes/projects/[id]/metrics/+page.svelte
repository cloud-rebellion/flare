<script lang="ts">
  import { onMount } from 'svelte';
  import ProjectTabs from '$lib/components/ProjectTabs.svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { relativeTime } from '$lib/format';
  import type { MetricName, MetricPoint, Project } from '$lib/types';

  const id = $derived(page.params.id ?? '');

  let project = $state<Project | null>(null);
  let names = $state<MetricName[] | null>(null);
  let error = $state<string | null>(null);
  let selected = $state<string | null>(null);
  let points = $state<MetricPoint[]>([]);
  let windowMin = $state(60);
  let loadingSeries = $state(false);

  $effect(() => {
    if (session.loaded && !session.user) goto('/login', { replaceState: true });
  });

  onMount(async () => {
    try {
      project = await api.project(id);
      names = await api.metrics(id);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load metrics';
      names = [];
    }
  });

  async function select(name: string) {
    selected = name;
    loadingSeries = true;
    try {
      points = await api.metricSeries(id, name, windowMin);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load series';
      points = [];
    } finally {
      loadingSeries = false;
    }
  }

  function changeWindow(w: number) {
    windowMin = w;
    if (selected) select(selected);
  }

  // One metric NAME can carry many independent series, one per label set
  // (http_requests{route=/a} and {route=/b} are different things). Drawing all
  // their points as a single line produced a sawtooth that belonged to no
  // series, and min/max/avg/latest computed over the mixture were meaningless.
  // Split by label set and chart each one.
  function labelKey(labels: unknown): string {
    if (!labels || typeof labels !== 'object') return '';
    const entries = Object.entries(labels as Record<string, unknown>).sort(([a], [b]) =>
      a.localeCompare(b)
    );
    return entries.map(([k, v]) => `${k}=${String(v)}`).join(', ');
  }

  // Muted, distinguishable, and consistent with the dashboard's amber accent.
  const seriesColors = ['#fbbf24', '#38bdf8', '#34d399', '#f472b6', '#a3a3a3', '#fb923c'];

  const series = $derived.by(() => {
    const groups = new Map<string, MetricPoint[]>();
    for (const p of points) {
      const k = labelKey(p.labels);
      const g = groups.get(k);
      if (g) g.push(p);
      else groups.set(k, [p]);
    }
    // Global y-scale so the series are visually comparable.
    const all = points.map((p) => p.value);
    const gMin = all.length ? Math.min(...all) : 0;
    const gMax = all.length ? Math.max(...all) : 1;
    const span = gMax - gMin || 1;

    return [...groups.entries()].map(([key, pts], idx) => {
      const vals = pts.map((p) => p.value);
      const n = pts.length;
      const path =
        n < 2
          ? ''
          : pts
              .map((p, i) => {
                const x = (i / (n - 1)) * 98 + 1;
                const y = 29 - ((p.value - gMin) / span) * 28;
                return `${i === 0 ? 'M' : 'L'}${x.toFixed(2)},${y.toFixed(2)}`;
              })
              .join(' ');
      return {
        key,
        label: key || 'no labels',
        color: seriesColors[idx % seriesColors.length],
        path,
        min: Math.min(...vals),
        max: Math.max(...vals),
        avg: vals.reduce((a, b) => a + b, 0) / vals.length,
        latest: vals[vals.length - 1],
        count: n
      };
    });
  });

  // Summary stats stay honest: for a single series they describe it; with more
  // than one, the per-series legend below is the truth.
  const stats = $derived(series.length === 1 ? series[0] : null);

  function fmt(n: number): string {
    if (Math.abs(n) >= 1000 || (n !== 0 && Math.abs(n) < 0.01)) return n.toExponential(2);
    return Number.isInteger(n) ? String(n) : n.toFixed(2);
  }
</script>

{#if project}
  <div class="mb-6 flex items-center gap-2 text-sm text-zinc-500">
    <a href="/projects" class="hover:text-zinc-300">Projects</a>
    <span>/</span>
    <span class="text-zinc-300">{project.name}</span>
  </div>

  <ProjectTabs projectId={id} active="metrics" />

  {#if names === null}
    <div class="divide-y divide-zinc-800/60">
      {#each Array(5) as _, i (i)}<div class="h-10 animate-pulse bg-zinc-900/30"></div>{/each}
    </div>
  {:else if names.length === 0}
    <div class="rounded-lg border border-dashed border-zinc-800 px-6 py-14 text-center">
      <p class="text-sm font-medium text-zinc-300">No metrics</p>
      <p class="mx-auto mt-1 max-w-md text-sm text-zinc-500">
        Send metrics via OTLP to <code class="font-mono">{project.otlp_endpoint}/v1/metrics</code>, or POST a JSON
        array of <code class="font-mono">{'{name, value, kind, labels}'}</code> to the native
        <code class="font-mono">/metrics</code> endpoint with header <code class="font-mono">x-flare-key</code>.
      </p>
    </div>
  {:else}
    <div class="grid gap-6 md:grid-cols-[minmax(0,16rem)_1fr]">
      <ul class="divide-y divide-zinc-800/60 self-start rounded-lg border border-zinc-800/80">
        {#each names as m (m.name)}
          <li>
            <button
              onclick={() => select(m.name)}
              class="flex w-full items-center gap-2 px-3 py-2.5 text-left transition-colors hover:bg-zinc-900/40 {selected === m.name ? 'bg-zinc-900/60' : ''}"
            >
              <div class="min-w-0 flex-1">
                <div class="truncate font-mono text-[13px] text-zinc-200">{m.name}</div>
                <div class="text-[11px] text-zinc-600">{m.kind} · {m.points} pts · {relativeTime(m.last_seen)}</div>
              </div>
            </button>
          </li>
        {/each}
      </ul>

      <div>
        {#if !selected}
          <div class="flex min-h-[16rem] items-center justify-center rounded-lg border border-dashed border-zinc-800 text-sm text-zinc-500">
            Select a metric to chart it.
          </div>
        {:else}
          <div class="rounded-lg border border-zinc-800/80 bg-zinc-900/40 p-5">
            <div class="mb-3 flex flex-wrap items-center justify-between gap-3">
              <h2 class="font-mono text-sm text-zinc-200">{selected}</h2>
              <div class="flex items-center gap-1 text-xs">
                {#each [{ l: '1h', w: 60 }, { l: '6h', w: 360 }, { l: '24h', w: 1440 }] as opt (opt.w)}
                  <button
                    onclick={() => changeWindow(opt.w)}
                    class="rounded px-2 py-1 {windowMin === opt.w ? 'bg-zinc-800 text-zinc-100' : 'text-zinc-500 hover:text-zinc-300'}"
                  >
                    {opt.l}
                  </button>
                {/each}
              </div>
            </div>

            {#if loadingSeries}
              <div class="h-40 animate-pulse rounded bg-zinc-900/40"></div>
            {:else if series.length === 0}
              <p class="py-12 text-center text-sm text-zinc-500">No points in this window.</p>
            {:else}
              <svg viewBox="0 0 100 30" preserveAspectRatio="none" class="h-40 w-full">
                {#each series as s (s.key)}
                  <path d={s.path} fill="none" stroke={s.color} stroke-width="0.5" vector-effect="non-scaling-stroke" />
                {/each}
              </svg>

              {#if stats}
                <div class="mt-4 grid grid-cols-2 gap-3 sm:grid-cols-5">
                  {#each [['latest', stats.latest], ['min', stats.min], ['max', stats.max], ['avg', stats.avg], ['points', stats.count]] as [label, value] (label)}
                    <div class="rounded-md border border-zinc-800/80 px-3 py-2">
                      <div class="text-[10px] uppercase tracking-wide text-zinc-500">{label}</div>
                      <div class="mt-0.5 font-mono text-sm text-zinc-200">{label === 'points' ? value : fmt(value as number)}</div>
                    </div>
                  {/each}
                </div>
              {:else}
                <!-- More than one label set: a single summary would average
                     across unrelated series, so break it out per series. -->
                <div class="mt-4 overflow-x-auto">
                  <table class="w-full min-w-[32rem] text-left text-xs">
                    <thead class="text-[10px] uppercase tracking-wide text-zinc-500">
                      <tr class="border-b border-zinc-800/80">
                        <th class="py-1.5 pr-3 font-medium">Series</th>
                        <th class="py-1.5 pr-3 text-right font-medium">Latest</th>
                        <th class="py-1.5 pr-3 text-right font-medium">Min</th>
                        <th class="py-1.5 pr-3 text-right font-medium">Max</th>
                        <th class="py-1.5 pr-3 text-right font-medium">Avg</th>
                        <th class="py-1.5 text-right font-medium">Points</th>
                      </tr>
                    </thead>
                    <tbody class="divide-y divide-zinc-800/40">
                      {#each series as s (s.key)}
                        <tr>
                          <td class="py-1.5 pr-3">
                            <span class="inline-flex items-center gap-2">
                              <span class="inline-block h-2 w-2 shrink-0 rounded-full" style="background-color: {s.color}"></span>
                              <span class="truncate font-mono text-zinc-300">{s.label}</span>
                            </span>
                          </td>
                          <td class="py-1.5 pr-3 text-right font-mono text-zinc-200">{fmt(s.latest)}</td>
                          <td class="py-1.5 pr-3 text-right font-mono text-zinc-400">{fmt(s.min)}</td>
                          <td class="py-1.5 pr-3 text-right font-mono text-zinc-400">{fmt(s.max)}</td>
                          <td class="py-1.5 pr-3 text-right font-mono text-zinc-400">{fmt(s.avg)}</td>
                          <td class="py-1.5 text-right font-mono text-zinc-500">{s.count}</td>
                        </tr>
                      {/each}
                    </tbody>
                  </table>
                </div>
              {/if}
            {/if}
          </div>
        {/if}
      </div>
    </div>
  {/if}
{:else if error}
  <p class="rounded-md border border-rose-900/60 bg-rose-950/40 px-3 py-2 text-sm text-rose-300">{error}</p>
{:else}
  <div class="flex min-h-[30vh] items-center justify-center text-sm text-zinc-500">Loading...</div>
{/if}
