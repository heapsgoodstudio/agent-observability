import { describe, expect, it } from 'vitest';
import { watchDelay, watchStep } from './collectionWatch';

describe('refreshing after a profile change', () => {
  const since = 1_790_116_200;

  it('refreshes once the import the change started has finished', () => {
    expect(watchStep({ state: 'complete', startedAt: since, finishedAt: since + 1 }, since, 1_000)).toBe('refresh');
    expect(watchStep({ state: 'failed', startedAt: since + 2, finishedAt: since + 3 }, since, 3_000)).toBe('refresh');
  });

  it('keeps waiting while that import, or one ahead of it, is still running', () => {
    expect(watchStep({ state: 'running', startedAt: since, finishedAt: 0 }, since, 1_000)).toBe('wait');
    expect(watchStep({ state: 'running', startedAt: since - 60, finishedAt: 0 }, since, 30_000)).toBe('wait');
    expect(watchStep({ state: 'queued', startedAt: 0, finishedAt: 0 }, since, 30_000)).toBe('wait');
  });

  it('does not mistake an import finished before the change for this one', () => {
    expect(watchStep({ state: 'complete', startedAt: since - 30, finishedAt: since - 29 }, since, 1_000)).toBe('wait');
  });

  it('stops when the change started no import, and gives up eventually', () => {
    expect(watchStep({ state: 'complete', startedAt: since - 30, finishedAt: since - 29 }, since, 5_000)).toBe('stop');
    expect(watchStep({ state: 'idle', startedAt: 0, finishedAt: 0 }, since, 5_000)).toBe('stop');
    expect(watchStep(undefined, since, 5_000)).toBe('stop');
    expect(watchStep({ state: 'running', startedAt: since, finishedAt: 0 }, since, 180_000)).toBe('stop');
  });

  it('checks every second at first, then backs off', () => {
    expect(watchDelay(0)).toBe(1_000);
    expect(watchDelay(14_999)).toBe(1_000);
    expect(watchDelay(15_000)).toBe(5_000);
  });
});
