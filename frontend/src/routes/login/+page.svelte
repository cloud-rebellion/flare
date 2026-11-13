<script lang="ts">
  import { onMount } from 'svelte';
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { api, ApiError } from '$lib/api';
  import { session } from '$lib/session.svelte';
  import { checkPortalSession, consumePortalIntent } from '$lib/ssoLaunch';

  let mode = $state<'login' | 'register'>('login');
  let email = $state('');
  let password = $state('');
  let orgName = $state('');
  let error = $state<string | null>(null);
  let busy = $state(false);

  let ssoOpen = $state(false);
  let ssoOrg = $state('');

  const ssoErrors: Record<string, string> = {
    sso_unknown_org: 'No workspace with that name.',
    sso_disabled: 'SSO is not enabled for that workspace.',
    sso_state: 'The sign-in link expired. Please try again.',
    sso_unverified: 'Your identity provider has not verified your email.',
    sso_other_org: 'That account belongs to a different workspace.',
    sso_provision: 'Could not create your account. Ask an admin to invite you.',
    sso_link: 'Could not link your account to your identity provider.',
    sso_identity_mismatch:
      'This account is linked to a different identity provider account. Ask an owner to check the workspace SSO settings.',
    session_expired: 'Your session expired. Please sign in again.'
  };

  onMount(() => {
    const code = page.url.searchParams.get('error');
    if (code) error = ssoErrors[code] ?? 'Single sign-on failed. Please try again.';
    const intent = consumePortalIntent(new URL(window.location.href), (path) => {
      // Initial onMount runs while SvelteKit constructs its root. Its shallow
      // replaceState calls root.$set before that root exists. Consume only this
      // URL marker synchronously and preserve all router-owned history state.
      window.history.replaceState(window.history.state, '', path);
    });
    if (!intent) return;
    const controller = new AbortController();
    busy = true;
    void checkPortalSession(controller.signal).then((result) => {
      if (controller.signal.aborted) return;
      if (result.kind === 'session') {
        session.user = result.user;
        void goto('/projects', { replaceState: true });
      } else if (result.kind === 'launch') {
        window.location.assign('/api/auth/oidc/launch');
      } else {
        error = 'Single sign-on could not be checked. You can sign in below.';
      }
    }).finally(() => { busy = false; });
    return () => controller.abort();
  });

  function startSso(e: SubmitEvent) {
    e.preventDefault();
    if (!ssoOrg.trim()) return;
    window.location.href = '/api/auth/oidc/start?org=' + encodeURIComponent(ssoOrg.trim());
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    error = null;
    busy = true;
    try {
      session.user =
        mode === 'login'
          ? await api.login(email, password)
          : await api.register(email, password, orgName);
      goto('/projects', { replaceState: true });
    } catch (err) {
      error = err instanceof ApiError ? err.message : 'Something went wrong';
    } finally {
      busy = false;
    }
  }
</script>

<div class="flex min-h-[70vh] items-center justify-center">
  <div class="w-full max-w-sm">
    <div class="mb-8 flex items-center gap-2.5">
      <span class="inline-block h-3 w-3 rounded-full bg-amber-400 shadow-[0_0_12px_3px_rgba(251,191,36,0.4)]"></span>
      <h1 class="text-xl font-semibold tracking-tight">Flare</h1>
    </div>

    <h2 class="mb-1 text-lg font-medium">
      {mode === 'login' ? 'Sign in' : 'Create your workspace'}
    </h2>
    <p class="mb-6 text-sm text-zinc-500">
      {mode === 'login' ? 'Welcome back.' : 'Errors, logs, and traces on infrastructure you own.'}
    </p>

    <form onsubmit={submit} class="flex flex-col gap-4">
      {#if mode === 'register'}
        <div class="flex flex-col gap-1.5">
          <label for="org" class="text-xs font-medium text-zinc-400">Workspace name</label>
          <input
            id="org"
            bind:value={orgName}
            placeholder="Northwind Engineering"
            class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
          />
        </div>
      {/if}
      <div class="flex flex-col gap-1.5">
        <label for="email" class="text-xs font-medium text-zinc-400">Email</label>
        <input
          id="email"
          type="email"
          required
          bind:value={email}
          autocomplete="email"
          class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
        />
      </div>
      <div class="flex flex-col gap-1.5">
        <div class="flex items-center justify-between">
          <label for="password" class="text-xs font-medium text-zinc-400">Password</label>
          {#if mode === 'login'}
            <a href="/forgot-password" class="text-xs text-zinc-500 transition-colors hover:text-amber-300">Forgot password?</a>
          {/if}
        </div>
        <input
          id="password"
          type="password"
          required
          minlength={8}
          bind:value={password}
          autocomplete={mode === 'login' ? 'current-password' : 'new-password'}
          class="rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
        />
      </div>

      {#if error}
        <p class="rounded-md border border-rose-900/60 bg-rose-950/40 px-3 py-2 text-sm text-rose-300">
          {error}
        </p>
      {/if}

      <button
        type="submit"
        disabled={busy}
        class="mt-1 rounded-md bg-amber-400 px-3 py-2 text-sm font-medium text-zinc-950 transition-colors hover:bg-amber-300 active:translate-y-px disabled:opacity-60"
      >
        {busy ? 'Working...' : mode === 'login' ? 'Sign in' : 'Create workspace'}
      </button>
    </form>

    <button
      class="mt-5 text-sm text-zinc-500 transition-colors hover:text-zinc-300"
      onclick={() => {
        mode = mode === 'login' ? 'register' : 'login';
        error = null;
      }}
    >
      {mode === 'login' ? 'Need an account? Create one' : 'Have an account? Sign in'}
    </button>

    {#if mode === 'login'}
      <div class="mt-6 border-t border-zinc-800/80 pt-5">
        {#if ssoOpen}
          <form onsubmit={startSso} class="flex gap-2">
            <input
              bind:value={ssoOrg}
              placeholder="workspace name"
              class="flex-1 rounded-md border border-zinc-800 bg-zinc-900 px-3 py-2 text-sm outline-none focus:border-amber-400/60"
            />
            <button type="submit" class="rounded-md border border-zinc-700 px-3 py-2 text-sm text-zinc-200 transition-colors hover:bg-zinc-800 active:translate-y-px">Continue</button>
          </form>
        {:else}
          <button onclick={() => (ssoOpen = true)} class="text-sm text-zinc-400 transition-colors hover:text-zinc-200">
            Sign in with SSO
          </button>
        {/if}
      </div>
    {/if}

    <a href="/docs" class="mt-6 inline-block text-xs text-zinc-600 transition-colors hover:text-zinc-400">Documentation &amp; API reference &rarr;</a>
  </div>
</div>
