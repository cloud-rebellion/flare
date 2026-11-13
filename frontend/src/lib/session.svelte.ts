import { api, ApiError } from './api';
import { isSessionUser } from './ssoLaunch';
import type { User } from './types';

export const session = $state<{ user: User | null; loaded: boolean }>({
  user: null,
  loaded: false
});

export async function loadSession(): Promise<void> {
  try {
    const user = await api.me();
    if (isSessionUser(user)) session.user = user;
  } catch (error) {
    // An unavailable database or network is not proof that a known native
    // session ended. Only the server's actual signed-out result clears it.
    if (error instanceof ApiError && error.status === 401) session.user = null;
  }
  session.loaded = true;
}
