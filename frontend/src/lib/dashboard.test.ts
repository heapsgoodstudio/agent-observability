import { create } from '@bufbuild/protobuf';
import { describe, expect, it } from 'vitest';
import { AccountSnapshotSchema, AllowanceWindowSchema, BillingSnapshotSchema } from '@/src/gen/claudeobs/v1/obs_pb';
import { accountFreshness, confirmationAge, billingLabel, historySegments, matchesAccountScope, billedTotal, scopeHref, sidebarFigure, sidebarGroups, windowRoles, windowState } from './dashboard';

describe('account presentation invariants', () => {
  it('renders one reported limit when current and weekly share a window', () => {
    const account = create(AccountSnapshotSchema, { currentWindowId: 'weekly', weeklyWindowId: 'weekly', windows: [{ id: 'weekly', usedPercent: 0, remainingPercent: 100 }] });
    const roles = windowRoles(account);
    expect(roles.shared).toBe(true); expect(roles.current).toBe(roles.weekly); expect(roles.extra).toEqual([]);
  });
  it('keeps absent billing different from explicit reported zero and disabled', () => {
    expect(billingLabel(undefined)).toBe('Unknown');
    expect(billingLabel(create(BillingSnapshotSchema, { enabled: true }))).toBe('Amount unknown');
    expect(billingLabel(create(BillingSnapshotSchema, { enabled: false }))).toBe('Overage disabled');
    expect(billingLabel(create(BillingSnapshotSchema, { reportedOverageUsd: 0, enabled: false }))).toBe('$0.00');
    expect(billingLabel(create(BillingSnapshotSchema, { reportedOverageCredits: 212.4375 }))).toBe('212.44 credits');
    expect(billingLabel(create(BillingSnapshotSchema, { reportedOverageCredits: 212.4375, estimatedOverageUsd: 8.4975, creditPriceUsd: 0.04 }))).toBe('$8.50');
    expect(billingLabel(create(BillingSnapshotSchema, { reportedOverageCredits: 0, estimatedOverageUsd: 0, creditPriceUsd: 0.04 }))).toBe('$0.00');
  });
  it('does not infer allowance restored from a passed reset', () => {
    const window = create(AllowanceWindowSchema, { state: 'fresh', usedPercent: 100, remainingPercent: 0, resetsAt: 10, confirmedAt: 9 });
    expect(windowState(window, 11)).toBe('Awaiting reset confirmation'); expect(window.remainingPercent).toBe(0);
  });
  it('uses confirmation time independently from value change time', () => {
    const window = create(AllowanceWindowSchema, { state: 'fresh', observedAt: 10, confirmedAt: 990 });
    expect(windowState(window, 1000)).toBe('Confirmed'); expect(windowState(window, 1400)).toBe('Confirmed'); expect(windowState(window, 1590)).toBe('Confirmed'); expect(windowState(window, 1591)).toBe('Stale reading');
  });
  it('keeps period resets and explicitly missing intervals disconnected', () => {
    expect(historySegments([{ ts: 1, value: 10 }, { ts: 2, value: 20 }, { ts: 3, value: 0 }, { ts: 5, value: 15 }, { ts: 9, value: 20 }], [{ start: 6, end: 8 }]).map((s) => s.map((p) => p.ts))).toEqual([[1, 2], [3, 5], [9]]);
  });
  it('never widens mismatched provider/account/profile combinations', () => {
    const a = create(AccountSnapshotSchema, { key: 'claude:one', provider: 'claude', profiles: [{ profile: 'work' }] });
    expect(matchesAccountScope(a, { accountKey: 'claude:one', provider: 'codex', profile: 'work' })).toBe(false);
    expect(matchesAccountScope(a, { accountKey: 'claude:one', provider: 'claude', profile: 'other' })).toBe(false);
    expect(matchesAccountScope(a, { accountKey: 'claude:other', provider: 'claude', profile: 'work' })).toBe(false);
    expect(matchesAccountScope(a, { accountKey: 'claude:one', provider: '', profile: 'work' })).toBe(true);
  });
  it('encodes account identity and clears old scope from links', () => {
    expect(scopeHref('/insights?tab=advice&accountKey=old#report', { accountKey: 'codex:team/x', provider: 'codex', profile: '' })).toBe('/insights?tab=advice&accountKey=codex%3Ateam%2Fx&provider=codex#report');
    expect(scopeHref('/sessions?accountKey=old&profile=old', { accountKey: '', provider: '', profile: '' })).toBe('/sessions');
  });
});

