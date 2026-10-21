// Hash-alias resolution for the one-time redirect off the root page. Pure
// string work, no DOM: window.location.hash is read by whichever page calls
// this, never by this module itself.
//
// Why this exists: bin/usage-alert.sh posts #now, #overage and #history to
// the live dashboard, so breaking any of those three sends an alert click to
// a blank page. The rest are the plain page names, plus the empty and bare
// hashes the root page redirects from. No alias carries a session id, so
// none of them needs a `?id=` threaded through: every one resolves to a
// bare path.
type PageName = 'overview' | 'spend' | 'sessions' | 'findings' | 'advice' | 'health';

interface AliasEntry {
  page: PageName;
  scrollTo: string | null;
}

// Exported for aliases.scroll-targets.test.ts, which asserts every non-null
// scrollTo here names a Section id that actually exists on its target page's
// component.
export const ALIASES: Record<string, AliasEntry> = {
  '': { page: 'overview', scrollTo: null },
  '#': { page: 'overview', scrollTo: null },
  '#overview': { page: 'overview', scrollTo: null },
  '#now': { page: 'overview', scrollTo: 'meters' },
  '#overage': { page: 'overview', scrollTo: 'curve' },
  '#spend': { page: 'spend', scrollTo: null },
  '#history': { page: 'spend', scrollTo: 'daily' },
  '#sessions': { page: 'sessions', scrollTo: null },
  '#insights': { page: 'findings', scrollTo: null },
  '#findings': { page: 'findings', scrollTo: null },
  '#advice': { page: 'advice', scrollTo: null },
  '#health': { page: 'health', scrollTo: null },
};

// The route each page name maps to. Session detail has no entry: it lives
// at /sessions with a `?id=` query param rather than its own path, and
// nothing in ALIASES ever resolves to it. 'findings' and 'advice' survived
// the Insights merge as internal keys: both now resolve to the /insights
// view (advice to its tab), so old bookmarks and the alias vocabulary keep
// working.
const PAGE_ROUTE: Record<PageName, string> = {
  overview: '/',
  spend: '/spend',
  sessions: '/sessions',
  findings: '/insights',
  advice: '/insights?tab=advice',
  health: '/health',
};

export interface AliasTarget {
  path: string;
  scrollTo: string | null;
}

// Resolves a raw hash (as read from window.location.hash, e.g. "#now") to
// the path and section id it should redirect to, or null when the hash names
// no known alias. Strips a trailing "?..." and lowercases before matching,
// since a hash carrying query params or mixed case is otherwise
// indistinguishable from one that legitimately doesn't match anything.
export function resolveAlias(hash: string): AliasTarget | null {
  const raw = String(hash ?? '');
  const qi = raw.indexOf('?');
  const pathRaw = qi === -1 ? raw : raw.slice(0, qi);
  const path = pathRaw.toLowerCase();

  if (!Object.hasOwn(ALIASES, path)) return null;

  const { page, scrollTo } = ALIASES[path];
  return { path: PAGE_ROUTE[page], scrollTo };
}
