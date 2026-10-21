// Pure presentation helpers for the Overview, Health and Shell
// (components/pages/{Overview,Health}.tsx, components/Shell.tsx). The API
// (proto/claudeobs/v1/obs.proto) returns these values raw: an age in seconds
// plus a "known" bool, a flat checks[] array, a plain badge_count/healthy,
// rather than a resolved fact. The threshold decision is therefore made here
// instead of being invented from other fields. No DOM, no fetching, no
// module state.
import type { HealthCheck, ModelUsageRow, PlanWindow } from '../gen/claudeobs/v1/obs_pb';
import type { BadgeTone } from '../components/ui/Badge';
import { ageText, compactInt, countdown, fmtClock, money } from './format';
import { CLAUDE_FRESH_FOR_S } from './allowance';

// Plan data older than this is flagged stale. Spelled a second time as
// staleAfterS in internal/server/health.go, which resolves the same rule for
// GetHealth; the two must change together or Overview and Health disagree. GetOverviewResponse carries the raw plan_data_age_s/age_known
// rather than a resolved "stale" bool (unlike GetHealthResponse's poller
// rows, which health.go already resolves server-side), so the threshold
// itself is ported here.
export const STALE_AFTER_S = CLAUDE_FRESH_FOR_S;

export function isStale(ageS: number, known: boolean): boolean {
  return known && ageS > STALE_AFTER_S;
}

// "billed figure ... stale, run /usage to refresh" appears once the cached
// /usage figure is more than 6 hours old. This one has no server-side twin:
// nothing resolves "billed figure stale" for us.
export const BILLED_STALE_AFTER_S = 21600;

export function isBilledStale(ageS: number, known: boolean): boolean {
  return known && ageS > BILLED_STALE_AFTER_S;
}

// The caption beneath each plan window: "no sample from the poller" when the
// window carries no percentage at all,
// "reset time not reported" when it has a percentage but no reset
// timestamp, otherwise the countdown to that reset.
export function planWindowSub(w: Pick<PlanWindow, 'hasPct' | 'hasReset' | 'resetTs'>, now?: number): string {
  if (!w.hasPct) return 'no sample from the poller';
  if (!w.hasReset) return 'reset time not reported';
  return countdown(w.resetTs, now);
}

// The per-model usage line under a by-model attribution row, e.g. "12.3k
// output, 40% cached, $12.50 per M output". null means no token history for
// this model: say nothing, since "0%" would read as a real measurement.
export function modelUsageMeta(
  row: Pick<ModelUsageRow, 'output' | 'cacheRead' | 'input' | 'cacheCreation' | 'costUsd'>,
): string | null {
  const total = row.output + row.cacheRead + row.input + row.cacheCreation;
  if (!total) return null;
  const parts = [`${compactInt(row.output)} output`, `${Math.round((100 * row.cacheRead) / total)}% cached`];
  if (row.output > 0) parts.push(`${money(row.costUsd / (row.output / 1e6))} per M output`);
  return parts.join(', ');
}

export interface FreshnessPill {
  tone: BadgeTone;
  text: string;
  note?: string;
}

/**
 * The freshness pill shown beside a plan figure.
 *
 * Two ages, and the verdict comes from the second one.
 *
 * `ageS` is how long since the figures last CHANGED, which is what Claude
 * Code's own fetchedAtMs records. `confirmedAgeS` is how long since a live
 * fetch last CONFIRMED them. They are not the same question, because Claude
 * Code does not rewrite its cache when a fetch comes back identical: a profile
 * sitting at a steady percentage ages past the stale cutoff indefinitely while
 * its numbers are correct to the second. Measured 2026-09-21, one profile read
 * 44 minutes old against a live probe and a 6 second old statusline tap that
 * both agreed with it to the point.
 *
 * So the badge says "confirmed 2m ago" and calls that fresh, and mentions the
 * change age second, where it reads as information rather than as a warning.
 * With nothing ever confirmed (a signed-out profile, or the probe disabled) it
 * falls back to the change age and the old wording, which is the honest answer
 * when nobody has checked.
 */
export function freshnessPill(
  ageS: number,
  known: boolean,
  confirmedAgeS?: number,
  confirmedKnown?: boolean,
): FreshnessPill {
  if (confirmedKnown && confirmedAgeS !== undefined) {
    const stale = isStale(confirmedAgeS, true);
    const changed = known ? `, unchanged for ${ageText(ageS)}` : '';
    return {
      tone: stale ? 'warn' : 'ok',
      text: `${stale ? 'plan data stale' : 'plan data fresh'}, confirmed ${ageText(confirmedAgeS)} ago${changed}`,
    };
  }
  if (!known) {
    return {
      tone: 'neutral',
      text: 'plan data age unknown',
      note: 'the limits poller has not written claude_plan_data_age_seconds',
    };
  }
  const stale = isStale(ageS, known);
  return {
    tone: stale ? 'warn' : 'ok',
    text: `${stale ? 'plan data stale' : 'plan data fresh'}, ${ageText(ageS)} old`,
  };
}

export interface BillingPill {
  tone: BadgeTone;
  headline: string;
  note: string;
}

