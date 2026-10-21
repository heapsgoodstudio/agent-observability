// Pure list helpers for the Sessions page (components/pages/Sessions.tsx).
// Sort and filter are page state, deliberately not encoded in the URL. The
// merge/badge logic that used to sit beside them
// (mergeRows, the "real $" gate) is now resolved server-side on each
// SessionRow (live, billedOverlap): ListSessionsResponse already returns the
// full, already-merged row set. What is left here is exactly what was always
// a pure UI concern: sorting and filtering the fetched rows for display. No
// DOM, no fetching, no module state.
import type { SessionRow } from '../gen/claudeobs/v1/obs_pb';
import { ageText, shortProject } from './format';
import { providerOf } from './providers';

export type SessionSort = 'tokens' | 'recent';

export function totalSessionTokens(row: Pick<SessionRow, 'tokens'>): number {
  return row.tokens ? row.tokens.input + row.tokens.output + row.tokens.cacheRead + row.tokens.cacheCreation : 0;
}

// The table shows at most this many rows after sort/filter, with a "showing
// top N" note.
export const SESSIONS_ROW_CAP = 200;

// Applied to a copy, so the caller's array (React state) is never mutated in
// place.
export function sortSessions(rows: SessionRow[], sort: SessionSort): SessionRow[] {
  const copy = [...rows];
  return sort === 'recent'
    ? copy.sort((a, b) => (b.end ?? 0) - (a.end ?? 0))
    : copy.sort((a, b) => totalSessionTokens(b) - totalSessionTokens(a));
}

// The name a session shows in its Project column: the workspace the server
// derived from where the session actually worked ("owner/repo" for a git
// repository, a directory path otherwise). A row indexed before that field
// existed carries an empty workspace and falls back to the shortened project
// key, which is exactly what this column showed before.
export function projectLabel(row: Pick<SessionRow, 'workspace' | 'project'>): string {
  return row.workspace || shortProject(row.project);
}

// A session's profile cell, or null when the row predates the column.
//
// Null rather than a guess: a row indexed before profiles existed genuinely
// does not know which profile it came from, and naming one would be inventing
// it. The same rule the delivery counters follow, for the same reason.
export function profileLabel(row: Pick<SessionRow, 'profile'>): string | null {
  return row.profile ? row.profile : null;
}

// Matches the trimmed, lower-cased filter text against the session id, its
// displayed project label, the underlying project key, or its profile. The
// key stays matchable because it is what Claude Code's own directory listing
// shows, so a path pasted from there still finds its sessions; the profile is
// matchable so "personal" narrows the list to one profile without a separate
// control, which is the same affordance the filter already gives projects.
export function filterSessions(rows: SessionRow[], filter: string): SessionRow[] {
  const f = filter.trim().toLowerCase();
  if (!f) return rows;
  return rows.filter((r) =>
    r.id.includes(f)
    || r.project.toLowerCase().includes(f)
    || (r.profile ?? '').toLowerCase().includes(f)
    || (r.nativeId ?? '').toLowerCase().includes(f)
    || providerOf(r).includes(f)
    || projectLabel(r).toLowerCase().includes(f));
}

// A delivery counter's cell text, or null when there is nothing to show.
// Zero is null rather than "0" on purpose: these counters have no historical
// backfill, so a session that ended before the collector started reads 0 in
// exactly the same way as one that genuinely opened no pull requests, and
// printing "0" would assert the second when only the first is known.
export function deliveryText(n: number | undefined): string | null {
  if (!n || n <= 0) return null;
  return String(Math.round(n));
}

// The index-staleness caption.
// ListSessionsResponse carries generatedAt as an RFC 3339 string (the db's
// own resolved "is the index stale" bool is ListSessionsResponse.indexStale,
// read directly rather than recomputed here); this only derives the age text
// for display, the same derivation healthUpdatedAt makes from a timestamp
// field elsewhere. Returns null when the field is absent or unparseable, so
// a caller can omit the age suffix entirely rather than showing "NaNs old".
export function indexAgeText(generatedAt: string, now: number): string | null {
  if (!generatedAt) return null;
  const t = Date.parse(generatedAt);
  if (Number.isNaN(t)) return null;
  return ageText(now - t / 1000);
}