describe('freshness scope and timestamp meaning', () => {
  const now = 2_000_000_000;
  it('names stale model windows and reports their confirmation instead of a newer current window', () => {
    const account = create(AccountSnapshotSchema, { provider: 'claude', state: 'stale', confirmedAt: now - 60, observedAt: now - 60, windows: [
      { id: 'current', state: 'fresh', source: 'statusline', confirmedAt: now - 60 },
      { id: 'weekly', state: 'fresh', source: 'statusline', confirmedAt: now - 60 },
      { id: 'model', state: 'stale', source: 'cache', confirmedAt: now - 660, observedAt: now - 2000 },
    ] });
    expect(accountFreshness(account, now)).toMatchObject({ state: 'stale', label: '1 of 3 windows stale', at: now - 660, timeLabel: 'last confirmed' });
  });
  it('does not promote a recent value change into a missing confirmation', () => {
    const window = create(AllowanceWindowSchema, { state: 'stale', source: 'cache', usedPercent: 0, observedAt: now - 60 });
    expect(windowState(window, now)).toBe('Confirmation unavailable');
    const account = create(AccountSnapshotSchema, { provider: 'claude', state: 'stale', confirmedAt: now - 60, windows: [window] });
    expect(accountFreshness(account, now)).toMatchObject({ state: 'unknown', label: 'Confirmation unknown', at: undefined });
  });
  it('uses the oldest confirmation when all windows are fresh', () => {
    const account = create(AccountSnapshotSchema, { provider: 'claude', state: 'fresh', confirmedAt: now - 10, windows: [
      { state: 'fresh', confirmedAt: now - 10 }, { state: 'fresh', confirmedAt: now - 240 },
    ] });
    expect(accountFreshness(account, now)).toMatchObject({ state: 'fresh', label: 'Confirmed', at: now - 240, timeLabel: 'oldest confirmation' });
  });
  it('keeps Codex stale beyond five minutes and makes the boundary visible without rounding', () => {
    const account = create(AccountSnapshotSchema, { provider: 'codex', state: 'fresh', windows: [{ state: 'fresh', source: 'codex', confirmedAt: now - 300 }] });
    expect(accountFreshness(account, now).label).toBe('Confirmed');
    expect(accountFreshness(account, now + 1).label).toBe('Stale');
    expect(confirmationAge(now - 300, now + 1, true)).toBe('5m 1s ago');
  });
});

describe('Oh My Pi accounts', () => {
  const now = 2_000_000_000;
  const omp = (over: object = {}) => create(AccountSnapshotSchema, { key: 'omp:baseten', provider: 'omp', account: 'baseten', state: 'fresh', confirmedAt: now - 240, capabilities: ['activity', 'estimated_spend'], billing: { estimatedOverageUsd: 9.685 }, ...over });

  it('say Collected, not Confirmed: no provider confirms a local collection', () => {
    const f = accountFreshness(omp(), now);
    expect(f.label).toBe('Collected');
    expect(f.timeLabel).toBe('last collected');
    expect(f.state).toBe('fresh');
    expect(accountFreshness(omp({ confirmedAt: now - 901 }), now).label).toBe('Stale');
    expect(accountFreshness(omp({ confirmedAt: undefined }), now).label).toBe('Awaiting collection');
    expect(accountFreshness(omp({ error: 'sessions directory unavailable' }), now).label).toBe('Collection failed');
  });

  it('show month-to-date spend in the sidebar where allowance accounts show % used', () => {
    expect(sidebarFigure(omp()).text).toBe('$9.69');
    expect(sidebarFigure(omp({ billing: { estimatedOverageUsd: 0.0035 } })).text).toBe('<$0.01');
    expect(sidebarFigure(omp({ billing: {} })).text).toBe('—');
    const claude = create(AccountSnapshotSchema, { provider: 'claude', state: 'fresh', currentWindowId: 'five_hour', windows: [{ id: 'five_hour', usedPercent: 26, remainingPercent: 74 }], billing: { estimatedOverageUsd: 50 } });
    expect(sidebarFigure(claude).text).toBe('26%');
  });
});

