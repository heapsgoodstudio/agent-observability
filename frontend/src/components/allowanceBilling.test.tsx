import { create } from '@bufbuild/protobuf';
import type { ReactNode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AccountSnapshotSchema, BillingSnapshotSchema, AllowanceWindowSchema, GetSpendResponseSchema, ReportSchema, SessionRowSchema } from '@/src/gen/claudeobs/v1/obs_pb';
import { BillingValue, WindowMeter } from './AccountUI';
import { AccountRow } from './pages/Overview';
import { UsageContent } from './pages/Spend';
import { PropertiesPanel } from './pages/SessionDetail';
import { ReportView } from './report/ReportView';
import { DEPTH_INFO } from '@/src/lib/advice';

const { renderToStaticMarkup } = require('react-dom/server') as { renderToStaticMarkup(node: ReactNode): string };
const selection = vi.hoisted(() => ({ profile: '' }));
vi.mock('@/src/lib/useProfile', async (original) => ({ ...await original<object>(), useProfile: () => ({ profile: selection.profile, provider: 'claude' }) }));
vi.mock('@/src/lib/scrollTarget', () => ({ useScrollTarget: () => {} }));
vi.mock('@/src/lib/useNow', () => ({ useNow: () => 2_000_000_000 }));
vi.mock('@/src/lib/client', () => ({ obsClient: {} }));
beforeEach(() => { selection.profile = ''; });

