import type { Report, ReportAction, ReportDriver } from '@/src/gen/claudeobs/v1/obs_pb';
import { money } from './format';

// Pure helpers for the structured report (components/report/ReportView.tsx and
// AnalysisSummaryCard.tsx): tone and wording maps for the bands, the bar
// geometry for the money breakdown, and the per-kind section headings. No DOM,
// no fetching, no module state.
//
// Every band arrives already validated: internal/report blanks a value outside
// its set before it reaches the wire, so a lookup that misses here means "the
// model did not say", and the caller omits the chip rather than printing an
// unknown word.

// A report renders only when it has the two fields every surface is built
// from. internal/report enforces the same rule, and this agreeing with it is
// the point: a response can carry an older markdown report instead, and that
// is what the fallback rendering is for.
//
// A type guard rather than a plain boolean, so a caller writes
// `hasReport(raw) ? raw : null` once and every later use is already narrowed,
// instead of repeating `hasReport(r) && r` at each render site.
export function hasReport(r?: Report): r is Report {
  return !!r && r.headline.trim() !== '' && r.summary.trim() !== '';
}

// The three Badge tones a report is allowed to use, declared here rather than
// imported: nothing else in src/lib reaches into components, and this subset is
// itself the rule. Badge reserves "danger" for real-dollar states across this
// design system, and a verdict is a judgement, not a billed figure, so a type
// that cannot express danger enforces that rather than leaving it to review.
// ReportTone is assignable to BadgeTone wherever a component passes one on.
export type ReportTone = 'neutral' | 'ok' | 'warn';

export const VERDICT_TONE: Record<string, ReportTone> = {
  efficient: 'ok',
  mixed: 'warn',
  wasteful: 'warn',
};

export const COST_TONE: Record<string, ReportTone> = {
  small: 'neutral',
  moderate: 'warn',
  large: 'warn',
};

export function verdictLabel(verdict: string): string | null {
  return verdict in VERDICT_TONE ? verdict : null;
}

const AVOIDABLE_TEXT: Record<string, string> = {
  none: 'nothing here looks avoidable',
  small: 'a small part of this looks avoidable',
  moderate: 'a fair part of this looks avoidable',
  large: 'a large part of this looks avoidable',
};

export function avoidableText(avoidable: string): string | null {
  return AVOIDABLE_TEXT[avoidable] ?? null;
}

const COST_LABEL: Record<string, string> = {
  small: 'small usage impact',
  moderate: 'moderate usage impact',
  large: 'large usage impact',
};

export function costLabel(cost: string): string | null {
  return COST_LABEL[cost] ?? null;
}

const EFFORT_TEXT: Record<string, string> = {
  low: 'low effort',
  medium: 'some effort',
  high: 'high effort',
};

const SAVING_TEXT: Record<string, string> = {
  small: 'saves a little',
  moderate: 'saves some',
  large: 'saves a lot',
};

// The caption beside one action: whichever halves the model supplied, joined.
export function actionMeta(a: ReportAction): string | null {
  const parts = [EFFORT_TEXT[a.effort], SAVING_TEXT[a.saving]].filter((p): p is string => !!p);
  return parts.length ? parts.join(' · ') : null;
}

export interface DriverRow {
  label: string;
  // Bar width, 0 to 100, scaled so the largest driver fills the track.
  pct: number;
  // The true share as text, which is not the same number as pct.
  share: string;
  amount: string;
  plain: string;
}

// Bars scale to the largest share rather than to 100, so a session whose
// biggest driver is 40% still reads as a shape rather than a row of stubs. The
// printed percentage stays the real share. Drivers whose shares are all zero (a
// report that named labels but no numbers) still list, with empty tracks,
// rather than vanishing.
export function driverRows(drivers: ReportDriver[]): DriverRow[] {
  const max = drivers.reduce((m, d) => Math.max(m, d.sharePct), 0);
  return drivers.map((d) => ({
    label: d.label,
    pct: max > 0 ? (d.sharePct / max) * 100 : 0,
    share: `${Math.round(d.sharePct)}%`,
    amount: d.amountUsd > 0 ? money(d.amountUsd) : '',
    plain: d.plain,
  }));
}

// The one action the session Overview card has room for.
export function topAction(r: Report): ReportAction | null {
  return r.actions[0] ?? null;
}

export interface ReportHeadings {
  story: string;
  drivers: string;
  problems: string;
  good: string;
  actions: string;
  config: string;
  fine: string;
}

// One report shape, two readings. An analysis speaks about one session in the
// past; advice speaks about a habit across many.
export const HEADINGS: Record<'analysis' | 'advice', ReportHeadings> = {
  analysis: {
    story: 'What this session was doing',
    drivers: 'Where usage went',
    problems: 'What used the allowance',
    good: 'What went well',
    actions: 'Do this next',
    config: 'Settings worth changing',
    fine: 'This is fine',
  },
  advice: {
    story: 'What your usage looks like',
    drivers: 'Where usage goes',
    problems: 'What uses the allowance',
    good: 'What is working',
    actions: 'Do this next',
    config: 'Settings worth changing',
    fine: 'This is fine',
  },
};
