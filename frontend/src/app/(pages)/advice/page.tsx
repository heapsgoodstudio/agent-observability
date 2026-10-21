'use client';

import { useEffect } from 'react';
import { useRouter } from 'next/navigation';
import { normalizeProvider, withProfile } from '@/src/lib/providers';

// Advice merged into the Insights view (its second tab); this route
// survives only so old bookmarks and muscle memory land somewhere real.
export default function AdvicePage() {
  const router = useRouter();
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    router.replace(withProfile('/insights?tab=advice', params.get('profile') ?? '', normalizeProvider(params.get('provider'))));
  }, [router]);
  return <p className="m-0 px-6 py-4 text-muted">moved to Insights, redirecting…</p>;
}
