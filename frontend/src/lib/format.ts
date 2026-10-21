// Formatting. Pure: no DOM, no fetching, no mutable module state (the one
// module-level statement is dayjs.extend below, a deterministic one-time
// plugin registration).
import dayjs from 'dayjs';
import relativeTime from 'dayjs/plugin/relativeTime';

const DAY = 86400;

// Two idioms for relative time, deliberately, split by what the reader needs.
//
// agoText/untilText below are prose ("45 minutes ago", "in 3 hours") and are
// what the human-facing captions use: how stale the billed figure is, when
// advice was written, how long a session ran. countdown further down stays
// numeric ("resets in 3h 12m") because a plan window gauge is a thing you
// steer by, and "in about 3 hours" is not good enough to decide whether to
// keep working. Do not collapse the two: the precision difference is the
// whole reason both exist.
//
// The thresholds are tuned rather than left at dayjs's defaults, which round
// anything past 44 minutes to "an hour" and anything past 21 hours to "a
// day". The boundaries here (90 minutes, 48 hours) are ageText's own, so the
// prose and the numeric forms change unit at the same moment instead of
// disagreeing about whether something is minutes or hours old.
dayjs.extend(relativeTime, {
  thresholds: [
    { l: 's', r: 44, d: 'second' },
    { l: 'm', r: 89 },
    { l: 'mm', r: 89, d: 'minute' },
    { l: 'h', r: 89 },
    { l: 'hh', r: 47, d: 'hour' },
    { l: 'd', r: 35 },
    { l: 'dd', r: 25, d: 'day' },
    { l: 'M', r: 45 },
    { l: 'MM', r: 10, d: 'month' },
    { l: 'y', r: 17 },
    { l: 'yy', d: 'year' },
  ],
});

const fmtUsd = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD' });
export const fmtInt = new Intl.NumberFormat('en-AU');
// en-GB rather than en-AU: en-AU abbreviates some months and not others
// ("4 Aug" next to "5 July"), and 24 hour clocks read better in a console.
export const fmtDay = new Intl.DateTimeFormat('en-GB', { month: 'short', day: 'numeric' });
export const fmtClock = new Intl.DateTimeFormat('en-GB', { hour: '2-digit', minute: '2-digit', hour12: false });

export const nowSec = (): number => Math.floor(Date.now() / 1000);
export const money = (v: number): string => fmtUsd.format(v);

export function moneyShort(v: number): string {
  if (v === 0) return '$0';
  if (Math.abs(v) >= 1000) return `$${(v / 1000).toFixed(1)}k`;
  if (Math.abs(v) >= 10) return `$${v.toFixed(0)}`;
  return `$${v.toFixed(2)}`;
}

// Abbreviated integers for stat tiles, where "2,669,988,408" does not fit the
// column. Callers pass the exact figure as the tile's title.
export function compactInt(v: unknown): string {
  const n = Number(v) || 0;
  const a = Math.abs(n);
  if (a >= 1e9) return `${(n / 1e9).toFixed(2)}B`;
  if (a >= 1e6) return `${(n / 1e6).toFixed(1)}M`;
  if (a >= 1e5) return `${Math.round(n / 1e3)}k`;
  return fmtInt.format(n);
}

export function ageText(s: number): string {
  if (s < 90) return `${Math.round(s)}s`;
  if (s < 5400) return `${Math.round(s / 60)}m`;
  if (s < 2 * DAY) return `${(s / 3600).toFixed(1)}h`;
  return `${(s / DAY).toFixed(1)}d`;
}

// dayjs's from() reads the direction off the sign of the difference, so both
// directions are one implementation. The two exported names exist so call
// sites read correctly ("written {agoText(t)}" against "{untilText(t)}"), not
// because the behavior differs; calling either with a timestamp on the wrong
// side of `now` still phrases it correctly.
function relative(tsSec: number, now: number): string {
  return dayjs.unix(tsSec).from(dayjs.unix(now));
}

// "45 minutes ago". Both take an explicit `now` for the same reason countdown
// does: the caller owns the clock, so a ticking component can re-render these
// every second without either function reaching for Date.now itself.
export function agoText(tsSec: number, now: number = nowSec()): string {
  return relative(tsSec, now);
}

// "in 3 hours".
export function untilText(tsSec: number, now: number = nowSec()): string {
  return relative(tsSec, now);
}

// Turns a server-reported age into the fixed instant it refers to.
//
// Several RPCs report "this figure was N seconds old when I answered"
// (GetOverview's billed_cache_age_s, plan_data_age_s) rather than a
// timestamp. That age is only true at the moment the response landed, so
// anchoring it to that moment is what lets a one-second clock age it onward
// without refetching.
//
// The trap this avoids is recomputing `now - ageS` on every tick: both terms
// advance together, so the caption reads "5 minutes" forever and the
// staleness these panels exist to surface never shows up. Pinned in
// format.test.ts.
export function reportedAt(ageS: number, fetchedAt: number): number {
  return fetchedAt - ageS;
}

// A reset that has just passed is a real state, for the minute or two between
// the boundary and the next poll. One that passed long ago is not: it means
// the reading is stale, and calling that "resetting now" is what made a six
// hour old cache look like a window perpetually about to roll. Anything past
// the grace window says so instead.
const RESET_GRACE_S = 300;

export function countdown(resetTs: number, now: number = nowSec()): string {
  const s = resetTs - now;
  if (s <= -RESET_GRACE_S) return 'reset time stale';
  if (s <= 0) return 'resetting now';
  const d = Math.floor(s / DAY);
  const h = Math.floor((s % DAY) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d) return `resets in ${d}d ${h}h`;
  if (h) return `resets in ${h}h ${m}m`;
  return `resets in ${m}m`;
}

// claude-haiku-4-5-20251001 -> haiku-4-5
// The two stores spell one model two ways: the OTel counter reports the
// context variant, "claude-opus-5[1m]", and the transcripts the backfill
// parses report "claude-opus-5". A breakdown that merges both would list the
// same model twice, half its money under each spelling, so the suffix is
// stripped here, where every panel already canonicalizes. The distinction is
// not recoverable from history at all, which is why this collapses rather
// than trying to carry it.
export const shortModel = (name: unknown): string => String(name)
  .replace(/^claude-/, '').replace(/-\d{8}$/, '').replace(/\[\d+[a-z]\]$/, '') || 'unknown';
// -Users-alice-Projects-app -> Projects-app
export const shortProject = (name: unknown): string => String(name).replace(/^-Users-[^-]+-/, '') || String(name);
