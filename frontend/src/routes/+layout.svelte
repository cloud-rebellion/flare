<script lang="ts">
  import '../app.css';
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { goto } from '$app/navigation';
  import { api } from '$lib/api';
  import { installFlareBrowserCapture } from '$lib/flare-client';
  import { session, loadSession } from '$lib/session.svelte';
  import { theme, initTheme, toggleTheme } from '$lib/theme.svelte';

  let { children } = $props();

  onMount(() => {
    const stopFlareCapture = installFlareBrowserCapture({
      dsn: import.meta.env.PUBLIC_FLARE_DSN,
      environment: import.meta.env.PUBLIC_FLARE_ENVIRONMENT || 'production',
      release: import.meta.env.PUBLIC_FLARE_RELEASE
    });
    initTheme();
    loadSession();
    return stopFlareCapture;
  });

  async function logout() {
    await api.logout();
    session.user = null;
    goto('/login');
  }

  const navItems = [
    { href: '/overview', label: 'Overview' },
    { href: '/projects', label: 'Projects' },
    { href: '/settings', label: 'Alerts' },
    { href: '/members', label: 'Team' },
    { href: '/docs', label: 'Docs' }
  ];
</script>

<div class="min-h-[100dvh] text-zinc-100">
  {#if session.user}
    <header class="sticky top-0 z-30 border-b border-zinc-800/80 bg-zinc-950/85 backdrop-blur">
      <div class="mx-auto flex h-14 max-w-6xl items-center gap-6 px-5">
        <a href="/projects" class="flex items-center gap-2">
          <span class="inline-block h-2.5 w-2.5 rounded-full bg-amber-400 shadow-[0_0_10px_2px_rgba(251,191,36,0.4)]"></span>
          <span class="text-sm font-semibold tracking-tight">Flare</span>
        </a>
        <nav class="flex items-center gap-1 text-sm">
          {#each navItems as item (item.href)}
            <a
              href={item.href}
              class="rounded-md px-2.5 py-1.5 transition-colors {page.url.pathname.startsWith(item.href)
                ? 'text-zinc-100'
                : 'text-zinc-400 hover:text-zinc-200'}"
            >
              {item.label}
            </a>
          {/each}
        </nav>
        <div class="ml-auto flex items-center gap-3 text-sm">
          <span class="hidden text-zinc-500 sm:inline">{session.user.email}</span>
          <button
            type="button"
            onclick={toggleTheme}
            aria-label={theme.value === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'}
            title={theme.value === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'}
            class="rounded-md border border-zinc-800 p-1.5 text-zinc-400 transition-colors hover:border-zinc-700 hover:text-zinc-200 active:translate-y-px"
          >
            {#if theme.value === 'dark'}
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="h-4 w-4" aria-hidden="true">
                <circle cx="12" cy="12" r="4" />
                <path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41" />
              </svg>
            {:else}
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="h-4 w-4" aria-hidden="true">
                <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z" />
              </svg>
            {/if}
          </button>
          <button
            onclick={logout}
            class="rounded-md border border-zinc-800 px-2.5 py-1.5 text-zinc-400 transition-colors hover:border-zinc-700 hover:text-zinc-200 active:translate-y-px"
          >
            Sign out
          </button>
        </div>
      </div>
    </header>
  {/if}

  <main class="mx-auto max-w-6xl px-5 py-8">
    {@render children()}
  </main>
</div>
