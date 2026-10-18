<script lang="ts">
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import ProjectTabs from '$lib/components/ProjectTabs.svelte';
  import type { IngestReceipt, Project } from '$lib/types';

  const id = $derived(page.params.id ?? '');
  let project = $state<Project | null>(null);
  let receipts = $state<IngestReceipt[] | null>(null);
  let error = $state<string | null>(null);
  let loading = $state(false);
  let updatedAt = $state<string | null>(null);
  let requestVersion = 0;
  let loadedProject = '';
  const nf = new Intl.NumberFormat();
  const reasonLabels: Record<string, string> = {
    invalid_envelope: 'Malformed envelope', invalid_event: 'Malformed event',
    invalid_event_id: 'Invalid event identity', invalid_transaction: 'Malformed transaction',
    invalid_transaction_id: 'Invalid transaction identity', invalid_client_report: 'Malformed SDK loss report',
    unsupported_session: 'Session tracking unsupported', unsupported_attachment: 'Attachment unsupported',
    unsupported_profile: 'Profile unsupported', unsupported_check_in: 'Sentry check-in unsupported',
    unsupported_log: 'Sentry log format unsupported', unsupported_span: 'Sentry span format unsupported',
    unsupported_metric: 'Sentry metric format unsupported', unsupported_other: 'Other unsupported item',
    identity_conflict: 'Item identity reused with different data', storage_failed: 'Storage unavailable'
  };

  $effect(() => {
    if (session.loaded && !session.user) goto('/login', { replaceState: true });
    if (session.loaded && session.user && id) load(id);
  });

  async function load(projectId: string) {
    const version = ++requestVersion;
    if (loadedProject !== projectId) {
      project = null;
      receipts = null;
      updatedAt = null;
      loadedProject = projectId;
    }
    loading = true;
    try {
      const [nextProject, nextReceipts] = await Promise.all([
        api.project(projectId), api.ingestReceipts(projectId)
      ]);
      if (version !== requestVersion) return;
      project = nextProject;
      receipts = nextReceipts;
      updatedAt = new Date().toLocaleTimeString();
      error = null;
    } catch (err) {
      if (version === requestVersion) error = err instanceof ApiError ? err.message : 'Failed to load intake receipts';
    } finally {
      if (version === requestVersion) loading = false;
    }
  }

  function reasons(receipt: IngestReceipt): [string, number][] {
    return Object.entries(receipt.rejection_counts ?? {}).sort(([a], [b]) => a.localeCompare(b));
  }
</script>

<div class="mb-6 flex flex-wrap items-center gap-2 text-sm text-zinc-400">
  <a href="/projects" class="hover:text-zinc-200">Projects</a><span>/</span>
  <a href={`/projects/${encodeURIComponent(id)}`} class="hover:text-zinc-200">{project?.name ?? 'Project'}</a>
