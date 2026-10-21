'use client';

import { useEffect } from 'react';

// The shared half of the alias-redirect contract (see src/lib/aliases.ts):
// the root page resolves a known hash alias and replaces the URL with its
// path plus `#<scrollTo>` (e.g. "/spend#daily"), either navigating there
// (a real mount, for any page other than overview) or, for an alias that
// already resolves to "/", rewriting the hash in place. Either way, by the
// time a page using this hook mounts, window.location.hash already names a
// real section id (ALIASES' scrollTo values: "meters", "curve", "daily"),
// not the alias key itself, so reading it directly here and
// scrolling to it is correct without this hook needing to know about
// aliases at all.
//
// Every page with a scroll target consumes this on mount; it is a no-op
// when there is no hash or no matching element, so a plain page load (no
// hash, or a hash this page doesn't own a section for) does nothing.
export function useScrollTarget(): void {
  useEffect(() => {
    const id = window.location.hash.slice(1);
    if (!id) return;
    document.getElementById(id)?.scrollIntoView();
  }, []);
}
