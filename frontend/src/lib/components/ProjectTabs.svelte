<script lang="ts">
  let { projectId, active }: { projectId: string; active: string } = $props();
  const sections = [
    { key: 'issues', path: '', label: 'Issues' },
    { key: 'logs', path: '/logs', label: 'Logs' },
    { key: 'traces', path: '/traces', label: 'Traces' },
    { key: 'metrics', path: '/metrics', label: 'Metrics' },
    { key: 'business', path: '/business-health', label: 'Business' },
    { key: 'sdk-loss', path: '/sdk-loss', label: 'SDK loss' },
    { key: 'automation', path: '/automation', label: 'Automation' },
    { key: 'releases', path: '/releases', label: 'Releases' }
  ];
</script>

<nav aria-label="Project sections" class="mb-5 flex items-center gap-1 overflow-x-auto border-b border-zinc-800/80">
  {#each sections as section (section.key)}
    <a
      href={`/projects/${encodeURIComponent(projectId)}${section.path}`}
      aria-current={active === section.key ? 'page' : undefined}
      class="-mb-px shrink-0 whitespace-nowrap border-b-2 px-3 py-2 text-sm {active === section.key ? 'border-amber-400 text-zinc-100' : 'border-transparent text-zinc-400 hover:text-zinc-200'}"
    >{section.label}</a>
  {/each}
</nav>
