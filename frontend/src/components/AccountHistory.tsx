'use client';
import { useCallback, useState } from 'react';
import { obsClient } from '@/src/lib/client';
import { usePoll } from '@/src/lib/usePoll';
import { known, accountLabel } from '@/src/lib/dashboard';
import type { AccountSnapshot, GetAccountHistoryResponse } from '@/src/gen/claudeobs/v1/obs_pb';
import { ObservationChart } from './charts/ObservationChart';
import { Alert, BillingValue, PageState } from './AccountUI';

export function useAccountHistory(accountKey: string, days: number) {
  return usePoll(useCallback(() => obsClient.getAccountHistory({ accountKey, days }), [accountKey, days]), 60000);
}
export function AllowanceTrace({ history, windowId, resetAt, durationSeconds }: { history: GetAccountHistoryResponse | null; windowId: string; resetAt?: number; durationSeconds?: number }) {
  if (!known(resetAt)) return <p className="obs-chart-summary mt-6">Current-window history is unavailable because the provider has not reported a reset boundary.</p>;
  const series = history?.windows.filter((s) => s.windowId === windowId && (!known(resetAt) || s.resetAt === resetAt)) ?? [];
  if (!history) return <p className="obs-chart-summary mt-6">Loading retained observations…</p>;
  return <div className="mt-6"><p className="obs-chart-summary mb-3">Consumption in the current reset window</p><ObservationChart series={series} from={known(durationSeconds) ? resetAt - durationSeconds : undefined} to={resetAt} resetAt={resetAt} label="Observed allowance consumption" percent /><p className="obs-chart-summary">{series[0]?.coverageNote || 'Only retained observations are shown. A reset is confirmed by the provider.'}</p></div>;
}
export function BillingHistory({ account, title = 'Reported overage' }: { account: AccountSnapshot; title?: string }) {
  const [days, setDays] = useState(7);
  const result = useAccountHistory(account.key, days);
  return <section className="obs-panel" aria-label={`${accountLabel(account)} reported overage history`}>
    <div className="obs-panel-heading"><h2>{title}</h2><label className="flex items-center gap-3 text-xs text-muted">Show history<select className="obs-input" aria-label="Overage history period" value={days} onChange={(e) => setDays(Number(e.target.value))}><option value={7}>7 days</option><option value={30}>30 days</option><option value={90}>90 days</option></select></label></div>
    <div className="obs-panel-body"><p className="obs-eyebrow">Latest reported amount</p><BillingValue billing={account.billing} />{account.provider === 'claude' && known(account.billing?.reportedOverageUsd) && <p className="text-xs text-muted mt-3">Month-to-date at its last observation, not a total for the selected {days} days.</p>}</div>
    {result.error && <div className="px-5"><Alert tone="error">{result.data ? 'History refresh failed. Showing the last response. ' : 'History unavailable. '}{result.error}</Alert></div>}
    {result.data?.errors.map((e) => <div className="px-5" key={e.section}><Alert>{e.section}: {e.error}</Alert></div>)}
    {result.data?.billing.length ? <div className="px-5 pb-5"><BillingCoverage history={result.data} /><ObservationChart series={result.data.billing.map((s) => ({ ...s, label: `${s.period || 'Reporting period unknown'}${s.source.includes('legacy') ? ' · Earlier collector' : ''}` }))} from={result.data.from} to={result.data.to} label={`${accountLabel(account)} provider-reported month-to-date overage in USD`} /></div> : account.billing?.state === 'unavailable' ? <PageState title="Billing history not provided">{account.billing.note || 'This provider does not expose reported overage through the connected source.'}</PageState> : !result.data && !result.error ? <PageState title="Loading reported history…" /> : <PageState title="No reported overage history">A missing observation is unknown, even when extra usage is disabled.</PageState>}
    <div className="obs-panel-note"><p>Each point is a provider-reported amount in USD. Monthly readings are not added together; gaps remain unknown.</p><details className="mt-3"><summary className="cursor-pointer">About this history</summary><p className="mt-2">{result.data?.coverageNote || 'Only retained provider observations are shown.'} Included usage has no dollar valuation.</p><p className="mt-2">Source: {account.billing?.pricingSource || 'Provider billing records'}. Historical readings are not final invoices.</p></details></div>
  </section>;
}

export function BillingCoverage({ history }: { history: GetAccountHistoryResponse }) {
  const times = history.billing.flatMap((s) => s.points.map((p) => p.ts)).filter((t) => Number.isFinite(t) && t > 0);
  if (!times.length) return null;
  const first = times.reduce((a, b) => Math.min(a, b));
  const last = times.reduce((a, b) => Math.max(a, b));
  const date = (t: number) => new Date(t * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
  return <p className="obs-chart-summary mb-3">Retained readings: {date(first)} – {date(last)}{first > history.from + 86400 ? '. Earlier dates in this range have no retained readings.' : ''}</p>;
}
