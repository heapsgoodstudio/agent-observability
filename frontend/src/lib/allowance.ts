// Confirmation-age limits, shared by current and legacy displays. Keep these
// aligned with server.staleAfterS and monitor.QuotaFreshFor respectively.
export const CLAUDE_FRESH_FOR_S = 10 * 60;
export const CODEX_FRESH_FOR_S = 5 * 60;

// Shared thresholds for consumed allowance in every provider's UI.
export const ALLOWANCE_BANDS = [
  { at: 75, tone: 'warn' as const },
  { at: 90, tone: 'danger' as const },
];

export function allowanceTone(used: number | undefined): 'normal' | 'warn' | 'danger' | 'unknown' {
  if (used === undefined || !Number.isFinite(used)) return 'unknown';
  if (used >= ALLOWANCE_BANDS[1].at) return 'danger';
  return used >= ALLOWANCE_BANDS[0].at ? 'warn' : 'normal';
}

/** Freshness must keep aging even when requests fail or a tab goes offline. */
export function allowanceStale(snapshot: { fetchedAt: number; stale: boolean }, now: number, refreshFailed = false): boolean {
  return refreshFailed || snapshot.stale || snapshot.fetchedAt <= 0 || now - snapshot.fetchedAt > CODEX_FRESH_FOR_S || snapshot.fetchedAt > now + 60;
}

export function codexPlanLabel(value: string): string {
  return ({ free: 'Free', go: 'Go', plus: 'Plus', pro: 'Pro', team: 'Business', business: 'Business', self_serve_business: 'Business', self_serve_business_prolite: 'Business', enterprise: 'Enterprise', edu: 'Education' } as Record<string, string>)[value.toLowerCase()] ?? 'Plan details unavailable';
}