</div>
<div class="mb-6 flex flex-wrap items-end justify-between gap-4">
  <div>
    <h1 class="text-xl font-semibold tracking-tight">Telemetry intake</h1>
    <p class="mt-1 text-sm text-zinc-400">What Flare accepted, rejected, or could not store.</p>
  </div>
  <div class="flex items-center gap-3">
    {#if updatedAt}<span class="text-xs text-zinc-400">Updated at {updatedAt}</span>{/if}
    <button type="button" disabled={loading} onclick={() => load(id)} class="rounded-md border border-zinc-700 px-3 py-1.5 text-sm hover:bg-zinc-800 disabled:opacity-50">{loading ? 'Loading…' : 'Refresh'}</button>
  </div>
</div>
<ProjectTabs projectId={id} active="intake" />
{#if error}
  <div role="alert" class="mb-5 rounded-lg border border-rose-900/50 bg-rose-950/20 p-4 text-sm text-rose-300">{error}{receipts ? '. Showing the last loaded snapshot.' : ''}</div>
{/if}
<div class="mb-5 rounded-lg border border-zinc-800 bg-zinc-900/40 p-4 text-sm text-zinc-300">
  <p>Accepted intake confirms delivery to Flare. It does not prove that an app reports every error.</p>
  <p class="mt-1 text-xs text-zinc-400">Rejection reasons are available for newly processed Sentry requests. Older receipts and other formats may have no breakdown. A malformed envelope counts as one request; other reason counts describe items. Retrying is appropriate only when the receipt says it is retryable.</p>
</div>
{#if !receipts && !error}
  <p class="p-8 text-sm text-zinc-400">Loading intake receipts…</p>
{:else if receipts}
  <section class="min-w-0 overflow-hidden rounded-lg border border-zinc-800/80 bg-zinc-900/40">
    <div class="flex flex-wrap justify-between gap-2 border-b border-zinc-800/80 px-5 py-4">
      <h2 class="text-sm font-semibold">Recent requests</h2>
      <span class="text-xs text-zinc-400">Newest first · up to 100 requests · no payload content</span>
    </div>
    {#if receipts.length === 0}
      <p class="px-5 py-10 text-sm text-zinc-400">No intake receipts observed for this project. Verify the app's connection and send a controlled test signal.</p>
    {:else}
      <div class="overflow-x-auto">
        <table class="w-full min-w-[900px] text-left text-sm">
          <thead class="bg-zinc-900/60 text-xs text-zinc-400"><tr>
            <th class="px-5 py-3 font-medium">Received</th><th class="px-3 py-3 font-medium">Signal</th>
            <th class="px-3 py-3 font-medium">Result</th><th class="px-3 py-3 text-right font-medium">Accepted</th>
            <th class="px-3 py-3 text-right font-medium">Rejected</th><th class="px-3 py-3 text-right font-medium">Dropped</th>
            <th class="px-5 py-3 font-medium">Rejection detail</th>
          </tr></thead>
          <tbody class="divide-y divide-zinc-800/60">
            {#each receipts as row (row.id)}
              <tr>
                <td class="px-5 py-3 text-xs text-zinc-400" title={row.id}>{row.received_at ? new Date(row.received_at).toLocaleString() : 'Not recorded'}</td>
                <td class="px-3 py-3 text-xs"><span class="text-zinc-200">{row.pillar}</span><br /><span class="text-zinc-500">{row.protocol}</span></td>
                <td class="px-3 py-3 text-xs"><span class={['rejected', 'failed'].includes(row.outcome) ? 'text-rose-300' : row.outcome === 'partial' || row.outcome === 'in_progress' ? 'text-amber-300' : 'text-emerald-300'}>{row.outcome.replaceAll('_', ' ')}</span><br /><span class="text-zinc-500">HTTP {row.http_status}{row.retryable ? ' · retryable' : ''}</span></td>
                <td class="px-3 py-3 text-right font-mono text-xs">{nf.format(row.accepted_count)}</td>
                <td class="px-3 py-3 text-right font-mono text-xs {row.rejected_count ? 'text-rose-300' : 'text-zinc-500'}" title={row.rejection_counts?.invalid_envelope ? 'The envelope could not be parsed; its item count is unknown.' : undefined}>{row.rejection_counts?.invalid_envelope ? '-' : nf.format(row.rejected_count)}</td>
                <td class="px-3 py-3 text-right font-mono text-xs {row.dropped_count ? 'text-amber-300' : 'text-zinc-500'}">{nf.format(row.dropped_count)}</td>
                <td class="px-5 py-3 text-xs text-zinc-300">
                  {#each reasons(row) as [code, count] (code)}
                    <div>{reasonLabels[code] ?? 'Unclassified rejection'} <span class="font-mono text-zinc-400">×{nf.format(count)}</span></div>
                  {:else}
                    <span class="text-zinc-500">{row.rejected_count > 0 || row.outcome === 'rejected' || row.outcome === 'failed' ? 'Breakdown unavailable' : '-'}</span>
                  {/each}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </section>
{/if}
