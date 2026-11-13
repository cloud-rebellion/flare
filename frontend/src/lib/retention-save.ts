// The save and reset flow of the retention settings, kept out of the Svelte
// component so it can be tested. The server is the gate: every write carries
// the state the admin looked at (`expected`), and a 409 (a reduction that was
// not confirmed, or a policy someone else changed meanwhile) is answered by
// reloading and showing the confirm again from FRESH data. Confirming sends
// confirm_reduction: true. Before, a 409 re-showed the confirm from the stale
// view and the confirm click re-sent false, so it could never succeed.
import { reduces } from './retention';
import type { RetentionExpected, RetentionPolicy, RetentionSettings } from './types';

export interface RetentionSaveDeps {
  load(): Promise<RetentionSettings>;
  setRetention(p: RetentionPolicy, confirmReduction: boolean, expected: RetentionExpected): Promise<RetentionSettings>;
  resetRetention(confirmReduction: boolean, expected: RetentionExpected): Promise<RetentionSettings>;
}

export type RetentionSaveOutcome =
  /** Written. `settings` is the server's view after the change. */
  | { kind: 'saved'; settings: RetentionSettings }
  /** Needs (another) explicit confirm. `settings` is freshly loaded; render the confirm from it. */
  | { kind: 'confirm'; settings: RetentionSettings; changedElsewhere: boolean }
  | { kind: 'error'; message: string };

export function expectedOf(s: RetentionSettings): RetentionExpected {
  return { effective: s.effective, updated_at: s.updated_at };
}

const status = (err: unknown): number | undefined =>
  typeof err === 'object' && err !== null && 'status' in err ? Number((err as { status: unknown }).status) : undefined;
const message = (err: unknown, fallback: string): string =>
  err instanceof Error && err.message ? err.message : fallback;

async function refreshed(deps: RetentionSaveDeps, seen: RetentionSettings): Promise<RetentionSaveOutcome> {
  try {
    const fresh = await deps.load();
    const changedElsewhere =
      fresh.updated_at !== seen.updated_at || JSON.stringify(fresh.effective) !== JSON.stringify(seen.effective);
    return { kind: 'confirm', settings: fresh, changedElsewhere };
  } catch (err) {
    return { kind: 'error', message: message(err, 'Failed to reload retention settings') };
  }
}

/**
 * Save `next`. `confirmed` is true only when the admin clicked the confirm
 * button for the view in `seen`. A reduction that is not yet confirmed first
 * reloads, so the confirm quotes current counts and today's cutoffs.
 */
export async function saveRetention(
  deps: RetentionSaveDeps,
  seen: RetentionSettings,
  next: RetentionPolicy,
  confirmed: boolean
): Promise<RetentionSaveOutcome> {
  if (!confirmed && reduces(seen.effective, next)) return refreshed(deps, seen);
  try {
    return { kind: 'saved', settings: await deps.setRetention(next, confirmed, expectedOf(seen)) };
  } catch (err) {
    if (status(err) === 409) return refreshed(deps, seen);
    return { kind: 'error', message: message(err, 'Failed to save retention') };
  }
}

/** Reset to the environment default, with the same confirm rules. */
export async function resetRetention(
  deps: RetentionSaveDeps,
  seen: RetentionSettings,
  confirmed: boolean
): Promise<RetentionSaveOutcome> {
  if (!confirmed && seen.source === 'stored' && reduces(seen.effective, seen.env_default)) return refreshed(deps, seen);
  try {
    return { kind: 'saved', settings: await deps.resetRetention(confirmed, expectedOf(seen)) };
  } catch (err) {
    if (status(err) === 409) return refreshed(deps, seen);
    return { kind: 'error', message: message(err, 'Failed to reset retention') };
  }
}

export type RetentionAction = 'save' | 'reset';

/** What the settings form holds between clicks. */
export interface RetentionFormState {
  settings: RetentionSettings;
  /** Which confirm box is showing; a click while it shows IS the confirmation. */
  confirming: RetentionAction | null;
}

export interface RetentionSubmitResult {
  settings: RetentionSettings;
  confirming: RetentionAction | null;
  error: string | null;
  saved: string | null;
  /** True after a write: reset the form fields to the saved policy. */
  adopt: boolean;
}

export const CHANGED_ELSEWHERE =
  'Retention was changed by someone else while this page was open. Review the current policy above before saving.';

/**
 * One click on Save, Reset, or a confirm button, as a pure state transition.
 * The component only renders the result. A click while the matching confirm
 * box shows sends confirm_reduction: true; any other click does not. After a
 * 409 the confirm is re-shown from the FRESH settings only if the change
 * still lowers retention against them.
 */
export async function submitRetention(
  deps: RetentionSaveDeps,
  state: RetentionFormState,
  action: RetentionAction,
  next?: RetentionPolicy
): Promise<RetentionSubmitResult> {
  const confirmed = state.confirming === action;
  let out: RetentionSaveOutcome;
  if (action === 'save') {
    if (!next) return { ...keep(state), error: 'Nothing to save', saved: null, adopt: false };
    out = await saveRetention(deps, state.settings, next, confirmed);
  } else {
    out = await resetRetention(deps, state.settings, confirmed);
  }
  if (out.kind === 'saved') {
    return {
      settings: out.settings,
      confirming: null,
      error: null,
      saved: action === 'save' ? 'Saved. It applies on the next retention pass.' : 'Reset to the environment default.',
      adopt: true
    };
  }
  if (out.kind === 'error') return { ...keep(state), error: out.message, saved: null, adopt: false };
  const fresh = out.settings;
  const stillLowers =
    action === 'save'
      ? reduces(fresh.effective, next as RetentionPolicy)
      : fresh.source === 'stored' && reduces(fresh.effective, fresh.env_default);
  return {
    settings: fresh,
    confirming: stillLowers ? action : null,
    error: out.changedElsewhere ? CHANGED_ELSEWHERE : null,
    saved: null,
    adopt: false
  };
}

function keep(state: RetentionFormState): Pick<RetentionSubmitResult, 'settings' | 'confirming'> {
  return { settings: state.settings, confirming: state.confirming };
}
