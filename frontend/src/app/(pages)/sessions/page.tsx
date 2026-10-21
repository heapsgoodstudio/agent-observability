'use client';

import { Suspense } from 'react';
import { useSearchParams } from 'next/navigation';
import { Sessions } from '@/src/components/pages/Sessions';
import { SessionDetail } from '@/src/components/pages/SessionDetail';

// Static export constraint: the session id (and the analysis sub-tab) are
// read from the query string client-side rather than a dynamic route
// segment, so this one route serves both the session list and a session's
// detail view. useSearchParams requires a Suspense boundary in the App
// Router; this is the one place it lives for this route, per the brief.
function SessionsRoute() {
  const searchParams = useSearchParams();
  const id = searchParams.get('id');
  return id ? <SessionDetail key={id} id={id} /> : <Sessions />;
}

export default function SessionsPage() {
  return (
    <Suspense
      fallback={
        <p className="m-0 text-muted">loading…</p>
      }
    >
      <SessionsRoute />
    </Suspense>
  );
}
