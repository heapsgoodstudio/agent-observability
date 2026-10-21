'use client';

import { useEffect, useState } from 'react';
import { nowSec } from './format';
import { describeErr } from './presentation';
import { pollCycle } from './pollCycle';
import { COLLECTION_FINISHED_EVENT, PROFILES_CHANGED_EVENT } from './providers';

export const POLL_MS = 20000;
export type Polled<T> = { data: T | null; error: string | null; fetchedAt: number };

// The fetcher is the scope identity. A new scope hides the old response in
// the same render, before effects run. Old promises cannot publish afterward.
export function usePoll<T>(fetcher: () => Promise<T>, intervalMs = POLL_MS): Polled<T> {
  const [result, setResult] = useState<Polled<T> & { owner: typeof fetcher }>({ owner: fetcher, data: null, error: null, fetchedAt: 0 });
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const visible = () => document.visibilityState !== 'hidden';
    const cycle = pollCycle(fetcher,
      (data) => setResult({ owner: fetcher, data, error: null, fetchedAt: nowSec() }),
      (error) => setResult((old) => ({ owner: fetcher, data: old.owner === fetcher ? old.data : null, fetchedAt: old.owner === fetcher ? old.fetchedAt : 0, error: describeErr(error) })),
      () => { timer = setTimeout(run, intervalMs); }, visible);
    const run = () => { clearTimeout(timer); return cycle.run(); };
    const onVisible = () => { clearTimeout(timer); void run(); };
    // A data change must not be lost to a fetch already in flight, which may
    // have left before the change landed.
    const onChanged = () => { clearTimeout(timer); void cycle.run(true); };
    void run();
    document.addEventListener('visibilitychange', onVisible);
    window.addEventListener('focus', onVisible);
    window.addEventListener(PROFILES_CHANGED_EVENT, onChanged);
    window.addEventListener(COLLECTION_FINISHED_EVENT, onChanged);
    return () => {
      cycle.dispose();
      clearTimeout(timer);
      document.removeEventListener('visibilitychange', onVisible);
      window.removeEventListener('focus', onVisible);
      window.removeEventListener(PROFILES_CHANGED_EVENT, onChanged);
      window.removeEventListener(COLLECTION_FINISHED_EVENT, onChanged);
    };
  }, [fetcher, intervalMs]);
  return result.owner === fetcher ? result : { data: null, error: null, fetchedAt: 0 };
}
