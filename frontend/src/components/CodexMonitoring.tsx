'use client';

import { useCallback, type ReactNode } from 'react';
import Link from 'next/link';
import { obsClient } from '@/src/lib/client';
import type { CodexAccountUsage, CodexUsageWindow } from '@/src/gen/claudeobs/v1/obs_pb';
import { usePoll } from '@/src/lib/usePoll';
import { useProfile, withProfile } from '@/src/lib/useProfile';
import { useNow } from '@/src/lib/useNow';
import { agoText, ageText, compactInt, countdown, fmtInt, money } from '@/src/lib/format';
import { ViewHeader } from '@/src/components/ViewHeader';
import { Banner } from '@/src/components/ui/Banner';
import { Badge } from '@/src/components/ui/Badge';
import { StatTile } from '@/src/components/ui/StatTile';
import { Meter } from '@/src/components/ui/Meter';
import { observedTotal } from '@/src/lib/providers';
import { ALLOWANCE_BANDS, allowanceStale, codexPlanLabel } from '@/src/lib/allowance';
import { creditPriceLabel, creditsLabel } from '@/src/lib/dashboard';

export function ProviderPage({ title, children, codexContent, historyOnly = false }: { title: string; children: ReactNode; codexContent?: ReactNode; historyOnly?: boolean }) {
  const { provider, profile } = useProfile();
  if (provider === 'codex') return <>{codexContent ?? <ViewHeader title={`${title} · Codex`} />}<CodexMonitoring key={profile} profile={profile} historyOnly={historyOnly} /></>;
  return <>{children}{!provider ? <CodexMonitoring key={profile} profile={profile} historyOnly={historyOnly} /> : null}</>;
}

