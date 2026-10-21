'use client';
import Link from 'next/link';
import { usePathname } from 'next/navigation';
import type { ReactNode } from 'react';
import type { AccountSnapshot, AllowanceWindow, BillingSnapshot } from '@/src/gen/claudeobs/v1/obs_pb';
import { accountFreshness, accountLabel, accountPlan, accountScope, billingLabel, confirmationAge, creditPriceLabel, creditsLabel, known, providerName, scopeHref, windowState, type DashboardScope } from '@/src/lib/dashboard';
import { providerMark } from '@/src/lib/providers';
import { agoText, countdown } from '@/src/lib/format';
import { useNow } from '@/src/lib/useNow';
import { allowanceTone } from '@/src/lib/allowance';

export function PageState({ title, children, error = false }: { title: string; children?: ReactNode; error?: boolean }) {
  return <div className="obs-empty" role={error ? 'alert' : 'status'}><h2>{title}</h2>{children && <p>{children}</p>}</div>;
}
export function LoadingPage({ title = 'Loading account data…' }: { title?: string }) {
  return <div className="obs-page" role="status" aria-label={title}><p className="mb-6 text-muted">{title}</p><div className="obs-skeleton" /><div className="obs-skeleton" /></div>;
}
export function Alert({ children, tone = 'warning' }: { children: ReactNode; tone?: 'warning' | 'error' | 'info' }) {
  return <div className="obs-alert" data-tone={tone} role={tone === 'error' ? 'alert' : undefined}>{children}</div>;
}
export function ProviderMark({ provider }: { provider: string }) {
  return <span className="obs-provider-mark" data-provider={provider} aria-label={providerName(provider)}>{providerMark(provider)}</span>;
}
export function Freshness({ account }: { account: AccountSnapshot }) {
  const now = useNow(); const freshness = accountFreshness(account, now);
  const title = `${freshness.detail}${known(freshness.at) ? ` ${freshness.timeLabel}: ${new Date(freshness.at * 1000).toLocaleString()}.` : ''}`;
  return <span className="obs-status" data-state={freshness.state} title={title}><span className="obs-dot" /><span>{freshness.label}{known(freshness.at) ? ` · ${freshness.timeLabel} ${confirmationAge(freshness.at, now, freshness.state === 'stale')}` : ''}</span></span>;
}
export function WindowMeter({ window, detailed = false }: { window?: AllowanceWindow; detailed?: boolean }) {
  const now = useNow();
  if (!window) return <div className="obs-muted text-xs">Not provided by this account</div>;
  const state = windowState(window, now); const used = window.usedPercent; const remaining = window.remainingPercent;
  const unconfirmed = state === 'Confirmation unavailable' || state === 'Reading unavailable';
  const confirmed = known(window.confirmedAt) && window.confirmedAt <= now + 60 ? window.confirmedAt : undefined;
  const timing = known(confirmed) ? `Last confirmed ${confirmationAge(confirmed, now, state !== 'Confirmed')}` : known(window.observedAt) ? `${window.source.toLowerCase() === 'cache' ? 'Last value change' : 'Last observation'} ${agoText(window.observedAt, now)}` : '';
  return <div className="obs-window" data-usage={allowanceTone(used)}>
    <div className="obs-window-value"><strong>{known(used) ? `${Math.round(used)}%` : 'Unknown'}</strong>{known(used) && <span>used</span>}<small>{known(remaining) ? `${Math.round(remaining)}% remaining` : 'Remaining unknown'}</small></div>
    <div className="obs-meter" role={known(used) ? 'meter' : undefined} aria-label={`${window.label}: allowance consumed`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={known(used) ? Math.min(100, Math.max(0, used)) : undefined}><span style={{ width: known(used) ? `${Math.min(100, Math.max(0, used))}%` : 0 }} /></div>
    <div className="obs-reset">{known(window.resetsAt) ? window.resetsAt <= now ? 'Reset passed; awaiting confirmation' : countdown(window.resetsAt, now) : 'Reset time unavailable'}{known(window.resetsAt) && <time dateTime={new Date(window.resetsAt * 1000).toISOString()}>{new Date(window.resetsAt * 1000).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' })}</time>}</div>
    {(detailed || state !== 'Confirmed') && <p className="obs-status mt-2" data-state={state === 'Confirmed' ? 'fresh' : unconfirmed ? 'unknown' : 'stale'}><span>{state}{timing ? ` · ${timing}` : ''}{window.error ? ` · ${window.error}` : ''}</span></p>}
  </div>;
}
export function BillingValue({ billing, showEstimate = false }: { billing?: BillingSnapshot; showEstimate?: boolean }) {
  const now = useNow();
  // Credits are provider-reported, so they show whatever the caller asked;
  // their dollar figure is always named as an estimate at the set price.
  if (!known(billing?.reportedOverageUsd) && known(billing?.reportedOverageCredits)) {
    const credits = creditsLabel(billing.reportedOverageCredits);
    return <div><div className="obs-billing-value">{billingLabel(billing)}</div><div className="obs-billing-note">{known(billing.estimatedOverageUsd) && known(billing.creditPriceUsd) ? `Estimated from ${credits} at ${creditPriceLabel(billing.creditPriceUsd)} each` : 'Provider-reported credits; no price per credit set'}</div><div className="obs-billing-note" title={known(billing.periodResetsAt) ? new Date(billing.periodResetsAt * 1000).toLocaleString() : undefined}>{known(billing.periodResetsAt) ? `Credit period ${countdown(billing.periodResetsAt, now)}` : billing.period}</div><div className="obs-billing-note">{billing.note}</div>{known(billing.observedAt) && <div className="obs-billing-note" title={new Date(billing.observedAt * 1000).toLocaleString()}>{billing.state === 'stale' ? 'Stale billing · ' : ''}Observed {agoText(billing.observedAt, now)}</div>}</div>;
  }
  const estimated = showEstimate && !known(billing?.reportedOverageUsd) && known(billing?.estimatedOverageUsd);
  if (estimated) return <div><div className="obs-billing-value">{billingLabel(billing, true)}</div><div className="obs-billing-note" title={billing?.note}>Estimated spend · {billing?.period}</div><div className="obs-billing-note">{billing?.note}</div>{known(billing?.observedAt) && <div className="obs-billing-note" title={new Date(billing.observedAt * 1000).toLocaleString()}>{billing?.state === 'stale' ? 'Stale collection · ' : ''}Collected {agoText(billing.observedAt, now)}</div>}</div>;
  return <div><div className="obs-billing-value" data-text={!known(billing?.reportedOverageUsd)}>{billingLabel(billing)}</div><div className="obs-billing-note">{known(billing?.reportedOverageUsd) ? 'Provider-reported overage' : billing?.enabled === false ? 'Provider reports extra usage off' : billing?.note || 'Billing data unavailable'}</div>{known(billing?.reportedOverageUsd) && <div className="obs-billing-note">{billing?.period || 'Reporting period unknown'} · USD</div>}{known(billing?.observedAt) && <div className="obs-billing-note" title={new Date(billing.observedAt * 1000).toLocaleString()}>{billing?.state === 'stale' ? 'Stale billing · ' : ''}Observed {agoText(billing.observedAt, now)}</div>}{billing?.state === 'stale' && !known(billing.observedAt) && <div className="obs-billing-note text-warn-text">Billing reading is stale</div>}</div>;
}
export function AccountHeading({ account }: { account: AccountSnapshot }) {
  return <header className="obs-heading"><div><div className="obs-eyebrow">{providerName(account.provider)} account</div><h1>{accountLabel(account)}</h1><p>{[accountPlan(account), `${account.profiles.length} linked profile${account.profiles.length === 1 ? '' : 's'}`].filter(Boolean).join(' · ')}</p></div><Freshness account={account} /></header>;
}
export function ScopeTabs({ scope }: { scope: DashboardScope }) {
  const path = usePathname();
  if (!scope.accountKey) return null;
  return <nav className="obs-tabs" aria-label="Account sections">{[['/', 'Overview'], ['/sessions', 'Activity'], ['/insights', 'Insights']].map(([href, label]) => <Link key={href} href={scopeHref(href, scope)} aria-current={path === href ? 'page' : undefined}>{label}</Link>)}</nav>;
}
export function AccountLink({ account }: { account: AccountSnapshot }) {
  return <Link href={scopeHref('/', accountScope(account))} className="obs-account-name">{accountLabel(account)} <span aria-hidden="true">↗</span></Link>;
}
