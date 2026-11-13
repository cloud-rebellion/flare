<script lang="ts">
  // Instance-wide data retention. Only admins of the operator workspace can
  // read or change it; anyone else gets a 403, shown as a short explanation.
  import { onMount } from 'svelte';
  import { api, ApiError } from '$lib/api';
  import { relativeTime } from '$lib/format';
  import {
    describePolicy,
    formatBytes,
    previewRemoval,
    reduces,
    samePolicy,
    toPolicy,
    validatePolicy
  } from '$lib/retention';
  import { submitRetention } from '$lib/retention-save';
  import type { RetentionColdMode, RetentionSettings } from '$lib/types';

  let settings = $state<RetentionSettings | null>(null);
  let loadError = $state<string | null>(null);
  let forbidden = $state(false);

  let hotDays = $state(30);
  let coldMode = $state<RetentionColdMode>('forever');
  let coldDays = $state(90);

  let busy = $state(false);
  let confirming = $state<'save' | 'reset' | null>(null);
  let saveError = $state<string | null>(null);
  let saved = $state<string | null>(null);

  const next = $derived(toPolicy(Number(hotDays), coldMode, Number(coldDays)));
  const invalid = $derived(settings ? validatePolicy(next, settings.bounds) : null);
  const changed = $derived(settings ? !samePolicy(next, settings.effective) : false);
  const lowers = $derived(settings ? reduces(settings.effective, next) : false);
  const preview = $derived(settings && !invalid ? previewRemoval(settings, next) : null);
  const resetPreview = $derived(settings ? previewRemoval(settings, settings.env_default) : null);
  const fmtRows = (n: number) => n.toLocaleString('en-US');

  const saveDeps = { load: api.retention, setRetention: api.setRetention, resetRetention: api.resetRetention };

  onMount(load);

  function adopt(s: RetentionSettings) {
    settings = s;
    hotDays = s.effective.hot_retention_days;
    coldMode = s.effective.cold_mode;
    coldDays = s.effective.cold_retention_days ?? Math.max(90, s.effective.hot_retention_days);
  }

  async function load() {
    loadError = null;
    try {
      adopt(await api.retention());
    } catch (err) {
      if (err instanceof ApiError && err.status === 403) forbidden = true;
      else loadError = err instanceof ApiError ? err.message : 'Failed to load retention settings';
    }
  }

  // The click logic lives in retention-save.ts (submitRetention), tested
  // there; this only renders its result. A confirm keeps what the admin
  // typed and swaps in the freshly loaded view, so the preview and the
  // confirm text are computed from current data.
  async function submit(action: 'save' | 'reset') {
    if (!settings) return;
    saveError = null;
    saved = null;
    busy = true;
    try {
      const r = await submitRetention(saveDeps, { settings, confirming }, action, action === 'save' ? next : undefined);
      if (r.adopt) adopt(r.settings);
      else settings = r.settings;
      confirming = r.confirming;
      saveError = r.error;
      saved = r.saved;
    } finally {
      busy = false;
    }
  }

  async function save(e?: Event) {
    e?.preventDefault();
    if (!settings || invalid || !changed) return;
    await submit('save');
  }

  async function reset() {
    if (!settings || settings.source !== 'stored') return;
    await submit('reset');
  }

  function edited() {
    confirming = null;
    saved = null;
  }
</script>

<h2 class="mt-14 text-xl font-semibold tracking-tight">Data retention</h2>
<p class="mt-1 mb-4 max-w-[65ch] text-sm text-zinc-500">
  How long errors, logs, traces and metrics are kept. Each day lives in Postgres first, then is archived to
  Parquet (unless you turn the archive off) and dropped. This applies to every workspace on this instance.
</p>

