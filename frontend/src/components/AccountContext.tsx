'use client';
import { createContext, useCallback, useContext, useEffect, type ReactNode } from 'react';
import type { GetUsageSnapshotResponse } from '@/src/gen/claudeobs/v1/obs_pb';
import { obsClient } from '@/src/lib/client';
import { usePoll, type Polled } from '@/src/lib/usePoll';
import { COLLECTION_FINISHED_EVENT, PROFILES_CHANGED_EVENT } from '@/src/lib/providers';
import { watchDelay, watchStep } from '@/src/lib/collectionWatch';
const Context = createContext<Polled<GetUsageSnapshotResponse>>({ data: null, error: null, fetchedAt: 0 });
export function AccountProvider({ children }: { children: ReactNode }) {
  const fetcher = useCallback(() => obsClient.getUsageSnapshot({}), []);
  const snapshot = usePoll(fetcher);
  useRefreshAfterCollection();
  return <Context.Provider value={snapshot}>{children}</Context.Provider>;
}
export const useAccounts = () => useContext(Context);

// After a profile change, follow the collection it started and announce when
// it finishes, so every poll refetches then rather than on its next interval.
// It lives here, beside the sidebar's accounts, because the shell outlives the
// Settings page: leaving Settings straight after saving must still refresh.
// A later change restarts the watch from its own time.
function useRefreshAfterCollection() {
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    let generation = 0;
    const onChange = () => {
      clearTimeout(timer);
      const mine = ++generation;
      const since = Math.floor(Date.now() / 1000);
      const began = Date.now();
      const check = async () => {
        const elapsed = Date.now() - began;
        let step: ReturnType<typeof watchStep>;
        try {
          step = watchStep((await obsClient.getSettings({})).historyImport, since, elapsed);
        } catch {
          step = elapsed >= 180_000 ? 'stop' : 'wait';
        }
        if (mine !== generation) return;
        if (step === 'refresh') window.dispatchEvent(new Event(COLLECTION_FINISHED_EVENT));
        else if (step === 'wait') timer = setTimeout(check, watchDelay(elapsed));
      };
      timer = setTimeout(check, watchDelay(0));
    };
    window.addEventListener(PROFILES_CHANGED_EVENT, onChange);
    return () => {
      generation++;
      clearTimeout(timer);
      window.removeEventListener(PROFILES_CHANGED_EVENT, onChange);
    };
  }, []);
}
