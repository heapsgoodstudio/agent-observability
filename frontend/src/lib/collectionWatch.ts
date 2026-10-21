import type { ImportStatus } from '@/src/gen/claudeobs/v1/obs_pb';

// A profile change shows in the roster at once, but some of what it adds only
// exists after the server's first collection of that profile, which runs in
// the background a moment later: an Oh My Pi or opencode account, a Codex
// allowance, newly indexed sessions. These two rules decide when that
// collection has finished, so the dashboard can refresh then instead of on
// its next 20 second poll.

export type WatchStep = 'refresh' | 'wait' | 'stop';

// since is the change's time in whole seconds; startedAt and finishedAt are
// whole seconds too. An import that began at or after the change and has
// completed or failed is the one the change started. When nothing is running
// or queued a few seconds after the change, the change started no import (a
// rename, a disable), so there is nothing to wait for.
export function watchStep(status: Pick<ImportStatus, 'state' | 'startedAt' | 'finishedAt'> | undefined, since: number, elapsedMs: number): WatchStep {
  const state = status?.state ?? '';
  const started = (status?.startedAt ?? 0) >= since;
  if (started && (state === 'complete' || state === 'failed')) return 'refresh';
  if (state === 'running' || state === 'queued') return elapsedMs >= 180_000 ? 'stop' : 'wait';
  if (!started && elapsedMs >= 5_000) return 'stop';
  return elapsedMs >= 180_000 ? 'stop' : 'wait';
}

// Every second while a first collection is usually seconds away, then every
// five until the watch gives up.
export function watchDelay(elapsedMs: number): number {
  return elapsedMs < 15_000 ? 1_000 : 5_000;
}
