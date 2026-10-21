import { create } from '@bufbuild/protobuf';
import type { ReactNode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { GetMonitoringResponseSchema } from '@/src/gen/claudeobs/v1/obs_pb';
import { CodexMonitoring, ProviderPage } from './CodexMonitoring';

const { renderToStaticMarkup } = require('react-dom/server') as { renderToStaticMarkup(node: ReactNode): string };
const state = vi.hoisted(() => ({ provider: '', profile: '', data: null as unknown, error: null as string | null }));
vi.mock('@/src/lib/useProfile', async (original) => ({
  ...await original<object>(),
  useProfile: () => ({ provider: state.provider, profile: state.profile }),
}));
vi.mock('@/src/lib/usePoll', () => ({ usePoll: () => ({ data: state.data, error: state.error }) }));
vi.mock('@/src/lib/client', () => ({ obsClient: {} }));

beforeEach(() => {
  state.provider = '';
  state.profile = '';
  state.error = null;
  state.data = create(GetMonitoringResponseSchema, { sessions: 2, configuredProfiles: 1 });
});

describe('provider page isolation', () => {
  const ClaudePanel = () => <p>Claude billing marker</p>;

  it('does not mount Claude content in a Codex-only view', () => {
    state.provider = 'codex';
    const html = renderToStaticMarkup(<ProviderPage title="Spend"><ClaudePanel /></ProviderPage>);
    expect(html).not.toContain('Claude billing marker');
    expect(html).toContain('Spend · Codex');
    expect(html).toContain('All indexed history');
  });

  it('keeps the provider summaries separate in the all-provider view', () => {
    const html = renderToStaticMarkup(<ProviderPage title="Spend"><ClaudePanel /></ProviderPage>);
    expect(html).toContain('Claude billing marker');
    expect(html).toContain('All indexed history');
    expect(html).toContain('Included and unclassified usage has no dollar valuation');
  });

  it('does not mount Codex monitoring in a Claude-only view', () => {
    state.provider = 'claude';
    const html = renderToStaticMarkup(<ProviderPage title="Spend"><ClaudePanel /></ProviderPage>);
    expect(html).toContain('Claude billing marker');
    expect(html).not.toContain('Codex monitoring');
  });
});

describe('Codex unknown and partial telemetry', () => {
  it('does not present missing pricing as a zero dollar estimate', () => {
    const html = renderToStaticMarkup(<CodexMonitoring profile="" />);
    expect(html).toContain('Unavailable');
    expect(html).toContain('Token history is incomplete');
    expect(html).not.toContain('$0.00');
    expect(html).toContain('Plan utilization is unknown');
  });

  it('shows one specific coverage message and links to the affected sessions', () => {
    state.data = create(GetMonitoringResponseSchema, {
      sessions: 221, tokens: { input: 123 }, usageGaps: [{ sessionId: 'codex:work:interrupted', profile: 'work', profileName: 'Work', startedAt: 123, reason: 'Interrupted without recorded token usage; the amount used is unknown.' }],
    });
    const html = renderToStaticMarkup(<CodexMonitoring profile="work" />);
    expect(html).toContain('Token totals available for 220 of 221 sessions. 1 session has unknown usage.');
    expect(html).toContain('Live plan limits are fetched separately');
    expect(html).toContain('View affected sessions');
    expect(html).toContain('Interrupted without recorded token usage');
    expect(html).toContain('/sessions?id=codex%3Awork%3Ainterrupted&amp;profile=work&amp;provider=codex');
    expect(html).toContain('Reimporting cannot recreate token records');
    expect(html).not.toContain('Usage totals are incomplete');
    expect(html).not.toContain('$0.00');
  });

  it('does not turn unknown usage, tools, errors, or duration into definitive zero totals', () => {
    state.data = create(GetMonitoringResponseSchema, { sessions: 1, configuredProfiles: 1, tokens: {} });
    const html = renderToStaticMarkup(<CodexMonitoring profile="" />);
    expect(html).toContain('Requests: Unavailable');
    expect(html).toContain('Recorded errors: Unavailable');
    expect(html).toContain('Session duration: Unavailable');
    expect(html).toContain('Input: Unavailable');
    expect(html).not.toContain('>0<');
    expect(html).not.toContain('0 recorded errors');
  });

  it('keeps positive partial activity totals useful and visibly incomplete', () => {
    state.data = create(GetMonitoringResponseSchema, {
      sessions: 1, toolCalls: 7, errorCount: 2, durationMs: 120000, requests: 3, tokens: { input: 15 },
    });
    const html = renderToStaticMarkup(<CodexMonitoring profile="" />);
    expect(html).toContain('>7+<');
    expect(html).toContain('Recorded errors: 2+');
    expect(html).toContain('Requests: 3+');
    expect(html).toContain('>15+<');
    expect(html).toContain('Session duration: 2m+');
  });

  it('ignores legacy all-usage dollar estimates while retaining source errors and history', () => {
    state.data = create(GetMonitoringResponseSchema, {
      sessions: 2, estimatedCostUsd: 5, costComplete: false,
      errors: [{ section: 'codex-history', error: 'One local record could not be read' }],
    });
    const html = renderToStaticMarkup(<CodexMonitoring profile="" />);
    expect(html).not.toContain('$5.00');
    expect(html).toContain('Provider-reported overage');
    expect(html).toContain('Estimated outside-cap usage');
    expect(html).toContain('One local record could not be read');
    expect(html).toContain('Preserved history appears below');
  });

  it('promotes account allowance, remaining capacity, reset times and credits without pricing them', () => {
    const now = Math.floor(Date.now() / 1000);
    state.data = create(GetMonitoringResponseSchema, {
      accounts: [{ profile: 'work', name: 'Work', account: 'account-1', planType: 'pro', fetchedAt: now, ordinaryUsageAllowed: true,
        resetCreditsAvailable: 2, buckets: [{ limitId: 'codex', name: 'Codex allowance', credits: { hasCredits: true, balance: '300.50' }, spendControlReached: false }] }],
      windows: [{ profile: 'work', account: 'account-1', name: 'Weekly allowance', usedPercent: 96, resetsAt: now + 3600, fetchedAt: now }],
      estimatedCostUsd: 98765, costComplete: true, models: [{ model: 'gpt', estimatedCostUsd: 98765 }],
    });
    const html = renderToStaticMarkup(<CodexMonitoring profile="" />);
    expect(html).toContain('Account account-1');
    expect(html).toContain('Included usage allowed');
    expect(html).toContain('Fresh snapshot');
    expect(html).toContain('4% remaining');
    expect(html).toContain('resets in');
    expect(html).toContain('(local time)');
    expect(html).toContain('300.50 credits');
    expect(html).toContain('Available reset credits: 2');
    expect(html).not.toContain('$');
    expect(html).not.toContain('98,765');
  });

  it('renders only explicit outside-cap amounts and keeps reported and estimated values separate', () => {
    state.data = create(GetMonitoringResponseSchema, {
      estimatedCostUsd: 98765, billing: { reportedOverageUsd: 4.25, estimatedOverageUsd: 1.50, pricingSourceUrl: 'https://openai.com/api/pricing/', pricingCheckedAt: '2026-09-22', outsideCapRequests: 2 },
    });
    const html = renderToStaticMarkup(<CodexMonitoring profile="" />);
    expect(html).toContain('$4.25');
    expect(html).toContain('$1.50');
    expect(html).toContain('API pricing source');
    expect(html).toContain('2 requests explicitly identified as outside the cap');
    expect(html).not.toContain('98,765');
  });

  it('shows reported credits as overage and prices them only at the configured rate', () => {
    state.data = create(GetMonitoringResponseSchema, { billing: { reportedOverageCredits: 15, estimatedOverageUsd: 0.6, creditPriceUsd: 0.04 } });
    const html = renderToStaticMarkup(<CodexMonitoring profile="" />);
    expect(html).toContain('15.00 credits');
    expect(html).toContain('$0.60');
    expect(html).toContain('Reported credits at $0.04 each (CODEX_CREDIT_USD)');
    expect(html).not.toContain('API pricing source');
  });

  it('does not imply live headroom, allowed usage, or automatic renewal after a stale or failed refresh', () => {
    const now = Math.floor(Date.now() / 1000);
    state.error = 'Offline';
    state.data = create(GetMonitoringResponseSchema, {
      accounts: [{ profile: 'work', name: 'Work', planType: 'self_serve_business_prolite', fetchedAt: now, ordinaryUsageAllowed: true }],
      windows: [{ profile: 'work', name: 'Weekly allowance', usedPercent: 96, fetchedAt: now, resetsAt: now - 5 }],
    });
    const html = renderToStaticMarkup(<CodexMonitoring profile="" />);
    expect(html).toContain('Stale snapshot');
    expect(html).toContain('Last observed: 4% remaining');
    expect(html).toContain('current status unconfirmed');
    expect(html).toContain('awaiting fresh confirmation');
    expect(html).toContain('Business');
    expect(html).not.toContain('self_serve_business_prolite');
    expect(html).not.toContain('Fresh snapshot');
    expect(html).not.toContain('resetting now');
  });

  it('counts reasoning within output rather than adding it twice', () => {
    state.data = create(GetMonitoringResponseSchema, {
      sessions: 2, costComplete: true, usageComplete: true,
      tokens: { input: 100, output: 200, cacheRead: 300 }, reasoningTokens: 150,
    });
    const html = renderToStaticMarkup(<CodexMonitoring profile="" />);
    expect(html).toContain('>600<');
    expect(html).toContain('included in output');
    expect(html).not.toContain('>750<');
  });
});