describe('opencode accounts', () => {
  it('read like Oh My Pi accounts: collected, with month-to-date spend in the sidebar', () => {
    const now = 2_000_000_000;
    const a = create(AccountSnapshotSchema, { key: 'opencode:baseten', provider: 'opencode', account: 'baseten', state: 'fresh', confirmedAt: now - 60, capabilities: ['activity', 'estimated_spend'], billing: { estimatedOverageUsd: 2.1 } });
    expect(accountFreshness(a, now).label).toBe('Collected');
    expect(accountFreshness(a, now).detail).toContain('opencode usage');
    expect(sidebarFigure(a).text).toBe('$2.10');
  });
});

describe('sidebar groups', () => {
  const account = (provider: string, key: string) => create(AccountSnapshotSchema, { provider, key });
  it('shows a configured Oh My Pi or opencode harness before it has any account', () => {
    const groups = sidebarGroups([account('claude', 'claude:a'), account('omp', 'omp:baseten')], [
      { provider: 'claude', enabled: true }, { provider: 'omp', enabled: true }, { provider: 'opencode', enabled: true },
    ]);
    expect(groups.map((g) => [g.provider, g.accounts.length])).toEqual([['claude', 1], ['omp', 1], ['opencode', 0]]);
  });
  it('adds no empty group for a disabled harness, or for Claude and Codex, whose profiles always have an account', () => {
    const groups = sidebarGroups([], [{ provider: 'opencode', enabled: false }, { provider: 'codex', enabled: true }, { provider: 'claude', enabled: true }]);
    expect(groups).toEqual([]);
  });
});

describe('accounts with no recorded cost', () => {
  it('show tokens in the sidebar, never an estimate', () => {
    const lms = create(AccountSnapshotSchema, { key: 'opencode:lms', provider: 'opencode', account: 'lms', state: 'fresh', capabilities: ['activity', 'token_usage'], monthToDateTokens: 3_132_530, billing: { estimatedOverageUsd: 0 } });
    expect(sidebarFigure(lms).text).toBe('3.1M tok');
    expect(sidebarFigure(create(AccountSnapshotSchema, { provider: 'opencode', state: 'fresh', capabilities: ['token_usage'] })).text).toBe('—');
  });
});

describe('hidden accounts in the sidebar', () => {
  it('are left out and counted, so an all-hidden group still says so', () => {
    const acct = (key: string, hidden = false) => create(AccountSnapshotSchema, { provider: 'opencode', key, hidden });
    const groups = sidebarGroups([acct('opencode:lms'), acct('opencode:w', true), acct('opencode:wire', true)], []);
    expect(groups).toHaveLength(1);
    expect(groups[0].accounts.map((a) => a.key)).toEqual(['opencode:lms']);
    expect(groups[0].hidden).toBe(2);
    const allHidden = sidebarGroups([acct('opencode:w', true)], []);
    expect(allHidden[0].accounts).toEqual([]);
    expect(allHidden[0].hidden).toBe(1);
  });
});

describe('stale sidebar figures', () => {
  it('keep the last known figure without sidebar staleness indicators', () => {
    const claude = (state: string) => create(AccountSnapshotSchema, { provider: 'claude', state, currentWindowId: 'five_hour', windows: [{ id: 'five_hour', usedPercent: 77, remainingPercent: 23 }] });
    expect(sidebarFigure(claude('stale'))).toMatchObject({ text: '77%', windows: [{ tone: 'warn' }] });
    expect(sidebarFigure(claude('stale'))).toEqual(sidebarFigure(claude('fresh')));
    expect(sidebarFigure(claude('fresh'))).toMatchObject({ text: '77%', windows: [{ tone: 'warn' }] });
    expect(sidebarFigure(claude('fresh')).title).not.toContain('Stale');
    expect(sidebarFigure(claude('unknown'))).toEqual({ text: '—' });
    const omp = create(AccountSnapshotSchema, { provider: 'omp', state: 'stale', capabilities: ['estimated_spend'], billing: { estimatedOverageUsd: 9.685 } });
    expect(sidebarFigure(omp)).toMatchObject({ text: '$9.69' });
    expect(sidebarFigure(omp).title).toBeUndefined();
  });
});

