import type { AccountPlan, PlanWindow } from '@/src/gen/claudeobs/v1/obs_pb';

/**
 * Picking, labelling and captioning for the Overview's account selector.
 *
 * An allowance belongs to a Claude account, not to a profile: several
 * instrumented profiles can be signed in to one account and share one set of
 * windows. Everything here is keyed on the account for that reason, and pure,
 * so the rules are testable without rendering anything.
 */

/**
 * The account whose meters to show: the one selected, else the primary the
 * server nominates, else the first (which the server sorts highest-first, so
 * the account closest to throttling wins a cold start).
 */
export function selectAccount(
  accounts: AccountPlan[],
  selected: string | null,
  primary: string,
): AccountPlan | null {
  if (accounts.length === 0) return null;
  return (
    accounts.find((a) => a.account === selected) ??
    accounts.find((a) => a.account === primary) ??
    accounts[0]
  );
}

/**
 * An account's label in the picker, carrying its highest window percentage.
 *
 * The percentage is on the label deliberately. A picker showing bare names
 * hides the number that matters until the reader opens each option in turn,
 * which is exactly when an account sitting at 94% goes unnoticed.
 */
export function accountLabel(a: AccountPlan): string {
  return a.hasMaxPct ? `${a.displayName} ${Math.round(a.maxPct)}%` : a.displayName;
}

/**
 * The profiles feeding an account, when there is more than one. A single
 * profile is already what displayName says, so repeating it is noise.
 */
export function feedsText(a: AccountPlan): string | null {
  return a.profiles.length > 1 ? `feeds: ${a.profiles.join(', ')}` : null;
}

/**
 * The billed-dollars caption.
 *
 * "$0.00" is the wrong answer for an account with extra usage switched off:
 * it reads as "spent nothing" when the truth is "cannot spend". Such an
 * account throttles at 100% instead of charging, so it is said in words.
 * Returns null when the poller has not reported either way, since a caption
 * asserting anything would be inventing it.
 */
export function billedCaption(a: AccountPlan, money: (v: number) => string): string | null {
  if (a.extraUsageKnown && a.extraUsageUsd > 0) return `Provider-reported overage ${money(a.extraUsageUsd)} month to date${a.overageKnown && !a.overageEnabled ? '; extra usage currently disabled' : ''}`;
  if (!a.overageKnown) return null;
  if (!a.overageEnabled) return 'Extra usage is currently disabled';
  if (!a.extraUsageKnown) return 'overage enabled, no billed figure yet';
  return `Provider-reported overage ${money(a.extraUsageUsd)} month to date`;
}

/**
 * The window rows to render: the selected account's when there is one, else
 * the flat list, which is what an install whose poller predates account
 * labels still reports.
 */
export function windowsFor(account: AccountPlan | null, flat: PlanWindow[]): PlanWindow[] {
  return account ? account.windows : flat;
}

/** Whether the picker earns its place. One account needs no chooser. */
export function showPicker(accounts: AccountPlan[]): boolean {
  return accounts.length > 1;
}
