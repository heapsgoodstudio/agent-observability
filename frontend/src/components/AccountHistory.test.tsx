import { create } from '@bufbuild/protobuf';
import type { ReactNode } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { AccountSnapshotSchema, GetAccountHistoryResponseSchema } from '@/src/gen/claudeobs/v1/obs_pb';
import { BillingCoverage, BillingHistory } from './AccountHistory';

const { renderToStaticMarkup } = require('react-dom/server') as { renderToStaticMarkup(node: ReactNode): string };
vi.mock('@/src/lib/client', () => ({ obsClient: {} }));
vi.mock('@/src/lib/useNow', () => ({ useNow: () => 2_000_000_000 }));
vi.mock('@/src/lib/usePoll', () => ({ usePoll: () => ({ data: null, error: null, fetchedAt: 0 }) }));

describe('billing history interpretation', () => {
  it('identifies the current MTD reading separately from the selected history range', () => {
    const account = create(AccountSnapshotSchema, { provider: 'claude', key: 'claude:test', displayName: 'Work', billing: { reportedOverageUsd: 951.19, period: 'Month to date at observation', pricingSource: 'Claude usage cache' } });
    const html = renderToStaticMarkup(<BillingHistory account={account} />);
    expect(html).toContain('$951.19');
    expect(html).toContain('Latest reported amount');
    expect(html).toContain('Month to date at observation');
    expect(html).toContain('not a total for the selected 7 days');
    expect(html).toContain('Source: Claude usage cache');
  });

  it('states retained coverage rather than implying the entire selected range is observed', () => {
    const from = Date.UTC(2026, 5, 24) / 1000;
    const first = Date.UTC(2026, 7, 9) / 1000;
    const last = Date.UTC(2026, 8, 22) / 1000;
    const html = renderToStaticMarkup(<BillingCoverage history={create(GetAccountHistoryResponseSchema, { from, to: last, billing: [{ points: [{ ts: last, value: 951 }, { ts: first, value: 25000 }] }] })} />);
    expect(html).toContain('Retained readings:');
    expect(html).toContain('Aug');
    expect(html).toContain('Sep');
    expect(html).toContain('Earlier dates in this range have no retained readings.');
  });

  it('does not invent coverage dates for empty history', () => {
    expect(renderToStaticMarkup(<BillingCoverage history={create(GetAccountHistoryResponseSchema)} />)).toBe('');
  });
});