describe('included and unclassified usage has no dollar valuation', () => {
  it('renders the supplied account and its explicit provider billing', () => {
    const account = create(AccountSnapshotSchema, { key: 'claude:mine', provider: 'claude', displayName: 'My account', profiles: [{ profile: 'work' }], currentWindowId: 'five_hour', windows: [{ id: 'five_hour', usedPercent: 20, remainingPercent: 80 }], billing: { reportedOverageUsd: 4.25 } });
    const html = renderToStaticMarkup(<AccountRow account={account} />);
    expect(html).toContain('My account'); expect(html).toContain('80%'); expect(html).toContain('remaining'); expect(html).toContain('$4.25'); expect(html).toContain('Provider-reported overage');
  });
  it('keeps missing readings unknown instead of pricing included usage', () => {
    const account = create(AccountSnapshotSchema, { provider: 'codex', displayName: 'Unresolved connection', profiles: [{ profile: 'work' }], state: 'unknown' });
    const html = renderToStaticMarkup(<AccountRow account={account} />);
    expect(html).toContain('Unknown'); expect(html).toContain('Not provided'); expect(html).not.toContain('$'); expect(html).not.toContain('0%');
  });
  it('shows a measured zero without treating an absent field as zero', () => {
    const zero = create(AllowanceWindowSchema, { usedPercent: 0, remainingPercent: 100 });
    expect(renderToStaticMarkup(<WindowMeter window={zero} />)).toContain('100%');
    expect(renderToStaticMarkup(<WindowMeter window={create(AllowanceWindowSchema)} />)).toContain('Unknown');
    expect(renderToStaticMarkup(<BillingValue billing={create(BillingSnapshotSchema, { reportedOverageUsd: 0 })} />)).toContain('$0.00');
    expect(renderToStaticMarkup(<BillingValue />)).not.toContain('$');
  });
  it('leads with the used figure and puts remaining beside it', () => {
    const html = renderToStaticMarkup(<WindowMeter window={create(AllowanceWindowSchema, { usedPercent: 20, remainingPercent: 80 })} />);
    expect(html).toContain('<strong>20%</strong><span>used</span>'); expect(html).toContain('<small>80% remaining</small>');
  });
  it('shows Codex credit overage with its estimate named as one, or in credits when no price is set', () => {
    const codex = (billing: object) => create(AccountSnapshotSchema, { key: 'codex:a', provider: 'codex', displayName: 'Work codex', profiles: [{ profile: 'codex-default' }], billing: { state: 'fresh', period: 'Current credit period at observation', periodResetsAt: 2_000_000_000 + 11 * 86400 + 3600, note: 'Credits Codex reports as used under this member’s individual credit limit.', ...billing } });
    const priced = renderToStaticMarkup(<AccountRow account={codex({ reportedOverageCredits: 212.4375, estimatedOverageUsd: 8.4975, creditPriceUsd: 0.04 })} />);
    expect(priced).toContain('$8.50'); expect(priced).toContain('Estimated from 212.44 credits at $0.04 each'); expect(priced).toContain('Credit period resets in 11d 1h');
    expect(priced).not.toContain('Provider-reported overage');
    const unpriced = renderToStaticMarkup(<AccountRow account={codex({ reportedOverageCredits: 212.4375 })} />);
    expect(unpriced).toContain('212.44 credits'); expect(unpriced).toContain('no price per credit set'); expect(unpriced).not.toContain('$');
  });
  it('labels provider amounts and never substitutes an API estimate', () => {
    expect(renderToStaticMarkup(<BillingValue billing={create(BillingSnapshotSchema, { reportedOverageUsd: 4.25 })} />)).toContain('$4.25');
    const html = renderToStaticMarkup(<BillingValue billing={create(BillingSnapshotSchema, { estimatedOverageUsd: 999 })} />);
    expect(html).not.toContain('$999'); expect(html).toContain('Unknown');
  });

  it('uses token and delivery counts despite legacy monetary spend fields', () => {
    const response = create(GetSpendResponseSchema, { dailyCostTotalUsd: 12345, deliverySessionCostUsd: 999, deliveryPrs: 2,
      modelUsage: [{ model: 'claude-test', costUsd: 12345, input: 100, output: 200 }],
      deliveryRows: [{ project: 'work', prs: 2, sessionCostUsd: 999 }] });
    const html = renderToStaticMarkup(<UsageContent resp={response} periodLabel="7 days" />);
    expect(html).toContain('Input tokens');
    expect(html).toContain('Total tokens');
    expect(html).not.toContain('$');
    expect(html).not.toContain('12,345');
  });

  it.each(['claude', 'codex'])('never prices a %s session from its legacy cost fields', (provider) => {
    const row = create(SessionRowSchema, { provider, costUsd: 12345, costKnown: true, usageKnown: true, tokens: { input: 100, output: 200 }, models: [{ model: 'test-model', costUsd: 12345 }] });
    const html = renderToStaticMarkup(<PropertiesPanel row={row} />);
    expect(html).toContain('Tokens');
    expect(html).not.toContain('$');
    expect(html).not.toContain('12,345');
    expect(html).not.toContain('Per request');
  });

  it('gates old report prose and removes legacy driver dollars even in new reports', () => {
    const legacy = create(ReportSchema, { headline: 'Included usage worth $999', summary: 'Old estimate $888', costDrivers: [{ label: 'Tokens', amountUsd: 999 }] });
    const html = renderToStaticMarkup(<ReportView report={legacy} kind="analysis" />);
    expect(html).toContain('Generate a new report');
    expect(html).not.toContain('$');
    const current = create(ReportSchema, { allowanceAware: true, headline: 'Usage review', summary: 'The request used the allowance.', costDrivers: [{ label: 'Tokens', amountUsd: 999 }] });
    expect(renderToStaticMarkup(<ReportView report={current} kind="analysis" />)).not.toContain('$');
  });

  it('does not assign prices to advice runs that use plan allowance', () => {
    expect(JSON.stringify(DEPTH_INFO)).not.toContain('$');
  });

  it('does not describe a current report that fails accounting checks as predating the change', () => {
    const report = create(ReportSchema, { schema: 2, allowanceAware: false, headline: 'Unsupported valuation', summary: 'Included tokens worth $999' });
    const html = renderToStaticMarkup(<ReportView report={report} kind="analysis" />);
    expect(html).toContain('does not meet the usage-only accounting requirements');
    expect(html).not.toContain('predates');
    expect(html).not.toContain('$999');
  });
});

describe('pay-as-you-go estimates', () => {
  it('labels an omp session estimate', () => {
    const row = create(SessionRowSchema, { provider: 'omp', costUsd: 4.063371, costKnown: true, usageKnown: true, tokens: { input: 100, output: 200 } });
    const html = renderToStaticMarkup(<PropertiesPanel row={row} />);
    expect(html).toContain('Estimated cost');
    expect(html).toContain('$4.06');
  });

  it('shows an estimate only when the caller opts in, and labels it', () => {
    const billing = create(BillingSnapshotSchema, { estimatedOverageUsd: 9.68, period: 'Month to date (2026-09)', note: "omp's estimate: provider-reported tokens at public API rates, not a bill." });
    expect(renderToStaticMarkup(<BillingValue billing={billing} />)).not.toContain('$9.68');
    const html = renderToStaticMarkup(<BillingValue billing={billing} showEstimate />);
    expect(html).toContain('$9.68');
    expect(html).toContain('Estimated spend');
    expect(html).toContain('not a bill');
  });
});
