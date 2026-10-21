import { describe, expect, it } from 'vitest';
import { ageText, agoText, compactInt, countdown, moneyShort, reportedAt, shortModel, shortProject, untilText } from './format';

describe('moneyShort', () => {
  it('switches unit at ten and a thousand', () => {
    expect(moneyShort(0)).toBe('$0');
    expect(moneyShort(2.5)).toBe('$2.50');
    expect(moneyShort(9.994)).toBe('$9.99');
    expect(moneyShort(42)).toBe('$42');
    expect(moneyShort(1500)).toBe('$1.5k');
    expect(moneyShort(-1500)).toBe('$-1.5k');
  });
});

describe('compactInt', () => {
  it('abbreviates from a hundred thousand up', () => {
    expect(compactInt(0)).toBe('0');
    expect(compactInt(99999)).toBe('99,999');
    expect(compactInt(150000)).toBe('150k');
    expect(compactInt(1500000)).toBe('1.5M');
    expect(compactInt(2669988408)).toBe('2.67B');
    expect(compactInt('not a number')).toBe('0');
  });
});

describe('ageText', () => {
  it('picks a unit per magnitude', () => {
    expect(ageText(45)).toBe('45s');
    expect(ageText(90)).toBe('2m');
    expect(ageText(5400)).toBe('1.5h');
    expect(ageText(172800)).toBe('2.0d');
  });
});

// agoText/untilText are the prose half of the split described in format.ts:
// dayjs phrasing for the human "as of" captions, countdown below for the plan
// window gauges that need the exact minute.
describe('agoText', () => {
  it('reads as prose rather than a unit suffix', () => {
    const now = 1000000;
    expect(agoText(now - 240, now)).toBe('4 minutes ago');
    expect(agoText(now - 10, now)).toBe('a few seconds ago');
    expect(agoText(now - 60, now)).toBe('a minute ago');
  });

  it('keeps reporting minutes past the 45 dayjs would round away', () => {
    // Untuned, dayjs calls anything over 44 minutes "an hour ago". A billed
    // figure 45 minutes stale and one 89 minutes stale are different facts on
    // this dashboard, so the threshold moves out to match ageText's own 90
    // minute breakpoint.
    const now = 1000000;
    expect(agoText(now - 45 * 60, now)).toBe('45 minutes ago');
    expect(agoText(now - 89 * 60, now)).toBe('89 minutes ago');
    expect(agoText(now - 91 * 60, now)).toBe('2 hours ago');
  });

  it('keeps reporting hours through two days, as ageText does', () => {
    const now = 1000000;
    expect(agoText(now - 30 * 3600, now)).toBe('30 hours ago');
    expect(agoText(now - 50 * 3600, now)).toBe('2 days ago');
  });
});

describe('untilText', () => {
  it('phrases a future instant the same way, forwards', () => {
    const now = 1000000;
    expect(untilText(now + 240, now)).toBe('in 4 minutes');
    expect(untilText(now + 3 * 3600, now)).toBe('in 3 hours');
    expect(untilText(now + 50 * 3600, now)).toBe('in 2 days');
  });
});

describe('reportedAt', () => {
  // The subtle one. The server answers "this figure was 300s old when I
  // built the response", and that age is only true at the instant it landed.
  // Anchoring it to that instant is what lets a one-second clock age it
  // without refetching.
  it('anchors a server-reported age so a ticking clock ages it', () => {
    const fetchedAt = 1000000;
    const at = reportedAt(300, fetchedAt);

    expect(ageText(fetchedAt - at)).toBe('5m');
    expect(ageText(fetchedAt + 600 - at)).toBe('15m');
    expect(agoText(at, fetchedAt + 600)).toBe('15 minutes ago');
  });

  // Recomputing `now - ageS` on every tick instead is the trap: both terms
  // advance together, so the caption reads 5m forever and the staleness the
  // page exists to surface never appears.
  it('does not hold the age constant as now advances', () => {
    const fetchedAt = 1000000;
    const at = reportedAt(300, fetchedAt);
    const early = ageText(fetchedAt - at);
    const later = ageText(fetchedAt + 3600 - at);
    expect(early).not.toBe(later);
  });
});

describe('countdown', () => {
  it('counts down and reports a passed reset', () => {
    const now = 1000000;
    expect(countdown(now - 5, now)).toBe('resetting now');
    expect(countdown(now + 300, now)).toBe('resets in 5m');
    expect(countdown(now + 3900, now)).toBe('resets in 1h 5m');
    expect(countdown(now + 90000, now)).toBe('resets in 1d 1h');
  });

  it('calls a long-passed reset stale rather than imminent', () => {
    // The live failure: a reset 113 minutes in the past, from a source hours
    // out of date, read as though the window were about to roll.
    const now = 1000000;
    expect(countdown(now - 299, now)).toBe('resetting now');
    expect(countdown(now - 301, now)).toBe('reset time stale');
    expect(countdown(now - 113 * 60, now)).toBe('reset time stale');
  });
});

describe('shortModel', () => {
  it('strips the prefix and the date suffix', () => {
    expect(shortModel('claude-haiku-4-5-20251001')).toBe('haiku-4-5');
    expect(shortModel('claude-opus-5')).toBe('opus-5');
    expect(shortModel('')).toBe('unknown');
  });

  it('collapses the context variant onto the plain model', () => {
    // The metrics store says one, the transcripts say the other. A merged
    // breakdown must key them together or one model becomes two rows.
    expect(shortModel('claude-opus-5[1m]')).toBe(shortModel('claude-opus-5'));
    expect(shortModel('claude-sonnet-5[1m]')).toBe('sonnet-5');
    // Only a trailing bracket, and only a context-sized one: a model whose
    // real name ends in brackets elsewhere keeps them.
    expect(shortModel('claude-opus-5[1m]-preview')).toBe('opus-5[1m]-preview');
  });
});

describe('shortProject', () => {
  it('strips only a leading home path', () => {
    expect(shortProject('-Users-alice-Projects-app')).toBe('Projects-app');
    expect(shortProject('Claude')).toBe('Claude');
    expect(shortProject('')).toBe('');
  });
});
