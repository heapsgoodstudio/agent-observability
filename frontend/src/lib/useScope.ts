'use client';
import { useMemo } from 'react';
import { useSearchParams } from 'next/navigation';
import type { DashboardScope } from './dashboard';
export function useScope(): DashboardScope {
  const params = useSearchParams();
  const provider = params.get('provider') ?? '';
  const accountKey = params.get('accountKey') ?? '';
  const profile = params.get('profile') ?? '';
  return useMemo(() => ({ provider, accountKey, profile }), [provider, accountKey, profile]);
}
