import type { User } from './types';

export function isSessionUser(value: unknown): value is User {
  if (value === null || typeof value !== 'object') return false;
  const user = value as Record<string, unknown>;
  return typeof user.id === 'string' && user.id.length > 0 &&
    typeof user.email === 'string' && user.email.length > 0 &&
    typeof user.org_id === 'string' && user.org_id.length > 0 &&
    typeof user.role === 'string' && ['owner', 'admin', 'member', 'viewer'].includes(user.role);
}

// Consume the one-shot intent synchronously, before any network request.
export function consumePortalIntent(url: URL, replace: (path: string) => void): boolean {
  const intent = url.searchParams.getAll('sso');
  if (intent.length === 0) return false;
  url.searchParams.delete('sso');
  replace(url.pathname + url.search + url.hash);
  return intent.length === 1 && intent[0] === '1' && !url.searchParams.has('error');
}

type PortalResult = { kind: 'session'; user: User } | { kind: 'launch' } | { kind: 'stopped' };

async function boundedJSON(response: Response): Promise<unknown> {
  if (response.headers.get('content-type')?.split(';')[0].trim() !== 'application/json' || !response.body) {
    throw new Error('session response unavailable');
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder('utf-8', { fatal: true });
  let bytes = 0;
  let text = '';
  try {
    for (;;) {
      const part = await reader.read();
      if (part.done) break;
      bytes += part.value.byteLength;
      if (bytes > 65536) {
        await reader.cancel();
        throw new Error('session response exceeds bounds');
      }
      text += decoder.decode(part.value, { stream: true });
    }
    return JSON.parse(text + decoder.decode());
  } finally {
    reader.releaseLock();
  }
}

// Unknown state stops. Only an actual signed-out response can reach readiness,
// and only the fixed same-origin launch endpoint can start an authorization.
export async function checkPortalSession(signal: AbortSignal, request: typeof fetch = fetch): Promise<PortalResult> {
  async function probe(path: string): Promise<{ status: number; body?: unknown }> {
    const timeout = new AbortController();
    const abort = () => timeout.abort();
    if (signal.aborted) throw new Error('cancelled');
    signal.addEventListener('abort', abort, { once: true });
    const timer = setTimeout(abort, 5000);
    try {
      const response = await request(path, { method: 'GET', credentials: 'same-origin',
        cache: 'no-store', redirect: 'error', signal: timeout.signal, headers: { Accept: 'application/json' } });
      return { status: response.status, body: response.status === 200 ? await boundedJSON(response) : undefined };
    } finally {
      clearTimeout(timer);
      signal.removeEventListener('abort', abort);
    }
  }
  try {
    const native = await probe('/api/auth/me');
    if (signal.aborted) return { kind: 'stopped' };
    if (native.status === 200) {
      return isSessionUser(native.body) ? { kind: 'session', user: native.body } : { kind: 'stopped' };
    }
    if (native.status !== 401) return { kind: 'stopped' };
    const provider = await probe('/api/auth/oidc/launch-status');
    if (signal.aborted || provider.status !== 200 || provider.body === null || typeof provider.body !== 'object' ||
      (provider.body as Record<string, unknown>).ready !== true) return { kind: 'stopped' };
    return { kind: 'launch' };
  } catch {
    return { kind: 'stopped' };
  }
}
