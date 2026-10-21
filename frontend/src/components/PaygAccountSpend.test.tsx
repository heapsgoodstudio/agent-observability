import { create } from '@bufbuild/protobuf';
import type { ReactNode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AccountSnapshotSchema, GetPayAsYouGoResponseSchema } from '@/src/gen/claudeobs/v1/obs_pb';
import { PaygAccountSpend } from './PaygAccountSpend';

const { renderToStaticMarkup } = require('react-dom/server') as { renderToStaticMarkup(node: ReactNode): string };
const state = vi.hoisted(() => ({ data: null as unknown, error: null as string | null }));
vi.mock('@/src/lib/usePoll', () => ({ usePoll: () => ({ data: state.data, error: state.error }) }));
vi.mock('@/src/lib/client', () => ({ obsClient: {} }));
vi.mock('@/src/lib/useNow', () => ({ useNow: () => 2_000_000_000 }));

const paid = ['activity', 'estimated_spend'];
const baseten = create(AccountSnapshotSchema, { key: 'omp:baseten', provider: 'omp', account: 'baseten', displayName: 'Baseten', capabilities: paid });
const response = (over: object = {}) => create(GetPayAsYouGoResponseSchema, {
  configuredProfiles: 1, month: '2026-09',
  providers: [{
    provider: 'baseten', costRecorded: true, monthToDateUsd: 9.68, monthToDateComplete: true, last30DaysUsd: 9.68, last30DaysComplete: true, lastActiveDay: '2026-09-03',
    days: [{ day: '2026-09-02', costUsd: 7.37, requests: 10, tokens: 100 }, { day: '2026-09-03', costUsd: 0.43, requests: 2, tokens: 10 }],
    models: [{ model: 'glm-5.3', costUsd: 6.49, costComplete: true, requests: 230, input: 370000, output: 577000, cacheRead: 24500000 }],
    ...over,
  }],
});

beforeEach(() => { state.error = null; state.data = null; });

describe('pay-as-you-go account spend', () => {
  it("shows the account's own provider in the shared panel, chart and table styles", () => {
    state.data = response();
    const html = renderToStaticMarkup(<PaygAccountSpend account={baseten} />);
    expect(html).toContain('$9.68');
    expect(html).toContain('glm-5.3');
    expect(html).toContain('$6.49');
    expect(html).toContain('No allowance. Baseten bills per token.');
    expect(html).toContain('not a bill');
    expect(html).toContain('class="obs-chart"');
    expect(html).toContain('class="obs-data-table"');
    expect(html).not.toContain('viewBox'); // the scaled canvas made labels huge
  });

  it('shows a sub-cent month as spend, and incomplete pricing as a lower bound', () => {
    state.data = response({ monthToDateUsd: 0.0035, last30DaysUsd: 0.0035, last30DaysComplete: false });
    const html = renderToStaticMarkup(<PaygAccountSpend account={baseten} />);
    // The month-to-date stat, not the chart's $0.00 axis baseline.
    expect(html).toContain('<div class="obs-billing-value">&lt;$0.01</div>');
    expect(html).toContain('<div class="obs-billing-value">&lt;$0.01+</div>');
  });

  it("does not show another provider's spend", () => {
    state.data = response();
    const fireworks = create(AccountSnapshotSchema, { key: 'omp:fireworks', provider: 'omp', account: 'fireworks', displayName: 'Fireworks AI', capabilities: paid });
    const html = renderToStaticMarkup(<PaygAccountSpend account={fireworks} />);
    expect(html).not.toContain('$9.68');
    expect(html).toContain('No usage recorded for this provider.');
  });

  it("names the account's own harness in the estimate caption", () => {
    state.data = response();
    const viaOpencode = create(AccountSnapshotSchema, { key: 'opencode:baseten', provider: 'opencode', account: 'baseten', displayName: 'Baseten', capabilities: paid });
    expect(renderToStaticMarkup(<PaygAccountSpend account={viaOpencode} />)).toContain('opencode&#x27;s estimate');
    expect(renderToStaticMarkup(<PaygAccountSpend account={baseten} />)).toContain('Oh My Pi&#x27;s estimate');
  });
  it('shows a provider with no recorded cost by its tokens, never as spend', () => {
    state.data = create(GetPayAsYouGoResponseSchema, { configuredProfiles: 1, month: '2026-09', providers: [{
      provider: 'lms', costRecorded: false, monthToDateUsd: 0, monthToDateComplete: true, monthToDateTokens: 3_132_530, last30DaysTokens: 20_246_808, lastActiveDay: '2026-09-08',
      days: [{ day: '2026-09-01', costUsd: 0, requests: 100, tokens: 3_132_530 }],
      models: [{ model: 'qwen3.8-27b-fast', costUsd: 0, costComplete: true, requests: 210, input: 3_000_000, output: 50_000, cacheRead: 0 }],
    }] });
    const lms = create(AccountSnapshotSchema, { key: 'opencode:lms', provider: 'opencode', account: 'lms', displayName: 'LM Studio', capabilities: ['activity', 'token_usage'] });
    const html = renderToStaticMarkup(<PaygAccountSpend account={lms} />);
    expect(html).toContain('Token usage');
    expect(html).toContain('Tokens this month');
    expect(html).toContain('3.1M tok');
    expect(html).toContain('recorded no cost');
    expect(html).not.toContain('Estimated spend');
    expect(html).toContain('<td>$0.00</td>'); // the model table still shows its $0 price
  });
});

