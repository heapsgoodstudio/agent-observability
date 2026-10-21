import { create } from '@bufbuild/protobuf';
import type { ReactNode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AccountSnapshotSchema, GetAccountHistoryResponseSchema, type AccountSnapshot, type GetAccountHistoryResponse } from '@/src/gen/claudeobs/v1/obs_pb';
import { Overview } from './Overview';

const { renderToStaticMarkup } = require('react-dom/server') as { renderToStaticMarkup(node: ReactNode): string };
const state = vi.hoisted(() => ({ account: null as AccountSnapshot | null, history: null as GetAccountHistoryResponse | null }));
vi.mock('next/navigation', () => ({ useRouter: () => ({ replace: vi.fn() }), useSearchParams: () => new URLSearchParams(), usePathname: () => '/' }));
vi.mock('@/src/components/AccountContext', () => ({ useAccounts: () => ({ data: { accounts: [state.account], errors: [] }, error: null }) }));
vi.mock('@/src/lib/useScope', () => ({ useScope: () => ({ provider: state.account?.provider, accountKey: state.account?.key, profile: '' }) }));
vi.mock('@/src/lib/scrollTarget', () => ({ useScrollTarget: () => {} }));
vi.mock('@/src/lib/useNow', () => ({ useNow: () => 2_000_000_000 }));
vi.mock('@/src/lib/client', () => ({ obsClient: {} }));
vi.mock('@/src/components/AccountHistory', async (original) => ({ ...await original<object>(), useAccountHistory: () => ({ data: state.history, error: null }), BillingHistory: () => null }));

beforeEach(() => {
  state.account = create(AccountSnapshotSchema, {
    key: 'claude:one', provider: 'claude', includedUsageAllowed: true,
    currentWindowId: 'current', weeklyWindowId: 'weekly',
    windows: [
      { id: 'current', label: 'Current window' },
      { id: 'weekly', label: 'Weekly window' },
      { id: 'sonnet', label: 'Sonnet weekly', model: 'Sonnet', usedPercent: 23, remainingPercent: 77, resetsAt: 2_000_000_060, durationSeconds: 604800 },
    ],
  });
  state.history = create(GetAccountHistoryResponseSchema, { windows: [{ windowId: 'sonnet', label: 'Sonnet history', resetAt: 2_000_000_060, points: [{ ts: 2_000_000_000, value: 23 }] }] });
});

describe('account allowance windows', () => {
  it('shows Claude model limits and their retained trace without opening a disclosure', () => {
    const html = renderToStaticMarkup(<Overview />);
    const visible = html.replace(/<details\b[\s\S]*?<\/details>/g, '');
    expect(visible).toContain('Sonnet weekly');
    expect(visible).toContain('77%');
    expect(visible).toContain('Sonnet history: 23%');
    expect(html).not.toContain('additional model or bucket limits');
    expect(html).toContain('<summary class="cursor-pointer p-5 text-secondary">Provider permissions and credits</summary>');
  });

  it('keeps extra limits from other providers in their existing disclosure', () => {
    state.account!.provider = 'codex';
    state.account!.key = 'codex:one';
    const html = renderToStaticMarkup(<Overview />);
    expect(html).toContain('1 additional model or bucket limits');
    // The freshness tooltip can identify an unconfirmed extra window without
    // displaying its card outside the disclosure.
    const visibleText = html.replace(/<details\b[\s\S]*?<\/details>/g, '').replace(/<[^>]*>/g, '');
    expect(visibleText).not.toContain('Sonnet weekly');
  });
});
