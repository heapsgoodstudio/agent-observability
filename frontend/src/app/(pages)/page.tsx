'use client';

import { useEffect } from 'react';
import { useRouter } from 'next/navigation';
import { resolveAlias } from '@/src/lib/aliases';
import { Overview } from '@/src/components/pages/Overview';
import { scopeHref } from '@/src/lib/dashboard';

// The alias redirect lives ONLY here, on the root page: on mount, inspect
// window.location.hash against ALIASES (src/lib/aliases.ts's resolveAlias).
// A known alias always goes through router.replace to its path plus
// `#<scrollTo>`, including Overview's own aliases (e.g. #now), which
// resolve back to "/" itself.
//
// This used to branch: a first pass assumed router.replace() would no-op a
// same-pathname, hash-only replace and bypassed it with a direct
// window.history.replaceState call. That assumption was wrong, and the
// bypass has been removed. Verified directly (chrome devtools MCP against
// `just build`'s real static export served by the actual ./dist/claude-obs
// serve binary, next@15.5.23, a scratch --install-dir/--addr so as not to
// touch the live instance): loaded "/" fresh, clicked a button that called
// router.replace('/#zzztest') from the already-mounted page (the exact
// same-pathname, hash-only case this file hits for #now/#overage), and
// window.location.href updated to ".../#zzztest" with no reload and no
// console errors. Reading next@15.5.23's own source confirms why:
// navigate-reducer.js's onlyHashChange fast path (checked when the
// canonicalUrl's pathname is unchanged) sets mutable.canonicalUrl and
// mutable.hashFragment and returns through handleMutable; app-router.js's
// HistoryUpdater effect then calls window.history.replaceState with that
// canonicalUrl, and layout-router.js's ScrollAndFocusHandler consumes the
// same hashFragment to scroll the target element into view. So
// router.replace already does both the URL rewrite and the scroll for the
// same-route case; this file no longer needs to do either manually. An
// unknown hash (or none) does nothing.
export default function Home() {
  const router = useRouter();

  useEffect(() => {
    if (!window.location.hash) return;
    const resolved = resolveAlias(window.location.hash);
    if (!resolved) return;

    const target = resolved.scrollTo ? `${resolved.path}#${resolved.scrollTo}` : resolved.path;
    const params = new URLSearchParams(window.location.search);
    router.replace(scopeHref(target, { profile: params.get('profile') ?? '', provider: params.get('provider') ?? '', accountKey: params.get('accountKey') ?? '' }));
  }, [router]);

  return <Overview />;
}
