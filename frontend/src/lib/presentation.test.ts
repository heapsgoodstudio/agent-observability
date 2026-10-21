import { describe, expect, it } from 'vitest';
import { fmtClock } from './format';
import {
  BILLED_STALE_AFTER_S,
  STALE_AFTER_S,
  billingPill,
  findingsBadgeLabel,
  freshnessPill,
  groupHealthChecks,
  healthAriaLabel,
  healthDotState,
  healthUpdatedAt,
  isBilledStale,
  isStale,
  modelUsageMeta,
  planWindowSub,
} from './presentation';

describe('isStale', () => {
  it('is false when the age is unknown', () => {
    expect(isStale(1e9, false)).toBe(false);
  });
  it('is false at or under the threshold', () => {
    expect(isStale(STALE_AFTER_S, true)).toBe(false);
    expect(isStale(10, true)).toBe(false);
  });
  it('is true past the threshold', () => {
    expect(isStale(STALE_AFTER_S + 1, true)).toBe(true);
  });
});

describe('isBilledStale', () => {
  it('is false when unknown', () => {
    expect(isBilledStale(1e9, false)).toBe(false);
  });
  it('is false at or under the threshold', () => {
    expect(isBilledStale(BILLED_STALE_AFTER_S, true)).toBe(false);
  });
  it('is true past the threshold', () => {
    expect(isBilledStale(BILLED_STALE_AFTER_S + 1, true)).toBe(true);
  });
});

describe('planWindowSub', () => {
  it('reports no sample when the window carries no percentage', () => {
    expect(planWindowSub({ hasPct: false, hasReset: false, resetTs: 0 })).toBe('no sample from the poller');
  });
  it('reports the reset not being carried when pct is known but reset is not', () => {
    expect(planWindowSub({ hasPct: true, hasReset: false, resetTs: 0 })).toBe('reset time not reported');
  });
  it('counts down when both are known', () => {
    const now = 1000;
    expect(planWindowSub({ hasPct: true, hasReset: true, resetTs: now + 3600 }, now)).toBe('resets in 1h 0m');
  });
});

describe('modelUsageMeta', () => {
  it('returns null when the model has no token history at all', () => {
    expect(modelUsageMeta({ output: 0, cacheRead: 0, input: 0, cacheCreation: 0, costUsd: 5 })).toBeNull();
  });
  it('builds the output/cached line without a per-M rate when output is zero', () => {
    expect(modelUsageMeta({ output: 0, cacheRead: 100, input: 0, cacheCreation: 0, costUsd: 5 })).toBe('0 output, 100% cached');
  });
  it('includes the per-M-output rate when output is positive', () => {
    // 1,000,000 output tokens, 1,000,000 cache-read tokens (50% cached), $10 cost
    // -> $10 / (1e6 / 1e6) = $10 per M output.
    const meta = modelUsageMeta({ output: 1e6, cacheRead: 1e6, input: 0, cacheCreation: 0, costUsd: 10 });
    expect(meta).toBe('1.0M output, 50% cached, $10.00 per M output');
  });
});

describe('freshnessPill', () => {
  it('is neutral with a note when unknown', () => {
    const p = freshnessPill(0, false);
    expect(p.tone).toBe('neutral');
    expect(p.text).toBe('plan data age unknown');
    expect(p.note).toBe('the limits poller has not written claude_plan_data_age_seconds');
  });
  it('is ok and carries no note when fresh', () => {
    const p = freshnessPill(10, true);
    expect(p.tone).toBe('ok');
    expect(p.text).toBe('plan data fresh, 10s old');
    expect(p.note).toBeUndefined();
  });
  it('is warn when stale', () => {
    const p = freshnessPill(STALE_AFTER_S + 60, true);
    expect(p.tone).toBe('warn');
    expect(p.text).toContain('plan data stale');
  });
});

describe('billingPill', () => {
  it('is neutral with a note when overage state is unknown', () => {
    const p = billingPill(false, false);
    expect(p.tone).toBe('neutral');
    expect(p.headline).toBe('allowance state unknown');
    expect(p.note).toBe('No recent allowance observation is available.');
  });
  it('warns of exhaustion without claiming a request was charged', () => {
    const p = billingPill(true, true);
    expect(p.tone).toBe('warn');
    expect(p.headline).toBe('allowance exhausted');
    expect(p.note).toContain('does not establish that a request was charged');
  });
  it('reports headroom without guaranteeing the next request is free', () => {
    const p = billingPill(false, true);
    expect(p.tone).toBe('neutral');
    expect(p.headline).toBe('below reported limits');
    expect(p.note).toContain('requires provider evidence');
  });
});