// Legacy helper name retained for callers. A cap observation describes
// allowance, never whether any particular request incurred a charge.
export function billingPill(inOverage: boolean, known: boolean): BillingPill {
  if (!known) {
    return { tone: 'neutral', headline: 'allowance state unknown', note: 'No recent allowance observation is available.' };
  }
  return inOverage
    ? {
        tone: 'warn',
        headline: 'allowance exhausted',
        note: 'A reported plan window is exhausted; this does not establish that a request was charged.',
      }
    : { tone: 'neutral', headline: 'below reported limits', note: 'Billing classification requires provider evidence.' };
}

// The Health page shows six panels (Store reachability, Poller freshness,
// Session indexer, Expected series, Log volume, Retention), but
// GetHealthResponse's buildHealth emits one flat checks[] array. This splits
// it back into those groupings by exact name rather than by array position,
// since position would silently break if the server ever reordered them.
// Log volume and Retention are not HealthCheck rows in the proto (see
// GetHealthResponse's separate log_volume_24h/vm_retention/vl_retention
// fields), so they are not part of this grouping.
const STORE_NAMES = new Set(['VictoriaMetrics', 'VictoriaLogs', 'Analysis runner']);
const SERIES_NAMES = new Set([
  'claude_in_overage',
  'claude_extra_usage_usd',
  'claude_plan_window_pct',
  'claude_code.cost.usage',
  'claude_code.token.usage',
]);

// Pick<HealthCheck, ...> rather than the full branded HealthCheck: callers
// (and this module's own tests) only ever need the three plain fields, and
// a proto Message's own generated type carries a required $typeName brand
// that a hand-built test fixture has no reason to construct.
type CheckLike = Pick<HealthCheck, 'name' | 'state' | 'detail'>;

export interface HealthGroups {
  stores: CheckLike[];
  poller: CheckLike[];
  indexer: CheckLike[];
  series: CheckLike[];
  profiles: CheckLike[];
}

export function groupHealthChecks(checks: CheckLike[]): HealthGroups {
  const groups: HealthGroups = { stores: [], poller: [], indexer: [], series: [], profiles: [] };
  for (const c of checks) {
    if (STORE_NAMES.has(c.name)) groups.stores.push(c);
    else if (c.name === 'plan data' || c.name.startsWith('plan data, ')) groups.poller.push(c);
    else if (c.name === 'session index') groups.indexer.push(c);
    else if (SERIES_NAMES.has(c.name)) groups.series.push(c);
    // Profile names are user-defined. Keep every remaining check visible,
    // including no-profile guidance and future checks from the server.
    else groups.profiles.push(c);
  }
  return groups;
}

// The Health page shows "updated <clock>" but no "N of M panels failing"
// count: GetHealthResponse carries no per-call failing-panel count, by design
// (see the comment in components/pages/Health.tsx).
// GetHealthResponse.identity.generated_at is an RFC 3339
// timestamp set fresh on every response (internal/server/identity.go), which
// carries the same fact this one call needs, "when was this response built".
// Returns null when the field is absent or unparseable, so a caller can skip
// rendering the affordance entirely rather than showing "Invalid Date".
export function healthUpdatedAt(generatedAt: string): string | null {
  if (!generatedAt) return null;
  const d = new Date(generatedAt);
  if (Number.isNaN(d.getTime())) return null;
  return `updated ${fmtClock.format(d)}`;
}

// A top-level RPC call failing outright (the whole page has nothing to
// render, not just one panel) needs a plain string to show.
export function describeErr(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/* Sidebar badge and dot ------------------------------------------------------
   The Findings nav item's live count and the Health nav item's live status
   dot. Shell.tsx polls both and only updates its cached value on success, so
   a failed poll leaves the sidebar showing whatever it last knew rather than
   resetting to zero or unknown. That is why these take the *previous*
   resolved value (or null/0 for "nothing resolved yet"), not the raw RPC
   response. */

// Zero (both the initial value and a real "nothing loud to report") and
// "hidden" read the same, so this returns null rather than "0" to let a
// caller skip rendering the badge element entirely.
export function findingsBadgeLabel(count: number): string | null {
  return count > 0 ? String(count) : null;
}

export type HealthDotState = 'unknown' | 'ok' | 'bad';

// The dot stays neutral until a health check has actually resolved, then
// asserts "ok" for true or "bad" for false. null is the pre-resolution state:
// "unknown" must be visually distinct from "bad", or a page that has not
// finished loading looks broken.
export function healthDotState(healthy: boolean | null): HealthDotState {
  if (healthy === null) return 'unknown';
  return healthy ? 'ok' : 'bad';
}

// The dot itself is aria-hidden (it is a colour swatch, not a control, and
// carries no name of its own), so this label, applied to the whole nav link,
// is what keeps the dot's state from being conveyed by colour alone. Returns
// undefined (no aria-label attribute) before any check has resolved, leaving
// the link's accessible name as its plain "Health" text.
export function healthAriaLabel(healthy: boolean | null): string | undefined {
  if (healthy === null) return undefined;
  return `Health, ${healthy ? 'ok' : 'degraded'}`;
}
