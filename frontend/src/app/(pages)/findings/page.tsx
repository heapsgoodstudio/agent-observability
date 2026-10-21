'use client';

import { useEffect } from 'react';
import { useRouter } from 'next/navigation';
import { normalizeProvider, withProfile } from '@/src/lib/providers';

// Findings merged into the Insights view; this route survives only so old
// bookmarks and muscle memory land somewhere real.
export default function FindingsPage() {
  const router = useRouter();
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    router.replace(withProfile('/insights', params.get('profile') ?? '', normalizeProvider(params.get('provider'))));
  }, [router]);
  return <p className="m-0 px-6 py-4 text-muted">moved to Insights, redirecting…</p>;
}