function ResetTime({ at, now }: { at: number; now: number }) {
  if (!(at > 0)) return <p className="mb-0 mt-1 text-[11px] text-muted">Reset time unavailable</p>;
  const date = new Date(at * 1000);
  return <div className="mt-2 text-[11px] leading-5 text-muted"><p className="m-0">{at > now ? countdown(at, now) : 'Reset time passed; awaiting fresh confirmation.'}</p><time dateTime={date.toISOString()}>{date.toLocaleString(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' })} (local time)</time></div>;
}

function AllowanceWindow({ window, now, accountStale }: { window: CodexUsageWindow; now: number; accountStale: boolean }) {
  const stale = allowanceStale(window, now, accountStale);
  return <div className="rounded-md border border-hairline p-3">
    <Meter label={window.name || window.kind || 'Allowance'} value={window.usedPercent} max={100} bands={ALLOWANCE_BANDS} />
    <p className="mb-0 mt-2 text-[13px] font-medium text-primary">{stale ? 'Last observed: ' : ''}{Math.max(0, 100 - window.usedPercent).toFixed(0)}% remaining</p>
    {stale ? <div className="mt-2"><Badge tone="warn">Stale snapshot</Badge></div> : null}
    <ResetTime at={window.resetsAt} now={now} />
  </div>;
}

function AccountAllowance({ account, windows, now, refreshFailed }: { account: CodexAccountUsage; windows: CodexUsageWindow[]; now: number; refreshFailed: boolean }) {
  const stale = allowanceStale(account, now, refreshFailed);
  return <section aria-label={`${account.name || account.profile} allowance`} className="space-y-4 rounded-lg border border-hairline p-4">
    <div className="flex flex-wrap items-center gap-2">
      <h3 className="m-0 text-[14px] font-medium text-primary">{account.name || account.profile || 'Codex account'}</h3>
      {account.planType ? <Badge tone="neutral">{codexPlanLabel(account.planType)}</Badge> : null}
      <Badge tone={stale ? 'warn' : 'ok'}>{stale ? 'Stale snapshot' : 'Fresh snapshot'}</Badge>
      <span className="text-[11px] text-muted">{account.fetchedAt > 0 ? `Observed ${agoText(account.fetchedAt, now)}` : 'Observation time unavailable'}</span>
    </div>
    <p className="m-0 text-[12px] text-secondary">{account.account ? `Account ${account.account}` : 'Account identity unavailable; shown separately by profile'} · {account.ordinaryUsageAllowed === undefined ? 'Included usage availability unknown' : `${stale ? 'Last observed: ' : ''}${account.ordinaryUsageAllowed ? 'Included usage allowed' : 'Included usage unavailable'}${stale ? '; current status unconfirmed' : ''}`}</p>
    {windows.length ? <div className="grid gap-3 md:grid-cols-2">{windows.map((window, i) => <AllowanceWindow key={`${window.limitId}-${window.kind}-${i}`} window={window} now={now} accountStale={stale} />)}</div> : <p className="m-0 text-[12px] text-muted">No plan windows reported. Headroom is unavailable.</p>}
    {account.buckets.map((bucket, i) => <div key={`${bucket.limitId}-${i}`} className="space-y-2 border-t border-hairline pt-3">
      <div className="flex flex-wrap items-center gap-2"><span className="text-[13px] font-medium text-primary">{bucket.name || bucket.limitId || 'Allowance bucket'}</span>{bucket.normalModelSlug ? <span className="text-[11px] text-muted">{bucket.normalModelSlug}</span> : null}</div>
      <p className="m-0 text-[12px] text-secondary">{stale ? 'Last observed credits' : 'Available credits'}: {bucket.credits?.unlimited ? 'Unlimited' : bucket.credits?.balance !== undefined ? `${bucket.credits.balance} credits` : bucket.credits ? bucket.credits.hasCredits ? 'Available; balance not reported' : 'None reported' : 'Unavailable'}</p>
      {bucket.individualLimit ? <div>
        <Meter label={stale ? 'Last observed individual allowance used' : 'Individual allowance used'} value={100 - bucket.individualLimit.remainingPercent} max={100} bands={ALLOWANCE_BANDS} />
        <p className="mb-0 mt-2 text-[12px] text-secondary">{bucket.individualLimit.remainingPercent}% remaining · {bucket.individualLimit.used} of {bucket.individualLimit.limit} provider units used</p>
        <ResetTime at={bucket.individualLimit.resetsAt} now={now} />
      </div> : null}
      <div className="flex flex-wrap gap-2">
        {bucket.spendControlReached !== undefined ? <Badge tone={bucket.spendControlReached ? 'warn' : 'neutral'}>{stale ? 'Last observed: ' : ''}{bucket.spendControlReached ? 'Spend control reached' : 'Spend control not reached'}</Badge> : null}
        {bucket.rateLimitReachedType ? <Badge tone="warn">{stale ? 'Last observed: ' : ''}Provider reported an active limit</Badge> : null}
      </div>
    </div>)}
    {account.resetCreditsAvailable !== undefined ? <p className="m-0 text-[12px] text-secondary">{stale ? 'Last observed reset credits' : 'Available reset credits'}: {fmtInt.format(account.resetCreditsAvailable)}</p> : null}
  </section>;
}

export function CodexMonitoring({ profile, historyOnly = false }: { profile: string; historyOnly?: boolean }) {
  const fetchMonitoring = useCallback(() => obsClient.getMonitoring({ profile }), [profile]);
  const { data, error } = usePoll(fetchMonitoring);
  const now = useNow();
  return <section aria-label="Codex monitoring" className="obs-page space-y-5">
    <div className="flex flex-wrap items-center gap-2"><h2 className="m-0 text-[16px] font-semibold text-primary">{historyOnly ? 'Codex token history' : 'Codex allowance'}</h2><Link className="ml-auto text-[12px] text-secondary hover:underline" href={withProfile('/settings', profile, 'codex')}>Manage profiles</Link></div>
    <p className="m-0 text-[12px] text-muted">{historyOnly ? 'All indexed Codex history. Token coverage is separate from live allowance and reset information.' : 'Provider-reported plan headroom, reset times, and available credits. Included and unclassified usage has no dollar valuation.'}</p>
    {error ? <Banner tone="error">{data ? 'Refresh failed; showing the last snapshot. ' : 'Could not load Codex monitoring: '}{error}</Banner> : null}
    {!data && !error ? <p className="text-muted">Loading Codex allowance…</p> : null}
    {data ? <>
      {data.errors.map((source, i) => <Banner key={`${source.section}-${i}`} tone="warn">{source.section}: {source.error}</Banner>)}
      {data.configuredProfiles === 0 ? <Banner tone="info">No Codex profiles connected. Add a Codex directory in Settings.{data.sessions > 0 ? ' Preserved history appears below.' : ''}</Banner> : null}
      {!historyOnly && <>{data.accounts.length ? data.accounts.map((account) => <AccountAllowance key={account.account || account.profile} account={account} now={now} refreshFailed={!!error} windows={data.windows.filter((window) => account.account ? window.account === account.account : !window.account && window.profile === account.profile)} />) : <p className="text-[13px] text-muted">No allowance snapshot available. Plan utilization is unknown.</p>}
      <section aria-label="Outside-cap spending" className="space-y-3 border-t border-hairline pt-5">
        <h3 className="m-0 text-[14px] font-medium text-primary">Outside-cap spending</h3>
        <div className="grid gap-6 sm:grid-cols-2">
          <StatTile label="Provider-reported overage" value={data.billing?.reportedOverageUsd !== undefined ? money(data.billing.reportedOverageUsd) : data.billing?.reportedOverageCredits !== undefined ? creditsLabel(data.billing.reportedOverageCredits) : 'Unavailable'} note={data.billing?.reportedOverageUsd === undefined && data.billing?.reportedOverageCredits !== undefined ? 'Credits used beyond the included allowance in the current credit period.' : 'Actual reported overage; shown only when supplied by the provider.'} />
          <StatTile label="Estimated outside-cap usage" value={data.billing?.estimatedOverageUsd !== undefined ? money(data.billing.estimatedOverageUsd) : 'Unavailable'} note={data.billing?.creditPriceUsd !== undefined ? `Reported credits at ${creditPriceLabel(data.billing.creditPriceUsd)} each (CODEX_CREDIT_USD); an estimate, not an invoice.` : 'API-price estimate for usage explicitly identified as outside the cap.'} />
        </div>
        <p className="m-0 text-[12px] text-muted">{data.billing?.note || 'Local history does not establish which requests were included or paid. No dollar amount is inferred from tokens or exhausted plan windows.'}</p>
        {data.billing && (data.billing.unclassifiedRequests > 0 || data.billing.unclassifiedSessions > 0) ? <p className="m-0 text-[12px] text-muted">Billing classification unavailable for {fmtInt.format(data.billing.unclassifiedRequests)} recorded requests across {fmtInt.format(data.billing.unclassifiedSessions)} sessions.</p> : null}
        {data.billing?.outsideCapRequests ? <p className="m-0 text-[12px] text-secondary">{fmtInt.format(data.billing.outsideCapRequests)} requests explicitly identified as outside the cap.</p> : null}
        {data.billing?.estimatedOverageUsd !== undefined && /^https?:\/\//.test(data.billing.pricingSourceUrl) ? <p className="m-0 text-[12px] text-muted"><a href={data.billing.pricingSourceUrl} target="_blank" rel="noreferrer">API pricing source</a>{data.billing.pricingCheckedAt ? ` · checked ${data.billing.pricingCheckedAt}` : ''}</p> : null}
      </section>
      </>}
      <section aria-label="Codex recorded activity" className="space-y-4 border-t border-hairline pt-5">
        <div className="flex items-center gap-2"><h3 className="m-0 text-[14px] font-medium text-primary">Recorded activity</h3><Badge tone="neutral">All indexed history</Badge></div>
        {data.sessions === 0 ? <p className="text-[13px] text-muted">No Codex sessions indexed yet. Import history in Settings or wait for the next indexing pass.</p> : <>
          <div className="grid grid-cols-2 gap-6 lg:grid-cols-3">
            <StatTile label="Sessions" value={fmtInt.format(data.sessions)} note={`Requests: ${observedTotal(data.requests, data.usageComplete, fmtInt.format)}`} />
            <StatTile label="Recorded tokens" value={data.tokens ? observedTotal(data.tokens.input + data.tokens.output + data.tokens.cacheRead + data.tokens.cacheCreation, data.usageComplete, compactInt) : 'Unavailable'} note="Input, output and cache tokens; no dollar valuation." />
            <StatTile label="Tool calls" value={observedTotal(data.toolCalls, data.toolsComplete, fmtInt.format)} note={`Recorded errors: ${observedTotal(data.errorCount, data.errorsComplete, fmtInt.format)}`} />
          </div>
          <div className="flex flex-wrap gap-x-6 gap-y-2 text-[12px] text-secondary">
            <span>Input: {data.tokens ? observedTotal(data.tokens.input, data.usageComplete, compactInt) : 'Unavailable'}</span>
            <span>Output: {data.tokens ? observedTotal(data.tokens.output, data.usageComplete, compactInt) : 'Unavailable'}</span>
            <span>Cache read: {data.tokens ? observedTotal(data.tokens.cacheRead, data.usageComplete, compactInt) : 'Unavailable'}</span>
            <span>Reasoning: {observedTotal(data.reasoningTokens, data.usageComplete, compactInt)} <span className="text-muted">(included in output)</span></span>
            <span>Session duration: {observedTotal(data.durationMs / 1000, data.durationComplete, ageText)}</span>
          </div>
          {!data.usageComplete ? <div className="space-y-3">
            <Banner tone="warn">{data.usageGaps.length ? `Token totals available for ${fmtInt.format(data.sessions - data.usageGaps.length)} of ${fmtInt.format(data.sessions)} sessions. ${fmtInt.format(data.usageGaps.length)} ${data.usageGaps.length === 1 ? 'session has' : 'sessions have'} unknown usage.` : 'Token history is incomplete.'} Recorded totals exclude unknown usage. Live plan limits are fetched separately.</Banner>
            {data.usageGaps.length ? <details className="text-[12px] text-secondary">
              <summary className="cursor-pointer">View affected sessions</summary>
              <ul className="mt-3 space-y-3 pl-5">{data.usageGaps.map((gap) => <li key={gap.sessionId}>
                <Link className="hover:underline" href={withProfile(`/sessions?id=${encodeURIComponent(gap.sessionId)}`, gap.profile, 'codex')}>
                  {gap.profileName || gap.profile || 'Codex'} · {gap.startedAt > 0 ? new Date(gap.startedAt * 1000).toLocaleString() : 'Session date unavailable'}
                </Link>
                <p className="mb-0 mt-1 text-muted">{gap.reason}</p>
              </li>)}</ul>
              <p className="mb-0 mt-3 text-muted">Import history in Settings to retry reading available logs. Reimporting cannot recreate token records that Codex never saved.</p>
            </details> : null}
          </div> : null}
          {!data.toolsComplete || !data.errorsComplete || !data.durationComplete ? <p className="m-0 text-[12px] text-muted">Activity coverage is incomplete. A + marks a partial total; unavailable means no known total.</p> : null}
        </>}
        <p className="m-0 text-[12px] text-muted">{fmtInt.format(data.configuredProfiles)} configured Codex profiles · {data.lastIndexedAt > 0 ? `indexed ${agoText(data.lastIndexedAt, now)}` : 'index freshness unknown'} · {data.lastActivityAt > 0 ? `last activity ${agoText(data.lastActivityAt, now)}` : 'no activity observed'}</p>
      </section>
    </> : null}
  </section>;
}
