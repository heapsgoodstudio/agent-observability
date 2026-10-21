import { describe, expect, it } from 'vitest';
import type { AccountPlan, ProfileRow } from '@/src/gen/claudeobs/v1/obs_pb';
import {
  ALL_PROFILES,
  accountOf,
  cardCaption,
  cardsFor,
  ignoresSelectionNote,
  normalizeProfile,
  pctLabel,
  reportsWindows,
  selectedProfile,
  siblingProfiles,
  usageNote,
} from './profiles';

const row = (over: Partial<ProfileRow> = {}): ProfileRow =>
  ({
    profile: 'acme',
    account: '5e6f7a8b',
    domain: 'work.example',
    maxPct: 23,
    hasMaxPct: true,
    usageAvailable: true,
    usageAvailableKnown: true,
    isPrimary: false,
    ...over,
  }) as ProfileRow;

const acct = (over: Partial<AccountPlan> = {}): AccountPlan =>
  ({
    account: '5e6f7a8b',
    displayName: 'acme',
    domain: 'work.example',
    profiles: ['acme'],
    windows: [],
    maxPct: 23,
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

// The machine this was built against: studio and default share one
// account, and the other three are separate. Every rule below is checked
// against that shape, because a shared account is the case the whole file
// exists for.
const ROSTER: ProfileRow[] = [
  row({ profile: 'personal', account: '9c0d1e2f', domain: 'personal.example', maxPct: 95 }),
  row({ profile: 'studio', account: '1a2b3c4d', domain: 'studio.example', maxPct: 49 }),
  row({ profile: 'default', account: '1a2b3c4d', domain: 'studio.example', maxPct: 49, usageAvailable: false }),
  row({ profile: 'acme', account: '5e6f7a8b', domain: 'work.example', maxPct: 23, isPrimary: true }),
];

describe('normalizeProfile', () => {
  it('passes a known profile through', () => {
    expect(normalizeProfile('studio', ROSTER)).toBe('studio');
  });

  it('treats null and empty as every profile', () => {
    expect(normalizeProfile(null, ROSTER)).toBe(ALL_PROFILES);
    expect(normalizeProfile('', ROSTER)).toBe(ALL_PROFILES);
  });

  // A stale bookmark naming a profile that has been removed should show the
  // whole dashboard, not a filter that matches nothing and reads as broken.
  it('falls back to every profile for a name the roster does not know', () => {
    expect(normalizeProfile('deleted', ROSTER)).toBe(ALL_PROFILES);
  });
});

describe('accountOf and siblingProfiles', () => {
  it('resolves a profile to its account', () => {
    expect(accountOf(ROSTER, 'acme')).toBe('5e6f7a8b');
  });

  it('gives two profiles on one account the same account', () => {
    expect(accountOf(ROSTER, 'studio')).toBe(accountOf(ROSTER, 'default'));
  });

  it('is empty for every profile and for an unknown name', () => {
    expect(accountOf(ROSTER, ALL_PROFILES)).toBe('');
    expect(accountOf(ROSTER, 'nope')).toBe('');
  });

  it('names the profiles sharing an allowance, and only the others', () => {
    expect(siblingProfiles(ROSTER, 'studio')).toEqual(['default']);
    expect(siblingProfiles(ROSTER, 'default')).toEqual(['studio']);
  });

  it('is empty for a profile that shares with nobody', () => {
    expect(siblingProfiles(ROSTER, 'acme')).toEqual([]);
  });
});

describe('selectedProfile', () => {
  it('finds the row', () => {
    expect(selectedProfile(ROSTER, 'personal')?.maxPct).toBe(95);
  });

  it('is null for every profile', () => {
    expect(selectedProfile(ROSTER, ALL_PROFILES)).toBeNull();
  });
});

describe('pctLabel', () => {
  it('rounds the account percentage', () => {
    expect(pctLabel(row({ maxPct: 48.6 }))).toBe('49%');
  });

  // A zero would read as an idle account when the truth is an account nothing
  // has measured. Same rule the delivery counters follow.
  it('is null rather than zero when nothing reported', () => {
    expect(pctLabel(row({ hasMaxPct: false, maxPct: 0 }))).toBeNull();
  });
});

describe('usageNote and reportsWindows', () => {
  it('says so when the profile cannot fetch usage at all', () => {
    expect(usageNote(row({ usageAvailableKnown: true, usageAvailable: false })))
      .toBe('not signed in, cannot report plan windows');
    expect(reportsWindows(row({ usageAvailableKnown: true, usageAvailable: false }))).toBe(false);
  });

  it('says nothing for a working profile', () => {
    expect(usageNote(row({ usageAvailableKnown: true, usageAvailable: true }))).toBeNull();
  });

  // Not yet probed is a different state from cannot fetch: it resolves on the
  // next poll, so asserting either answer would be inventing one.
  it('says nothing when no probe has been read', () => {
    expect(usageNote(row({ usageAvailableKnown: false, usageAvailable: false }))).toBeNull();
    expect(reportsWindows(row({ usageAvailableKnown: false, usageAvailable: false }))).toBe(true);
  });
});

describe('cardsFor', () => {
  const ACCOUNTS = [
    acct({ account: '9c0d1e2f', displayName: 'personal', profiles: ['personal'], maxPct: 95 }),
    acct({ account: '1a2b3c4d', displayName: 'studio.example', profiles: ['default', 'studio'], maxPct: 49 }),
    acct({ account: '5e6f7a8b', displayName: 'acme', profiles: ['acme'], maxPct: 23 }),
  ];

  // Every account gets a card and none is hidden behind a control: the whole
  // point of the cards layout.
  it('shows every account when no profile is selected', () => {
    expect(cardsFor(ACCOUNTS, ALL_PROFILES, ROSTER)).toHaveLength(3);
  });

  it('keeps the server order, highest first', () => {
    expect(cardsFor(ACCOUNTS, ALL_PROFILES, ROSTER).map((a) => a.account))
      .toEqual(['9c0d1e2f', '1a2b3c4d', '5e6f7a8b']);
  });

  it('narrows to one card for a selected profile', () => {
    const cards = cardsFor(ACCOUNTS, 'personal', ROSTER);
    expect(cards).toHaveLength(1);
    expect(cards[0].account).toBe('9c0d1e2f');
  });

  // The case the account/profile distinction exists for: selecting either of
  // two profiles on one account shows ONE card, not two, because there is one
  // allowance.
  it('shows one card for either profile of a shared account', () => {
    const viaHg = cardsFor(ACCOUNTS, 'studio', ROSTER);
    const viaDefault = cardsFor(ACCOUNTS, 'default', ROSTER);
    expect(viaHg).toHaveLength(1);
    expect(viaDefault).toHaveLength(1);
    expect(viaHg[0].account).toBe(viaDefault[0].account);
  });
});

describe('cardCaption', () => {
  it('names the profiles sharing an allowance', () => {
    const a = acct({ account: '1a2b3c4d', profiles: ['default', 'studio'] });
    expect(cardCaption(a, null)).toBe('shared by default, studio');
  });

  // One profile is already what the card's title says, so repeating it is
  // noise. Same rule feedsText follows.
  it('says nothing about sharing when there is one profile', () => {
    expect(cardCaption(acct({ profiles: ['acme'] }), null)).toBeNull();
  });

  it('joins the billed caption when there is one', () => {
    const a = acct({ account: '1a2b3c4d', profiles: ['default', 'studio'] });
    expect(cardCaption(a, 'billed $4.00 month to date'))
      .toBe('shared by default, studio · billed $4.00 month to date');
  });
});

describe('ignoresSelectionNote', () => {
  // A global control that some pages silently ignore is worse than no
  // control: the reader cannot tell "filtered to nothing" from "not filtered".
  it('admits a view does not honour the selection', () => {
    expect(ignoresSelectionNote('acme', 'Insights')).toBe('Insights covers every profile');
  });

  it('says nothing when nothing is selected', () => {
    expect(ignoresSelectionNote(ALL_PROFILES, 'Insights')).toBeNull();
  });
});
