'use client';

import { useEffect, useState } from 'react';
import { nowSec } from './format';

// A clock that re-renders its caller, so relative-time text advances on its
// own instead of freezing between fetches.
//
// The bug this fixes: ageText/countdown/agoText are pure functions of an
// explicit `now`, and every call site passed nowSec() computed *during
// render*. React re-renders only when state changes, so "written 4 minutes
// ago" stayed "4 minutes ago" until some unrelated state update happened to
// repaint it. On a page that fetched once on mount, that was never.
//
// One second is the right default because these strings change on a second
// boundary at their finest (ageText reports whole seconds under 90). It is
// deliberately independent of the data poll interval: the numbers refresh
// every POLL_MS, but the sentence about how old they are ticks continuously,
// which is what makes the page feel live rather than lurching every 20s.
//
// Call this in the SMALLEST component that renders the text, never in one
// that also renders a list or a chart. It re-renders its caller once a
// second, so a tick placed too high re-renders everything beneath it at 1Hz.
// This is not theoretical: useNow() sat at the top of SessionsContent to feed
// a single "indexed 4m ago" caption, and every second it re-ran
// filterSessions and sortSessions across all 253 sessions and re-rendered a
// 200-row table. Sessions.tsx's IndexStaleBadge/IndexedAgo are what that
// should look like: leaves that render one string and nothing else.
//
// Where the expensive thing genuinely depends on the clock, measure before
// reaching for memoisation. Overview's overage chart takes xMax={now} over
// 941 points, so it really does recompute every second, and that measured
// 0.164ms per render (~10ms of CPU per minute), which is not worth
// restructuring for.
//
// Hidden tabs are not a concern to handle here. Browsers already clamp
// background timers to roughly once a minute, and a tab nobody is looking at
// does not need a smooth clock; usePoll is what stops fetching while hidden.
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(nowSec);

  useEffect(() => {
    const id = setInterval(() => setNow(nowSec()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);

  return now;
}
