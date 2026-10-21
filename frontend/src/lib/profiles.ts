import type { AccountPlan, ProfileRow } from '@/src/gen/claudeobs/v1/obs_pb';

/**
 * The global profile selector's pure rules: what the options are, what each
 * one is called, and which account a selection resolves to.
 *
 * The distinction this file exists to hold: a PROFILE is a Claude Code config
 * directory and is what a session, a token and a dollar belong to. An ACCOUNT
 * is a Claude login and is what an allowance belongs to. Two profiles can be
 * signed in to one account and share one set of plan windows, so every rule
 * here that touches a percentage goes through the account.
 *
 * No DOM, no fetching, no module state, matching accounts.ts beside it.
 */

/** The sentinel for "every profile", and the value a missing filter means. */
export const ALL_PROFILES = '';

/**
 * Normalizes a ?profile= value against the roster the server returned.
 *
 * An unknown name resolves to "every profile" rather than to a filter that
 * matches nothing: a stale bookmark naming a profile that has since been
 * removed should show the whole dashboard, not an empty one that looks broken.
 */
export function normalizeProfile(raw: string | null, rows: ProfileRow[]): string {
  if (!raw) return ALL_PROFILES;
  return rows.some((r) => r.profile === raw) ? raw : ALL_PROFILES;
}

/**
 * The row for the selected profile, or null for "every profile" and for a
 * name the roster does not know.
 */
export function selectedProfile(rows: ProfileRow[], selected: string): ProfileRow | null {
  if (!selected) return null;
  return rows.find((r) => r.profile === selected) ?? null;
}

/**
 * The account a selection resolves to, empty for "every profile".
 *
 * This is the join every allowance figure goes through. Returning the account
 * rather than the profile is what stops a shared allowance being drawn twice.
 */
export function accountOf(rows: ProfileRow[], selected: string): string {
  return selectedProfile(rows, selected)?.account ?? '';
}

/**
 * The other profiles signed in to the same account as this one.
 *
 * Named so a caption can say "shares an allowance with studio" rather than
 * leaving the reader to work out why two rows show the same percentage.
 */
export function siblingProfiles(rows: ProfileRow[], profile: string): string[] {
  const account = accountOf(rows, profile);
  if (!account) return [];
  return rows.filter((r) => r.account === account && r.profile !== profile).map((r) => r.profile);
}

/**
 * The percentage caption for one row: the account's highest window.
 *
 * Null when nothing has reported one, so a caller renders a dash rather than a
 * zero. A zero here would read as an idle account when the truth is an account
 * nothing has measured.
 */
export function pctLabel(r: ProfileRow): string | null {
  return r.hasMaxPct ? `${Math.round(r.maxPct)}%` : null;
}

/**
 * Why a profile has no plan windows, in words, or null when it has them or
 * when nothing knows.
 *
 * The three states are deliberately distinct. Claude Code answering "no
 * subscription, no rate limits" is a lapsed sign-in and will never resolve
 * itself, so it is said out loud. Nothing having probed yet is not the same
 * thing and says nothing, because it resolves on the next poll. A permanent
 * zero with no explanation reads as a broken poller, which is the failure this
 * replaces.
 */
export function usageNote(r: ProfileRow): string | null {
  if (!r.usageAvailableKnown) return null;
  if (!r.usageAvailable) return 'not signed in, cannot report plan windows';
  return null;
}

/** Whether a profile can be expected to report plan windows at all. */
export function reportsWindows(r: ProfileRow): boolean {
  return !r.usageAvailableKnown || r.usageAvailable;
}

/**
 * The account cards the Overview meters render, in the order they appear.
 *
 * With no profile selected every account gets a card, highest first, so the
 * account closest to throttling is at the top and none is hidden behind a
 * control. With one selected, only that profile's account is shown, because
 * the reader has said which allowance they are asking about.
 *
 * The server already sorts accounts highest-first; this does not re-sort them,
 * so the cards and the selector cannot disagree about which account leads.
 */
export function cardsFor(accounts: AccountPlan[], selected: string, rows: ProfileRow[]): AccountPlan[] {
  const account = accountOf(rows, selected);
  if (!account) return accounts;
  const one = accounts.filter((a) => a.account === account);
  // A selected profile whose account has no plan of its own (nothing has
  // polled it) falls back to showing nothing rather than to showing every
  // account, since every account is precisely what was not asked for.
  return one;
}

/**
 * The caption under one account card: which profiles feed it, and what its
 * billed state is. Built from the card's own profiles list so it stays true
 * when a profile is selected and the others are off screen.
 */
export function cardCaption(a: AccountPlan, billed: string | null): string | null {
  const parts: string[] = [];
  if (a.profiles.length > 1) parts.push(`shared by ${a.profiles.join(', ')}`);
  if (billed) parts.push(billed);
  return parts.length > 0 ? parts.join(' · ') : null;
}

/**
 * The page-header note for a view that does not honour the selector.
 *
 * A global control that some pages silently ignore is worse than no control:
 * the reader has no way to tell "filtered to nothing" from "not filtered".
 * Insights and Health are install-wide, so they say so.
 */
export function ignoresSelectionNote(selected: string, viewName: string): string | null {
  if (!selected) return null;
  return `${viewName} covers every profile`;
}
