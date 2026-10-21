'use client';

import { Suspense } from 'react';
import { Insights } from '@/src/components/pages/Insights';

// useSearchParams (the ?tab= read inside Insights) requires a Suspense
// boundary in the App Router; this is the one place it lives for this
// route, same pattern as sessions/page.tsx.
export default function InsightsPage() {
  return (
    <Suspense fallback={<p className="m-0 px-6 py-4 text-muted">loading…</p>}>
      <Insights />
    </Suspense>
  );
}
