import { describe, expect, it } from 'vitest';
import { allowanceStale, codexPlanLabel } from './allowance';

describe('allowance snapshot freshness', () => {
  it('ages the display even without a new server response', () => {
    const snapshot = { fetchedAt: 1000, stale: false };
    expect(allowanceStale(snapshot, 1300)).toBe(false);
    expect(allowanceStale(snapshot, 1301)).toBe(true);
    expect(allowanceStale(snapshot, 1010, true)).toBe(true);
  });
  it('respects missing timestamps, bad future timestamps and provider staleness', () => {
    expect(allowanceStale({ fetchedAt: 0, stale: false }, 1000)).toBe(true);
    expect(allowanceStale({ fetchedAt: 1061, stale: false }, 1000)).toBe(true);
    expect(allowanceStale({ fetchedAt: 1000, stale: true }, 1000)).toBe(true);
  });
  it('uses a plain plan name without guessing the price tier', () => {
    expect(codexPlanLabel('self_serve_business_prolite')).toBe('Business');
    expect(codexPlanLabel('unknown_internal_code')).toBe('Plan details unavailable');
  });
});
