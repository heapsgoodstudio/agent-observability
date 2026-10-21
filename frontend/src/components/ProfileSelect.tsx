'use client';

import type { ProfileRow } from '@/src/gen/claudeobs/v1/obs_pb';
import { pctLabel, reportsWindows } from '@/src/lib/profiles';
import { indexedProvider, providerLabel, providerOf } from '@/src/lib/providers';

// The one profile control the whole dashboard reads, rendered in the sidebar
// (components/Shell.tsx) rather than in any page's own header: it applies to
// every view, so it belongs to the chrome. Three per-page affordances came
// before it, none of which agreed with the others about what "this profile"
// meant.
//
// It is a native <select>. The option list carries a percentage and an
// account name per row, which a custom popover would render more prettily, but
// a native select is keyboard accessible, works on a phone, and cannot trap
// focus. The one thing it costs is styling inside the option list, which is
// why the label packs its facts into one line of text rather than a layout.

export interface ProfileSelectProps {
  rows: ProfileRow[];
  value: string;
  onChange: (next: string) => void;
}

// One option's text: the profile, then its account's domain, then the
// account's percentage. The percentage is on the label deliberately, the same
// reasoning accounts.ts's accountLabel records: a picker of bare names hides
// the number that matters until the reader opens each option in turn, which is
// exactly when an account sitting at 95% goes unnoticed.
//
// The domain sits between them because it is what says two rows showing the
// same number are one allowance rather than two.
export function optionLabel(r: ProfileRow): string {
  const parts = [r.displayName || r.profile, providerLabel(providerOf(r))];
  if (r.provider && !r.enabled) parts.push('disabled');
  if (indexedProvider(providerOf(r))) {
    if (r.status) parts.push(r.status);
    return parts.join(' · ');
  }
  if (r.domain) parts.push(r.domain);
  if (!reportsWindows(r)) {
    parts.push('not signed in');
    return parts.join(' · ');
  }
  const pct = pctLabel(r);
  parts.push(pct ?? 'no plan data');
  return parts.join(' · ');
}

export function ProfileSelect({ rows, value, onChange }: ProfileSelectProps) {
  return (
    <div className="px-2.5">
      <label className="sr-only" htmlFor="profile-select">Profile</label>
      <select
        id="profile-select"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        // Disabled rather than hidden while the roster is still loading: a
        // control that appears after the first poll would shift the whole nav
        // down a row on every page load.
        disabled={rows.length === 0}
        className="h-7 w-full rounded-md border border-line bg-raised px-2 text-[12.5px] text-primary disabled:text-muted"
      >
        <option value="">All profiles</option>
        {rows.map((r) => (
          <option key={r.profile} value={r.profile}>
            {optionLabel(r)}
          </option>
        ))}
      </select>
    </div>
  );
}
