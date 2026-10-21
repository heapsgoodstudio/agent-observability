import { describe, expect, it } from 'vitest';
import type { AccountPlan } from '@/src/gen/claudeobs/v1/obs_pb';
import {
  accountLabel,
  billedCaption,
  feedsText,
  selectAccount,
  showPicker,
  windowsFor,
} from './accounts';

const acct = (over: Partial<AccountPlan> = {}): AccountPlan =>
  ({
    account: 'aaaa1111',
    displayName: 'acme',
    domain: 'work.example',
    profiles: ['acme'],
    windows: [],
    maxPct: 0,
    hasMaxPct: true,
    overageEnabled: false,
    overageKnown: true,
    extraUsageUsd: 0,
    extraUsageKnown: false,
    inOverage: false,
    planDataAgeS: 0,
    ageKnown: true,
    ...over,
  }) as AccountPlan;

const money = (v: number) => `$${v.toFixed(2)}`;

describe('selectAccount', () => {
  it('prefers the selection', () => {
    const a = acct({ account: 'a' });
    const b = acct({ account: 'b' });
    expect(selectAccount([a, b], 'b', 'a')?.account).toBe('b');
  });

  it('falls back to the primary when the selection is gone', () => {
    const a = acct({ account: 'a' });
    const b = acct({ account: 'b' });
    expect(selectAccount([a, b], 'vanished', 'b')?.account).toBe('b');
  });

  // The server sorts accounts highest-percentage first, so falling through to
  // the first one lands on whichever account is closest to throttling rather
  // than on an arbitrary account.
  it('falls back to the first when neither matches', () => {
    const a = acct({ account: 'a' });
    const b = acct({ account: 'b' });
    expect(selectAccount([a, b], null, 'nope')?.account).toBe('a');
  });

  it('returns null with no accounts', () => {
    expect(selectAccount([], null, '')).toBeNull();
  });
});

describe('accountLabel', () => {
  // The percentage belongs on the label: a picker of bare names hides the one
  // number that matters until each option is opened in turn, which is exactly
  // how an account at 94% goes unnoticed.
  it('carries the highest window percentage', () => {
    expect(accountLabel(acct({ displayName: 'personal', maxPct: 94.4, hasMaxPct: true }))).toBe('personal 94%');
  });

  it('omits it when no window reported one', () => {
    expect(accountLabel(acct({ displayName: 'default', hasMaxPct: false }))).toBe('default');
  });
});

describe('billedCaption', () => {
  it('retains reported historical overage after extra usage is disabled', () => {
    const account = acct({ overageEnabled: false, overageKnown: true, extraUsageKnown: true, extraUsageUsd: 25 });
    expect(billedCaption(account, money)).toBe('Provider-reported overage $25.00 month to date; extra usage currently disabled');
  });
  // The case this function exists for. An account with extra usage off cannot
  // be billed at all, and "$0.00" reads as "spent nothing" rather than
  // "cannot spend".
  it('says overage is off rather than showing zero dollars', () => {
    expect(billedCaption(acct({ overageEnabled: false, overageKnown: true }), money)).toBe(
      'Extra usage is currently disabled',
    );
  });

  it('shows the figure when overage is on', () => {
    const a = acct({ overageEnabled: true, overageKnown: true, extraUsageKnown: true, extraUsageUsd: 878.93 });
    expect(billedCaption(a, money)).toBe('Provider-reported overage $878.93 month to date');
  });

  it('distinguishes enabled-but-unreported from zero', () => {
    const a = acct({ overageEnabled: true, overageKnown: true, extraUsageKnown: false });
    expect(billedCaption(a, money)).toBe('overage enabled, no billed figure yet');
  });

  it('asserts nothing when the poller has not said', () => {
    expect(billedCaption(acct({ overageKnown: false }), money)).toBeNull();
  });
});

describe('feedsText', () => {
  // Two profiles on one account is the case the whole account grouping exists
  // for, and the only case where naming them adds anything.
  it('names the profiles when several share an account', () => {
    expect(feedsText(acct({ profiles: ['studio', 'default'] }))).toBe('feeds: studio, default');
  });

  it('stays quiet for a single profile', () => {
    expect(feedsText(acct({ profiles: ['acme'] }))).toBeNull();
  });
});

describe('windowsFor', () => {
  it('prefers the account windows', () => {
    const w = [{ window: 'five_hour' }] as AccountPlan['windows'];
    expect(windowsFor(acct({ windows: w }), [])).toBe(w);
  });

  // An install whose poller predates the account labels reports no accounts
  // at all, and must still render its meters from the flat list.
  it('falls back to the flat list with no account', () => {
    const flat = [{ window: 'seven_day' }] as AccountPlan['windows'];
    expect(windowsFor(null, flat)).toBe(flat);
  });
});

describe('showPicker', () => {
  it('is hidden for one account and shown for two', () => {
    expect(showPicker([acct()])).toBe(false);
    expect(showPicker([acct({ account: 'a' }), acct({ account: 'b' })])).toBe(true);
  });
});
