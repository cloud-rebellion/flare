<script lang="ts">
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { relativeTime, levelColor, statusColor } from '$lib/format';
  import type { Issue, IssueEvent } from '$lib/types';

  const id = $derived(page.params.id ?? '');

  let issue = $state<Issue | null>(null);
  let events = $state<IssueEvent[]>([]);
  let error = $state<string | null>(null);
  let eventsLoading = $state(true);
  let eventsError = $state<string | null>(null);
  let selectedEventIndex = $state(0);

  // The API returns at most 50 occurrences, newest first. Keep the index in
  // the fetched set so the stack and trace link always describe the same event.
  const selectedEvent = $derived(events[selectedEventIndex] ?? null);

  function receivedAtLabel(value: string): string {
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
  }

  $effect(() => {
    if (session.loaded && !session.user) goto('/login', { replaceState: true });
  });

  onMount(async () => {
    try {
      issue = await api.issue(id);
      // Stored triage was already persisted by auto-triage or an earlier run.
      // Without this it was fetched and thrown away, so the panel only ever
      // appeared right after someone pressed the button, and viewers (who
      // cannot run triage at all) could never see it.
      triage = issue.ai_triage ?? '';
      try {
        events = await api.issueEvents(id);
      } catch (err) {
        eventsError = err instanceof ApiError ? err.message : 'Failed to load occurrences';
      }
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to load issue';
    } finally {
      eventsLoading = false;
    }
  });

  async function setStatus(s: string) {
    try {
      issue = await api.setIssueStatus(id, s);
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to update status';
    }
  }

  let ghBusy = $state(false);
  const canWrite = $derived(session.user?.role !== 'viewer');

  let triage = $state('');
  let triaging = $state(false);
  let triageError = $state<string | null>(null);

  async function runTriage(refresh = false) {
    triaging = true;
    triageError = null;
    try {
      const r = await api.triageIssue(id, refresh);
      triage = r.triage;
    } catch (err) {
      // Show the failure next to the button. The common case is "not
      // configured" (no model endpoint set yet), which needs a Settings link.
      triageError = err instanceof ApiError ? err.message : 'AI triage failed';
    } finally {
      triaging = false;
    }
  }

  // Safe minimal markdown: split each line on ** to toggle bold. No raw HTML.
  function segments(line: string) {
    return line.split('**').map((text, i) => ({ text, bold: i % 2 === 1 }));
  }

  async function makeGithubIssue() {
    ghBusy = true;
    error = null;
    try {
      const r = await api.createGithubIssue(id);
      if (issue) issue = { ...issue, github_url: r.github_url };
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Failed to create GitHub issue';
    } finally {
      ghBusy = false;
    }
  }
</script>