describe('groupHealthChecks', () => {
  it('splits a flat checks array into the page\'s panel groupings by name', () => {
    const checks = [
      { name: 'VictoriaMetrics', state: 'ok', detail: 'x' },
      { name: 'VictoriaLogs', state: 'ok', detail: 'x' },
      { name: 'Analysis runner', state: 'ok', detail: 'x' },
      { name: 'plan data, cache', state: 'ok', detail: 'x' },
      { name: 'plan data, statusline', state: 'ok', detail: 'x' },
      { name: 'session index', state: 'ok', detail: 'x' },
      { name: 'claude_in_overage', state: 'ok', detail: 'x' },
      { name: 'claude_extra_usage_usd', state: 'ok', detail: 'x' },
      { name: 'claude_plan_window_pct', state: 'ok', detail: 'x' },
      { name: 'claude_code.cost.usage', state: 'ok', detail: 'x' },
      { name: 'claude_code.token.usage', state: 'ok', detail: 'x' },
    ];
    const groups = groupHealthChecks(checks);
    expect(groups.stores.map((c) => c.name)).toEqual(['VictoriaMetrics', 'VictoriaLogs', 'Analysis runner']);
    expect(groups.poller.map((c) => c.name)).toEqual(['plan data, cache', 'plan data, statusline']);
    expect(groups.indexer.map((c) => c.name)).toEqual(['session index']);
    expect(groups.series.map((c) => c.name)).toEqual([
      'claude_in_overage',
      'claude_extra_usage_usd',
      'claude_plan_window_pct',
      'claude_code.cost.usage',
      'claude_code.token.usage',
    ]);
  });
  it('also groups the no-sample "plan data" fallback row', () => {
    const groups = groupHealthChecks([{ name: 'plan data', state: 'warn', detail: 'no samples' }]);
    expect(groups.poller).toHaveLength(1);
  });
  it('retains profile, collection, and empty-install checks rather than silently hiding failures', () => {
    const checks = [
      { name: 'Work (codex)', state: 'warn', detail: 'Telemetry settings conflict' },
      { name: 'Work session collection', state: 'bad', detail: 'Could not read session history' },
      { name: 'profile settings', state: 'bad', detail: 'Registry unavailable' },
      { name: 'profiles', state: 'unknown', detail: 'Add a profile in Settings.' },
    ];
    expect(groupHealthChecks(checks).profiles).toEqual(checks);
  });
});

describe('healthUpdatedAt', () => {
  it('returns null when generated_at is absent', () => {
    expect(healthUpdatedAt('')).toBeNull();
  });
  it('returns null when generated_at does not parse as a date', () => {
    expect(healthUpdatedAt('not-a-timestamp')).toBeNull();
  });
  it('formats a valid RFC 3339 timestamp with the "updated <clock>" wording', () => {
    const iso = '2026-08-16T03:04:05Z';
    expect(healthUpdatedAt(iso)).toBe(`updated ${fmtClock.format(new Date(iso))}`);
  });
});

describe('findingsBadgeLabel', () => {
  // The Findings nav badge shows the plain count and is hidden at zero. The
  // cached count starts at 0, so "no refresh has completed yet" and
  // "checked, nothing loud to report" deliberately read the same: hidden.
  it('is hidden (null) at zero', () => {
    expect(findingsBadgeLabel(0)).toBeNull();
  });
  it('is the count as a string when positive', () => {
    expect(findingsBadgeLabel(3)).toBe('3');
  });
});

describe('healthDotState', () => {
  // The dot stays neutral ("unknown") until a check has actually resolved,
  // then asserts "ok" for true or "bad" for false. Never both at once, and
  // never "bad" merely because nothing has loaded yet.
  it('is unknown before any check has resolved', () => {
    expect(healthDotState(null)).toBe('unknown');
  });
  it('is ok when the check reports healthy', () => {
    expect(healthDotState(true)).toBe('ok');
  });
  it('is bad when the check reports unhealthy', () => {
    expect(healthDotState(false)).toBe('bad');
  });
});

describe('healthAriaLabel', () => {
  // The label is set only once the health state is known. The dot itself is
  // aria-hidden (a colour swatch, not a control, carrying no name of its
  // own), so this label is what keeps its state from being conveyed by
  // colour alone.
  it('is undefined before any check has resolved', () => {
    expect(healthAriaLabel(null)).toBeUndefined();
  });
  it('reads "Health, ok" when healthy', () => {
    expect(healthAriaLabel(true)).toBe('Health, ok');
  });
  it('reads "Health, degraded" when unhealthy', () => {
    expect(healthAriaLabel(false)).toBe('Health, degraded');
  });
});

describe('freshnessPill, confirmation age', () => {
  // The bug this exists for: Claude Code does not rewrite its usage cache when
  // a fetch comes back identical, so a profile at a steady percentage ages
  // past the stale cutoff while its numbers are correct to the second.
  // Measured 2026-09-21 at 44 minutes unchanged against a 6 second old tap
  // that agreed with it.
  it('calls a long-unchanged figure fresh when it was just confirmed', () => {
    const p = freshnessPill(2640, true, 120, true);
    expect(p.tone).toBe('ok');
    expect(p.text).toContain('fresh');
    expect(p.text).toContain('confirmed');
  });

  // The control: the same change age with no confirmation is still stale, so
  // the test above cannot pass by the pill having stopped judging anything.
  it('still calls it stale when nothing confirmed it', () => {
    const p = freshnessPill(2640, true, undefined, false);
    expect(p.tone).toBe('warn');
    expect(p.text).toContain('stale');
  });

  it('is stale when the confirmation itself is old', () => {
    const p = freshnessPill(60, true, 3600, true);
    expect(p.tone).toBe('warn');
    expect(p.text).toContain('stale');
  });

  // The change age is still worth saying, it just stops being the verdict.
  it('reports the change age second, as information', () => {
    expect(freshnessPill(2640, true, 120, true).text).toContain('unchanged for');
  });

  // A confirmation with no change age to pair it with says only what it knows.
  it('omits the change age when it is unknown', () => {
    const p = freshnessPill(0, false, 120, true);
    expect(p.text).not.toContain('unchanged for');
    expect(p.tone).toBe('ok');
  });

  it('keeps the old wording when nothing has ever confirmed', () => {
    expect(freshnessPill(60, true).text).toBe('plan data fresh, 60s old');
  });
});