{#if forbidden}
  <p class="text-sm text-zinc-500">
    Retention is instance-wide, so it is managed by admins of the operator workspace.
  </p>
{:else if loadError}
  <p role="alert" class="text-sm text-rose-400">
    {loadError}
    <button onclick={load} class="ml-2 text-xs text-zinc-400 underline hover:text-zinc-200">Retry</button>
  </p>
{:else if !settings}
  <div class="space-y-2" aria-hidden="true">
    <div class="h-12 animate-pulse rounded-md bg-zinc-900/60"></div>
    <div class="h-24 animate-pulse rounded-md bg-zinc-900/40"></div>
  </div>
{:else}
  <div class="mb-4 rounded-md border border-zinc-800/80 bg-zinc-900/40 px-4 py-3 text-sm">
    <div class="flex flex-wrap items-center gap-x-3 gap-y-1">
      <span class="text-zinc-200">{describePolicy(settings.effective)}</span>
      <span
        class="rounded border px-1.5 py-0.5 text-[11px] font-medium {settings.source === 'stored'
          ? 'border-amber-400/40 text-amber-300'
          : 'border-zinc-700 text-zinc-400'}"
      >
        {settings.source === 'stored' ? 'Saved in settings' : 'Environment default'}
      </span>
    </div>
    <div class="mt-1.5 space-y-0.5 text-xs text-zinc-500">
      {#if settings.source === 'stored'}
        <p>
          Changed {settings.updated_at ? relativeTime(settings.updated_at) : ''}{settings.updated_by ? ` by ${settings.updated_by}` : ''}.
          Environment default: {describePolicy(settings.env_default)}.
        </p>
      {:else}
        <p>From <span class="font-mono">RETENTION_DAYS</span> / <span class="font-mono">COLD_RETENTION_DAYS</span>. Saving here overrides them.</p>
      {/if}
      {#if settings.last_pass}
        <p>
          Last retention pass {relativeTime(settings.last_pass.last_pass_at)}:
          {#if settings.last_pass.policy_error}
            <span class="text-rose-400">could not read the policy, nothing was dropped.</span>
          {:else}
            <span class="font-mono">{settings.last_pass.hot_partitions_dropped}</span> partitions dropped{#if settings.last_pass.cold_pruned.days}, <span class="font-mono">{settings.last_pass.cold_pruned.days}</span> archive days pruned{/if}{#if settings.last_pass.cold_prune_error}, <span class="text-rose-400">archive pruning failed (see server logs)</span>{/if}.
          {/if}
          Passes run at startup and every 6 hours.
        </p>
      {/if}
    </div>
  </div>

  {#if settings.hot_paused_reason}
    <p class="mb-4 rounded-md border border-amber-400/30 bg-amber-400/10 px-3 py-2 text-sm text-amber-300">{settings.hot_paused_reason}</p>
  {/if}
  {#if settings.cold_tier.note}
    <p class="mb-4 text-sm text-zinc-400">{settings.cold_tier.note}</p>
  {/if}

  <form onsubmit={save} class="space-y-4">
    <div class="flex flex-col gap-1.5">
      <label for="hotdays" class="text-xs font-medium text-zinc-400">Keep in Postgres (days)</label>
      <input
        id="hotdays"
        type="number"
        min={settings.bounds.hot_min}
        max={settings.bounds.hot_max}
        bind:value={hotDays}
        oninput={edited}
        class="w-28 rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 font-mono text-sm outline-none focus:border-amber-400/60"
      />
      <span class="text-xs text-zinc-600">
        {settings.bounds.hot_min} to {settings.bounds.hot_max}. The minimum matches the 7 days of late events ingest accepts.
      </span>
    </div>

    <fieldset class="space-y-2">
      <legend class="mb-1.5 text-xs font-medium text-zinc-400">After that, the Parquet archive</legend>
      <label class="flex items-center gap-2 text-sm text-zinc-300">
        <input type="radio" name="coldmode" value="forever" bind:group={coldMode} onchange={edited} class="accent-amber-400" />
        Archive and keep forever
      </label>
      <label class="flex flex-wrap items-center gap-2 text-sm text-zinc-300">
        <input type="radio" name="coldmode" value="days" bind:group={coldMode} onchange={edited} class="accent-amber-400" />
        Archive and keep for
        <input
          type="number"
          aria-label="Archive retention in days"
          min={Number(hotDays) || settings.bounds.hot_min}
          max={settings.bounds.cold_max}
          bind:value={coldDays}
          oninput={edited}
          disabled={coldMode !== 'days'}
          class="w-24 rounded-md border border-zinc-800 bg-zinc-900 px-2.5 py-1.5 font-mono text-sm text-zinc-200 outline-none focus:border-amber-400/60 disabled:opacity-40"
        />
        days in total
      </label>
      <label class="flex items-center gap-2 text-sm text-zinc-300">
        <input type="radio" name="coldmode" value="off" bind:group={coldMode} onchange={edited} class="accent-amber-400" />
        Do not archive: delete aged days outright
      </label>
      {#if coldMode === 'off'}
        <p class="ml-6 text-xs text-amber-300/90">
          Aged days are dropped without a Parquet copy and cannot be recovered. Days already in the archive are left where they are.
        </p>
      {:else if coldMode === 'days' && !settings.cold_tier.prune_supported}
        <p class="ml-6 text-xs text-amber-300/90">Flare cannot prune this archive, so this limit is not enforced here.</p>
      {/if}
    </fieldset>

    {#if invalid}
      <p role="alert" class="text-sm text-rose-400">{invalid}</p>
    {:else if changed && preview}
      <div class="rounded-md border border-zinc-800/80 px-4 py-3 text-sm text-zinc-300">
        <p class="mb-1 text-xs font-medium text-zinc-400">On the next retention pass (approximate)</p>
        {#if preview.hotPaused}
          <p class="text-zinc-400">Nothing leaves Postgres: the archive is down, so aged days are kept until it is back.</p>
        {:else if preview.hotDays === 0 && preview.coldDays === 0}
          <p class="text-zinc-400">Nothing currently stored would be removed.</p>
        {:else}
          <ul class="list-inside list-disc space-y-0.5">
            {#if preview.hotDays}
              <li>
                <span class="font-mono">{preview.hotDays}</span> days (<span class="font-mono">~{fmtRows(preview.hotRows)}</span> rows,
                <span class="font-mono">{formatBytes(preview.hotBytes)}</span>) leave Postgres
                {#if preview.archivedRows && preview.deletedRows}
                  : <span class="font-mono">~{fmtRows(preview.archivedRows)}</span> rows archived, <span class="font-mono text-rose-300">~{fmtRows(preview.deletedRows)}</span> deleted
                {:else if preview.deletedRows}
                  and are <span class="text-rose-300">deleted</span>
                {:else}
                  into the archive
                {/if}
              </li>
            {/if}
            {#if preview.coldDays}
              <li>
                <span class="font-mono">{preview.coldDays}</span> archive days (<span class="font-mono">{preview.coldFiles}</span> files,
                <span class="font-mono">{formatBytes(preview.coldBytes)}</span>) are <span class="text-rose-300">deleted</span>
              </li>
            {/if}
          </ul>
        {/if}
        {#if preview.coldPruneUnavailable}
          <p class="mt-1 text-xs text-zinc-500">Archive size is not counted: Flare cannot prune this archive.</p>
        {/if}
      </div>
    {/if}

    {#if confirming === 'save' && preview}
      <div role="alertdialog" aria-labelledby="ret-confirm" class="rounded-md border border-rose-500/30 bg-rose-500/10 px-4 py-3 text-sm">
        <p id="ret-confirm" class="text-rose-300">
          This keeps less data than today: {describePolicy(next)}. What it removes is gone for good.
        </p>
        <div class="mt-3 flex items-center gap-3">
          <button type="submit" disabled={busy} class="rounded-md bg-rose-500 px-3.5 py-2 text-sm font-medium text-zinc-950 transition-colors hover:bg-rose-400 active:translate-y-px disabled:opacity-60">
            {busy ? 'Saving...' : 'Yes, lower retention'}
          </button>
          <button type="button" onclick={() => (confirming = null)} class="text-xs text-zinc-400 hover:text-zinc-200">Cancel</button>
        </div>
      </div>
    {:else if confirming === 'reset' && resetPreview}
      <div role="alertdialog" aria-labelledby="ret-reset-confirm" class="rounded-md border border-rose-500/30 bg-rose-500/10 px-4 py-3 text-sm">
        <p id="ret-reset-confirm" class="text-rose-300">
          The environment default keeps less: {describePolicy(settings.env_default)}. About
          <span class="font-mono">~{fmtRows(resetPreview.deletedRows + resetPreview.archivedRows)}</span> rows leave Postgres and
          <span class="font-mono">{resetPreview.coldDays}</span> archive days are deleted on the next pass.
        </p>
        <div class="mt-3 flex items-center gap-3">
          <button type="button" onclick={reset} disabled={busy} class="rounded-md bg-rose-500 px-3.5 py-2 text-sm font-medium text-zinc-950 transition-colors hover:bg-rose-400 active:translate-y-px disabled:opacity-60">
            {busy ? 'Resetting...' : 'Yes, reset'}
          </button>
          <button type="button" onclick={() => (confirming = null)} class="text-xs text-zinc-400 hover:text-zinc-200">Cancel</button>
        </div>
      </div>
    {:else}
      <div class="flex items-center gap-3">
        <button
          type="submit"
          disabled={busy || !!invalid || !changed}
          class="rounded-md bg-amber-400 px-3.5 py-2 text-sm font-medium text-zinc-950 transition-colors hover:bg-amber-300 active:translate-y-px disabled:opacity-60"
        >
          {busy ? 'Saving...' : lowers ? 'Review and lower retention' : 'Save retention'}
        </button>
        {#if settings.source === 'stored'}
          <button type="button" onclick={reset} disabled={busy} class="text-xs text-zinc-500 transition-colors hover:text-zinc-200">
            Reset to environment default
          </button>
        {/if}
      </div>
    {/if}

    {#if saveError}
      <p role="alert" class="text-sm text-rose-400">{saveError}</p>
    {/if}
    {#if saved}
      <p role="status" class="text-sm text-emerald-300">{saved}</p>
    {/if}
  </form>
{/if}