{#if issue}
  <div class="mb-6 flex items-center gap-2 text-sm text-zinc-500">
    <a href="/projects" class="hover:text-zinc-300">Projects</a>
    <span>/</span>
    <span class="text-zinc-300">Issue</span>
  </div>

  <div class="flex flex-wrap items-start justify-between gap-4">
    <div class="min-w-0">
      <div class="flex items-center gap-2.5">
        <span class="text-xs font-semibold uppercase {levelColor(issue.level)}">{issue.level}</span>
        <span class="rounded-full border px-2 py-0.5 text-xs capitalize {statusColor(issue.status)}">{issue.status}</span>
        {#if issue.sensitive}
          <span class="rounded-full border border-rose-800/70 bg-rose-950/40 px-2 py-0.5 text-xs font-medium text-rose-300">Sensitive data</span>
        {/if}
      </div>
      {#if issue.sensitive}
        <p class="mt-2 rounded-md border border-rose-900/50 bg-rose-950/20 px-3 py-2 text-xs text-rose-300/90">
          This issue's telemetry contained values that should not be logged ({issue.sensitive}). Rotate any exposed
          secret and scrub it at the source; the raw payload is stored until retention expires.
        </p>
      {/if}
      <h1 class="mt-2 text-lg font-semibold tracking-tight break-words">{issue.title}</h1>
      {#if issue.culprit}<p class="mt-1 font-mono text-sm text-zinc-500">{issue.culprit}</p>{/if}
      {#if issue.first_release}
        <p class="mt-1 text-xs text-zinc-500">first seen in <span class="font-mono text-zinc-400">{issue.first_release}</span></p>
      {/if}
    </div>
    <div class="flex flex-wrap gap-2">
      <a href={`/projects/${encodeURIComponent(issue.project_id)}/automation?issue_id=${encodeURIComponent(issue.id)}`} class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm text-zinc-300 transition-colors hover:bg-zinc-800">Automation history</a>
      {#if issue.github_url}
        <a href={issue.github_url} target="_blank" rel="noopener" class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm text-zinc-300 transition-colors hover:bg-zinc-800 active:translate-y-px">GitHub issue &#8599;</a>
      {:else if canWrite}
        <button onclick={makeGithubIssue} disabled={ghBusy} class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm text-zinc-300 transition-colors hover:bg-zinc-800 active:translate-y-px disabled:opacity-60">{ghBusy ? 'Creating...' : 'Create GitHub issue'}</button>
      {/if}
      <!-- Status changes are member+ on the server. A viewer used to see all
           three buttons and get a 403 on every click, with no explanation. -->
      {#if canWrite}
        {#if issue.status !== 'resolved'}
          <button onclick={() => setStatus('resolved')} class="rounded-md border border-emerald-500/40 px-3 py-1.5 text-sm text-emerald-300 transition-colors hover:bg-emerald-500/10 active:translate-y-px">Resolve</button>
        {/if}
        {#if issue.status !== 'ignored'}
          <button onclick={() => setStatus('ignored')} class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm text-zinc-300 transition-colors hover:bg-zinc-800 active:translate-y-px">Ignore</button>
        {/if}
        {#if issue.status !== 'unresolved'}
          <button onclick={() => setStatus('unresolved')} class="rounded-md border border-amber-400/40 px-3 py-1.5 text-sm text-amber-300 transition-colors hover:bg-amber-400/10 active:translate-y-px">Reopen</button>
        {/if}
      {:else}
        <span class="self-center text-xs text-zinc-600">Read-only access</span>
      {/if}
    </div>
  </div>

  <div class="mt-6 grid grid-cols-2 gap-px overflow-hidden rounded-lg border border-zinc-800/80 bg-zinc-800/40 sm:grid-cols-4">
    {#each [['Events', String(issue.event_count)], ['Status', issue.status], ['First seen', relativeTime(issue.first_seen)], ['Last seen', relativeTime(issue.last_seen)]] as [label, value] (label)}
      <div class="bg-zinc-950 px-4 py-3">
        <div class="text-xs text-zinc-500">{label}</div>
        <div class="mt-0.5 font-mono text-sm text-zinc-200 capitalize">{value}</div>
      </div>
    {/each}
  </div>

  {#if triage}
    <div class="mt-6 rounded-lg border border-amber-400/25 bg-amber-400/[0.03] p-4">
      <div class="mb-2 flex items-center gap-2">
        <span class="text-xs font-semibold uppercase tracking-wide text-amber-300">AI triage</span>
        <span class="text-[10px] text-zinc-600">PII-scrubbed, your endpoint</span>
        {#if canWrite}
          <button onclick={() => runTriage(true)} disabled={triaging} class="ml-auto text-xs text-zinc-500 transition-colors hover:text-zinc-300 disabled:opacity-60">{triaging ? 'Running...' : 'Re-run'}</button>
        {/if}
      </div>
      <div class="space-y-1.5 text-sm leading-relaxed text-zinc-300">
        <!-- Index-keyed on purpose: the model's reply repeats lines (blank
             lines between paragraphs, most obviously), and keying on the text
             threw "keyed each duplicate key" and blanked the panel. -->
        {#each triage.split('\n') as line, i (i)}
          <p>{#each segments(line) as seg}{#if seg.bold}<strong class="font-semibold text-zinc-100">{seg.text}</strong>{:else}{seg.text}{/if}{/each}</p>
        {/each}
      </div>
    </div>
  {:else if canWrite}
    <button onclick={() => runTriage(false)} disabled={triaging} class="mt-6 inline-flex items-center gap-2 rounded-md border border-amber-400/40 bg-amber-400/[0.06] px-3.5 py-2 text-sm font-medium text-amber-300 transition-colors hover:bg-amber-400/10 active:translate-y-px disabled:opacity-60">
      {#if triaging}
        <span class="inline-block h-1.5 w-1.5 animate-ping rounded-full bg-amber-300"></span>
        Analyzing on your endpoint...
      {:else}
        AI triage this issue
      {/if}
    </button>
  {/if}

  {#if triageError}
    <p class="mt-3 rounded-md border border-rose-900/60 bg-rose-950/40 px-3 py-2 text-sm text-rose-300">
      {triageError}
      {#if triageError.toLowerCase().includes('not configured')}
        <a href="/settings" class="ml-1 font-medium text-amber-300 underline hover:text-amber-200">Set up a model in Settings</a>
        to enable AI triage: it sends this issue's scrubbed stack trace to your own model endpoint and returns a plain-language cause and fix.
      {/if}
    </p>
  {/if}

  <section class="mt-8" aria-label="Issue occurrences">
    <div class="mb-3 flex flex-wrap items-center justify-between gap-3">
      <div>
        <h2 class="text-sm font-medium text-zinc-300">Occurrences</h2>
        {#if events.length > 0}
          <p class="mt-1 text-xs text-zinc-500">Showing the {events.length} most recent stored occurrence{events.length === 1 ? '' : 's'}, newest first.</p>
        {/if}
      </div>
      {#if events.length > 1}
        <label class="flex items-center gap-2 text-xs text-zinc-400" for="issue-occurrence">
          Occurrence
          <select id="issue-occurrence" bind:value={selectedEventIndex} class="max-w-full rounded-md border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-xs text-zinc-200">
            {#each events as event, index (event.id)}
              <option value={index}>#{index + 1} · {receivedAtLabel(event.received_at)}{event.release ? ` · ${event.release}` : ''}</option>
            {/each}
          </select>
        </label>
      {/if}
    </div>

    {#if eventsLoading}
      <p class="text-sm text-zinc-500">Loading occurrences...</p>
    {:else if eventsError}
      <p class="rounded-md border border-rose-900/60 bg-rose-950/40 px-3 py-2 text-sm text-rose-300">{eventsError}</p>
    {:else if events.length === 0}
      <p class="text-sm text-zinc-500">No stored occurrences are available for this issue.</p>
    {/if}

  {#if selectedEvent}
    {#if selectedEvent.trace_id && issue}
      <a
        href="/projects/{issue.project_id}/traces/{selectedEvent.trace_id}"
        class="mb-3 inline-flex items-center gap-2 rounded-md border border-zinc-800 bg-zinc-900/40 px-3 py-1.5 text-xs text-zinc-400 transition-colors hover:border-amber-400/50 hover:text-amber-300"
      >
        <span class="text-zinc-500">In trace</span>
        <span class="font-mono">{selectedEvent.trace_id.slice(0, 16)}</span>
        <span aria-hidden="true">&rarr;</span>
        <span>View trace</span>
      </a>
    {/if}
    {#if selectedEvent.exception_value || selectedEvent.message}
      <p class="mb-4 rounded-md border border-zinc-800 bg-zinc-900/40 px-3 py-2 text-sm text-zinc-300">
        {selectedEvent.exception_value || selectedEvent.message}
      </p>
    {/if}
    {#if selectedEvent.tags?.length}
      <dl class="mb-4 flex flex-wrap gap-2 text-xs">
        {#each selectedEvent.tags as tag}
          <div class="flex min-w-0 gap-1.5 rounded-md border border-zinc-800 bg-zinc-900/40 px-2 py-1">
            <dt class="min-w-0 break-all text-zinc-500">{tag.key}</dt>
            <dd class="min-w-0 break-all font-mono text-zinc-300">{tag.value}</dd>
          </div>
        {/each}
      </dl>
    {/if}

    {#if selectedEvent.stacktrace && selectedEvent.stacktrace.frames.length}
      <div class="overflow-hidden rounded-lg border border-zinc-800/80">
        <div class="flex items-center gap-2 border-b border-zinc-800/80 bg-zinc-900/40 px-4 py-2 text-xs font-medium text-zinc-400">
          Stack trace
          {#if selectedEvent.stacktrace.frames.some((f) => f.symbolicated)}
            <span class="rounded bg-emerald-500/10 px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-emerald-400">source-mapped</span>
          {/if}
        </div>
        <ul class="divide-y divide-zinc-800/60 font-mono text-[13px]">
          {#each [...selectedEvent.stacktrace.frames].reverse() as f, i (i)}
            <li class="px-4 py-2 {f.in_app === false ? 'opacity-50' : ''}">
              <div class="flex items-baseline gap-3">
                <span class="text-amber-300">{f.function || '?'}</span>
                <span class="truncate text-zinc-500">
                  {f.filename || f.module || ''}{f.lineno ? `:${f.lineno}` : ''}{f.symbolicated && f.colno ? `:${f.colno}` : ''}
                </span>
                {#if f.symbolicated}<span class="shrink-0 text-[10px] uppercase text-emerald-500/70">mapped</span>{/if}
                {#if f.in_app !== false}<span class="ml-auto shrink-0 text-[10px] uppercase text-zinc-600">in app</span>{/if}
              </div>
              {#if f.context_line}
                <div class="mt-1.5 overflow-x-auto rounded border border-zinc-800/80 bg-zinc-950 text-[12px] leading-relaxed">
                  {#each f.pre_context ?? [] as ln}
                    <div class="whitespace-pre px-3 text-zinc-600">{ln}</div>
                  {/each}
                  <div class="whitespace-pre border-l-2 border-amber-400 bg-amber-400/5 px-3 text-zinc-200">{f.context_line}</div>
                  {#each f.post_context ?? [] as ln}
                    <div class="whitespace-pre px-3 text-zinc-600">{ln}</div>
                  {/each}
                </div>
              {/if}
            </li>
          {/each}
        </ul>
      </div>
    {/if}

    <div class="mt-4 flex flex-wrap gap-x-6 gap-y-1 text-xs text-zinc-500">
      {#if selectedEvent.environment}<span>env <span class="font-mono text-zinc-400">{selectedEvent.environment}</span></span>{/if}
      {#if selectedEvent.release}<span>release <span class="font-mono text-zinc-400">{selectedEvent.release}</span></span>{/if}
      {#if selectedEvent.platform}<span>platform <span class="font-mono text-zinc-400">{selectedEvent.platform}</span></span>{/if}
      <span>received <span class="font-mono text-zinc-400" title={receivedAtLabel(selectedEvent.received_at)}>{relativeTime(selectedEvent.received_at)}</span></span>
    </div>
  {/if}
  </section>
{:else if error}
  <p class="rounded-md border border-rose-900/60 bg-rose-950/40 px-3 py-2 text-sm text-rose-300">{error}</p>
{:else}
  <div class="flex min-h-[30vh] items-center justify-center text-sm text-zinc-500">Loading...</div>
{/if}
