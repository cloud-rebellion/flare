<script lang="ts">
  import { page } from '$app/state';

  let { children } = $props();

  const nav = [
    { href: '/docs', label: 'Overview' },
    { href: '/docs#send', label: 'Send events' },
    { href: '/docs#sourcemaps', label: 'Source maps' },
    { href: '/docs#releases', label: 'Releases' },
    { href: '/docs#mcp', label: 'AI connection (MCP)' },
    { href: '/docs/api', label: 'API reference' }
  ];
</script>

<div class="mx-auto flex max-w-6xl gap-10 px-5 py-8">
  <aside class="hidden w-48 shrink-0 md:block">
    <a href="/" class="mb-6 flex items-center gap-2">
      <span class="inline-block h-2.5 w-2.5 rounded-full bg-amber-400 shadow-[0_0_10px_2px_rgba(251,191,36,0.4)]"></span>
      <span class="text-sm font-semibold tracking-tight">Flare docs</span>
    </a>
    <nav class="flex flex-col gap-0.5 text-sm">
      {#each nav as item (item.href)}
        {@const active = (page.url.pathname as string) === item.href.split('#')[0]}
        <a
          href={item.href}
          class="rounded-md px-2.5 py-1.5 transition-colors {active ? 'text-zinc-200' : 'text-zinc-500 hover:text-zinc-300'}"
        >
          {item.label}
        </a>
      {/each}
    </nav>
    <div class="mt-6 border-t border-zinc-800/80 pt-4">
      <a href="/projects" class="rounded-md px-2.5 py-1.5 text-sm text-amber-400 transition-colors hover:text-amber-300">
        Open dashboard &rarr;
      </a>
    </div>
  </aside>

  <div class="min-w-0 flex-1">
    {@render children()}
  </div>
</div>