describe('sidebar allowance windows', () => {
  const windows = [
    { id: 'five_hour', label: '5-hour allowance', usedPercent: 30.4, remainingPercent: 69.6 },
    { id: 'seven_day', label: 'Weekly allowance', usedPercent: 9.8, remainingPercent: 90.2 },
    { id: 'weekly_scoped', label: 'Model weekly allowance', usedPercent: 88, remainingPercent: 12 },
  ];
  it('shows current then weekly when they are different windows, and names both in the title', () => {
    const a = create(AccountSnapshotSchema, { provider: 'claude', state: 'fresh', currentWindowId: 'five_hour', weeklyWindowId: 'seven_day', windows });
    const f = sidebarFigure(a);
    expect(f.text).toBe('30% / 10%');
    expect(f.title).toBe('5-hour allowance: 30% used\nWeekly allowance: 10% used');
  });
  it('shows one figure for one shared window', () => {
    const a = create(AccountSnapshotSchema, { provider: 'codex', state: 'fresh', currentWindowId: 'codex:primary', weeklyWindowId: 'codex:primary', windows: [{ id: 'codex:primary', label: 'Weekly allowance', usedPercent: 13, remainingPercent: 87 }] });
    expect(sidebarFigure(a)).toMatchObject({ text: '13%', title: 'Weekly allowance: 13% used', windows: [{ tone: 'normal' }] });
  });
  it('keeps the slot for a weekly window with no reading, rather than dropping it', () => {
    const a = create(AccountSnapshotSchema, { provider: 'claude', state: 'fresh', currentWindowId: 'five_hour', weeklyWindowId: 'seven_day', windows: [windows[0], { id: 'seven_day', label: 'Weekly allowance' }] });
    expect(sidebarFigure(a).text).toBe('30% / —');
  });
  it('shows a dash, with no title, when there is no current window', () => {
    const a = create(AccountSnapshotSchema, { provider: 'claude', state: 'fresh', currentWindowId: 'five_hour', windows: [] });
    expect(sidebarFigure(a)).toEqual({ text: '—' });
  });
});

