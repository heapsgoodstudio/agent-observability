'use client';

import { useCallback } from 'react';
import { usePathname, useRouter, useSearchParams } from 'next/navigation';
import { ALL_PROFILES } from './profiles';
import { normalizeProvider, type ProviderFilter } from './providers';
export { withProfile } from './providers';

/**
 * The selected profile, read from and written to `?profile=`.
 *
 * The query string is the source of truth rather than a React context or
 * localStorage, for three reasons. Spend already treats it that way for
 * period, measure and project, so a second mechanism beside it would be two
 * answers to "what is this page showing". A URL survives a refresh and can be
 * pasted to someone. And there is no state to go stale against a poll that
 * reshuffles the roster: the name in the URL is checked against the roster on
 * every render (normalizeProfile), not remembered from when it was chosen.
 *
 * Writes use router.replace, not push: changing the filter is not a navigation
 * step, and a push would make the back button walk through every filter the
 * reader tried rather than returning them to the previous page.
 */
export function useProfile(): {
  profile: string;
  provider: ProviderFilter;
  setProfile: (next: string, provider?: ProviderFilter) => void;
  setProvider: (next: ProviderFilter) => void;
} {
  const router = useRouter();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const profile = searchParams.get('profile') ?? ALL_PROFILES;
  const provider = normalizeProvider(searchParams.get('provider'));

  const setProfile = useCallback(
    (next: string, nextProvider?: ProviderFilter) => {
      const params = new URLSearchParams(searchParams.toString());
      if (next) params.set('profile', next);
      else params.delete('profile');
      if (nextProvider !== undefined) {
        if (nextProvider) params.set('provider', nextProvider);
        else params.delete('provider');
      }
      params.delete('id');
      const qs = params.toString();
      router.replace(`${pathname}${qs ? `?${qs}` : ''}`, { scroll: false });
    },
    [router, pathname, searchParams],
  );

  const setProvider = useCallback((next: ProviderFilter) => setProfile('', next), [setProfile]);
  return { profile, provider, setProfile, setProvider };
}

/**
 * Carries the current profile onto a link.
 *
 * The sidebar nav is plain next/link, which would drop the query string and
 * silently reset the filter on every navigation. That is the one real cost of
 * keeping the selection in the URL, and this is where it is paid: one helper,
 * used by the nav, rather than a filter that quietly forgets itself.
 */