describe('month-to-date billed total', () => {
  const claude = (name: string, billing: object) => create(AccountSnapshotSchema, { key: `claude:${name}`, provider: 'claude', account: name, displayName: name, billing });
  it('sums provider-reported overage across accounts and names each one', () => {
    const t = billedTotal([claude('acme', { enabled: true, reportedOverageUsd: 612.40, state: 'fresh' }), claude('studio', { enabled: true, reportedOverageUsd: 387.60, state: 'fresh' }), claude('personal', { enabled: false })]);
    expect(t?.text).toBe('$1,000.00');
    expect(t?.stale).toBe(false);
    expect(t?.title).toContain('acme: $612.40');
    expect(t?.title).toContain('studio: $387.60');
    expect(t?.title).toContain('Extra usage off: personal');
  });
  it('includes Baseten and Fireworks estimates across harnesses, including hidden accounts', () => {
    const omp = create(AccountSnapshotSchema, { key: 'omp:baseten', provider: 'omp', displayName: 'Baseten', hidden: true, billing: { estimatedOverageUsd: 9.69 } });
    const opencode = create(AccountSnapshotSchema, { key: 'opencode:fireworks', provider: 'opencode', displayName: 'Fireworks', billing: { estimatedOverageUsd: 2.31 } });
    const t = billedTotal([claude('acme', { enabled: true, reportedOverageUsd: 10, state: 'fresh' }), omp, opencode]);
    expect(t?.text).toBe('≈$22.00');
    expect(t?.estimated).toBe(true);
    expect(t?.title).toContain('Baseten: $9.69 (estimated)');
    expect(t?.title).toContain('Fireworks: $2.31 (estimated)');
    expect(billedTotal([omp])?.text).toBe('≈$9.69');
  });
  it('uses a reported amount in preference to an estimate and counts a shared account once', () => {
    const a = claude('shared', { reportedOverageUsd: 10, estimatedOverageUsd: 99 });
    expect(billedTotal([a, a])?.text).toBe('$10.00');
  });
  it('does not attach a dollar value to unclassified subscription usage', () => {
    const codex = create(AccountSnapshotSchema, { key: 'codex:a', provider: 'codex', billing: { estimatedOverageUsd: 99, state: 'unavailable' } });
    expect(billedTotal([claude('acme', { reportedOverageUsd: 10 }), codex])?.text).toBe('$10.00');
  });
  it('adds reported Codex credits at the configured price as an estimate, and marks unpriced credits partial', () => {
    const codex = (billing: object) => create(AccountSnapshotSchema, { key: 'codex:a', provider: 'codex', displayName: 'Work codex', billing: { state: 'fresh', ...billing } });
    const t = billedTotal([claude('acme', { reportedOverageUsd: 10 }), codex({ reportedOverageCredits: 212.4375, estimatedOverageUsd: 8.4975, creditPriceUsd: 0.04 })]);
    expect(t?.text).toBe('≈$18.50');
    expect(t?.estimated).toBe(true);
    expect(t?.title).toContain('Work codex: $8.50 (estimated from 212.44 credits)');
    expect(t?.title).toContain('Codex credits priced at CODEX_CREDIT_USD');
    expect(t?.title).not.toContain('harness estimates');
    const unpriced = billedTotal([claude('acme', { reportedOverageUsd: 10 }), codex({ reportedOverageCredits: 212.4375 })]);
    expect(unpriced?.text).toBe('$10.00+');
    expect(unpriced?.title).toContain('Work codex: 212.44 credits, no price per credit set');
  });
  it('marks incomplete estimates and accounts with missing billed amounts as partial', () => {
    const omp = create(AccountSnapshotSchema, { key: 'omp:baseten', provider: 'omp', billing: { estimatedOverageUsd: 9, estimatedOverageComplete: false } });
    expect(billedTotal([omp])?.text).toBe('≈$9.00+');
    const t = billedTotal([claude('known', { reportedOverageUsd: 10 }), claude('missing', { enabled: true })]);
    expect(t?.text).toBe('$10.00+');
    expect(t?.title).toContain('missing: amount unknown');
  });
  it('keeps sub-cent spend visible', () => {
    const a = create(AccountSnapshotSchema, { key: 'omp:fireworks', provider: 'omp', billing: { estimatedOverageUsd: 0.0035 } });
    expect(billedTotal([a])?.text).toBe('≈<$0.01');
  });
  it('marks stale pay-as-you-go collections as stale', () => {
    const a = create(AccountSnapshotSchema, { key: 'omp:baseten', provider: 'omp', state: 'stale', billing: { estimatedOverageUsd: 9 } });
    expect(billedTotal([a])?.stale).toBe(true);
  });
  it('flags the total when any reading in it is stale', () => {
    const t = billedTotal([claude('acme', { enabled: true, reportedOverageUsd: 5, state: 'stale' }), claude('studio', { enabled: true, reportedOverageUsd: 5, state: 'fresh' })]);
    expect(t?.stale).toBe(true);
    expect(t?.title).toContain('acme: $5.00 (stale reading)');
  });
  it('shows a reported zero as $0.00, since that account can spend and has not', () => {
    expect(billedTotal([claude('acme', { enabled: true, reportedOverageUsd: 0, state: 'fresh' })])?.text).toBe('$0.00');
  });
  it('says Off, not $0.00, when every account has extra usage switched off', () => {
    expect(billedTotal([claude('personal', { enabled: false }), claude('spare', { enabled: false })])).toEqual({ text: 'Off', stale: false, estimated: false, title: 'Extra usage off: personal, spare' });
  });
  it('says Unknown when overage is on but nothing has reported an amount', () => {
    expect(billedTotal([claude('acme', { enabled: true })])?.text).toBe('Unknown');
  });
  it('shows nothing with no billing information at all', () => {
    expect(billedTotal([claude('acme', {})])).toBeNull();
    expect(billedTotal([])).toBeNull();
  });
});

