'use client';
import { Suspense, useState } from 'react';
import Link from 'next/link';
import { useRouter, useSearchParams } from 'next/navigation';
import { useAccounts } from '@/src/components/AccountContext';
import { AccountHeading, AccountLink, Alert, BillingValue, Freshness, LoadingPage, PageState, ProviderMark, ScopeTabs, WindowMeter } from '@/src/components/AccountUI';
import { BillingHistory, AllowanceTrace, useAccountHistory } from '@/src/components/AccountHistory';
import { Section } from '@/src/components/Section';
import { accountLabel, accountPlan, accountScope, known, providerName, scopeHref, windowRoles, matchesAccountScope, type DashboardScope, costRecorded, tokensLabel } from '@/src/lib/dashboard';
import { ALLOWANCE_BANDS, allowanceTone } from '@/src/lib/allowance';
import { detectedHarness } from '@/src/lib/providers';
import type { AccountSnapshot } from '@/src/gen/claudeobs/v1/obs_pb';
import { useScope } from '@/src/lib/useScope';
import { useScrollTarget } from '@/src/lib/scrollTarget';
import { PaygAccountSpend } from '@/src/components/PaygAccountSpend';

export function Overview() { return <Suspense fallback={<LoadingPage />}><OverviewRoute /></Suspense>; }
function OverviewRoute() {
  const scope = useScope();
  const snapshot = useAccounts();
  const router = useRouter(); const params = useSearchParams();
  const provider = scope.provider; const sort = params.get('sort') || 'name';
  function setProvider(value: string) { const next = new URLSearchParams(params.toString()); if (value) next.set('provider', value); else next.delete('provider'); next.delete('profile'); next.delete('accountKey'); router.replace(`/${next.size ? `?${next}` : ''}`, { scroll: false }); }
  function setSort(value: string) { const next = new URLSearchParams(params.toString()); next.set('sort', value); router.replace(`/?${next}`, { scroll: false }); }
  useScrollTarget();
  if (!snapshot.data) return snapshot.error ? <div className="obs-page"><PageState title="Accounts could not be loaded" error>{snapshot.error}</PageState><Link className="obs-button" href="/health">Check collection health</Link></div> : <LoadingPage />;
  if (scope.accountKey) {
    const account = snapshot.data.accounts.find((a) => matchesAccountScope(a, scope));
    return account ? <AccountOverview key={account.key} account={account} scope={scope} error={snapshot.error} /> : <div className="obs-page"><PageState title="Account unavailable">This account is not in the current connection roster. Select another account or check Settings.</PageState><Link href="/settings" className="obs-button">Manage connections</Link></div>;
  }
  // A hidden account's own page still opens (above), but lists leave it out.
  const visible = snapshot.data.accounts.filter((a) => !a.hidden);
  const accounts = visible.filter((a) => matchesAccountScope(a, scope)).toSorted((a, b) => {
    if (sort === 'remaining') return (windowRoles(b).current?.remainingPercent ?? -Infinity) - (windowRoles(a).current?.remainingPercent ?? -Infinity) || accountLabel(a).localeCompare(accountLabel(b));
    if (sort === 'reset') return (windowRoles(a).current?.resetsAt ?? Infinity) - (windowRoles(b).current?.resetsAt ?? Infinity) || accountLabel(a).localeCompare(accountLabel(b));
    return a.provider.localeCompare(b.provider) || accountLabel(a).localeCompare(accountLabel(b));
  });
  return <div className="obs-page"><header className="obs-heading"><div><div className="obs-eyebrow">Your agent workspace</div><h1>Overview</h1><p>Every account. Its allowance, next reset, and reported overage.</p></div><span className="obs-status"><span className="obs-dot" />Source freshness shown per account</span></header>
    {snapshot.error && <Alert tone="error">Refresh failed. Showing the last response. {snapshot.error}</Alert>}
    {snapshot.data.errors.map((e, i) => <Alert key={i}>{e.section}: {e.error}</Alert>)}
    <Section id="meters" wide><div>
      <div className="obs-toolbar mt-0"><h2 className="obs-section-title">Accounts<span className="obs-count">{accounts.length}</span></h2><div className="flex flex-wrap gap-3"><div className="obs-segments" aria-label="Provider filter">{['', ...new Set(visible.map((a) => a.provider))].map((p) => <button key={p} aria-pressed={provider === p} onClick={() => setProvider(p)}>{p ? providerName(p) : 'All providers'}</button>)}</div><select className="obs-input" aria-label="Sort accounts" value={sort} onChange={(e) => setSort(e.target.value)}><option value="name">Provider / name</option><option value="remaining">Most remaining</option><option value="reset">Next reset</option></select></div></div>
      {!accounts.length ? <div className="obs-panel"><PageState title={snapshot.data.accounts.length ? 'No accounts match this view' : 'Connect your first account'}>{snapshot.data.accounts.length ? 'Choose a different provider or open the all-accounts overview.' : 'Add a Claude or Codex profile directory in Settings. Account identity and allowance appear when the provider reports them.'}</PageState><div className="px-6 pb-6"><Link className="obs-button" href={snapshot.data.accounts.length ? '/' : '/settings'}>{snapshot.data.accounts.length ? 'All accounts' : 'Add a connection'}</Link></div></div> : <div className="obs-panel obs-account-table"><div className="obs-account-head" aria-hidden="true"><span>Account</span><span>Current window</span><span>Weekly window</span><span>Overage</span></div>{accounts.map((account) => <AccountRow key={account.key} account={account} />)}</div>}
      <div className="obs-caption"><span>Each percentage belongs to its account’s own allowance.</span><span>Additional model limits are available in account details.</span></div>
    </div></Section>
    <Section id="curve" wide><OverviewLower accounts={accounts} /></Section>
  </div>;
}
export function AccountRow({ account }: { account: AccountSnapshot }) {
  const roles = windowRoles(account);
  return <article className="obs-account-row" aria-label={accountLabel(account)}>
    <div className="obs-account-info"><ProviderMark provider={account.provider} /><div><AccountLink account={account} /><p className="obs-account-meta">{accountPlan(account) || providerName(account.provider)} · {account.profiles.length} profile{account.profiles.length === 1 ? '' : 's'}</p><Freshness account={account} /></div></div>
    {detectedHarness(account.provider) ? <><div><p className="obs-account-meta">Allowance</p><div className="obs-muted text-xs">None. Billed per token.</div></div><div /></> : roles.shared ? <div className="obs-shared-window"><div><p className="obs-account-meta">{roles.current?.label}</p><WindowMeter window={roles.current} /></div><div className="obs-shared-note"><strong>One shared window</strong>The provider reports the same limit for current and weekly allowance.</div></div> : <><div><span className="sr-only">Current window</span><p className="obs-account-meta">{roles.current?.label}</p><WindowMeter window={roles.current} /></div><div><span className="sr-only">Weekly window</span><p className="obs-account-meta">{roles.weekly?.label}</p><WindowMeter window={roles.weekly} /></div></>}
    {detectedHarness(account.provider) && !costRecorded(account)
      ? <div><div className="obs-billing-value">{known(account.monthToDateTokens) ? tokensLabel(account.monthToDateTokens) : 'Unknown'}</div><div className="obs-billing-note">Tokens this month · no cost recorded</div></div>
      : <BillingValue billing={account.billing} showEstimate={detectedHarness(account.provider)} />}
  </article>;
}
function OverviewLower({ accounts }: { accounts: AccountSnapshot[] }) {
  const [selected, setSelected] = useState('');
  // Billing history is provider-reported overage. omp accounts have none;
  // their estimated spend lives on the account page.
  const billed = accounts.filter((a) => !detectedHarness(a.provider));
  const account = billed.find((a) => a.key === selected) ?? billed.find((a) => known(a.billing?.reportedOverageUsd)) ?? billed[0];
  const attention = accounts.filter((a) => a.state !== 'fresh' || a.error || a.windows.some((w) => ['warn', 'danger'].includes(allowanceTone(w.usedPercent))));
  return <div className="obs-lower-grid"><div>{account ? <><div className="obs-toolbar mt-0"><h2 className="obs-section-title">Billing history</h2><select className="obs-input max-w-[280px]" aria-label="Billing history account" value={account.key} onChange={(e) => setSelected(e.target.value)}>{billed.map((a) => <option value={a.key} key={a.key}>{providerName(a.provider)} · {accountLabel(a)}</option>)}</select></div><BillingHistory key={account.key} account={account} /></> : null}</div>
    <section className="obs-panel self-start"><div className="obs-panel-heading"><h2>Needs attention</h2><span className="obs-count">{attention.length}</span></div><div className="obs-panel-body">{attention.length ? attention.map((a) => <div className="obs-attention" key={a.key}><span className="obs-dot mt-2 text-warn" /><div><Link href={scopeHref('/', accountScope(a))}>{accountLabel(a)}</Link><p>{a.error || (a.state !== 'fresh' ? 'Allowance is unavailable or stale. Check this connection before choosing it for work.' : `At least one reported window has used ${ALLOWANCE_BANDS[0].at}% or more of its allowance.`)}</p></div></div>) : <p className="obs-muted text-[13px]">{accounts.length ? 'No account warnings in the latest response.' : 'Connect an account to see its status.'}</p>}</div><p className="obs-panel-note"><Link className="text-accent-text" href="/health">Review collection health →</Link></p></section>
  </div>;
}
function AccountOverview({ account, scope, error }: { account: AccountSnapshot; scope: DashboardScope; error: string | null }) {
  const history = useAccountHistory(account.key, 7);
  const roles = windowRoles(account); const mainWindows = [roles.current, roles.weekly].filter((w, i, list) => w && list.findIndex((x) => x?.id === w.id) === i);
  const visibleWindows = account.provider === 'claude' ? [...mainWindows, ...roles.extra] : mainWindows;
  if (detectedHarness(account.provider)) return <div className="obs-page"><AccountHeading account={account} /><ScopeTabs scope={scope} />
    {error && <Alert tone="error">Refresh failed. Showing the last response. {error}</Alert>}{account.error && <Alert>{account.error}</Alert>}
    <Section id="meters" wide><PaygAccountSpend account={account} /></Section>
    <Section id="curve" wide><section className="obs-panel"><div className="obs-panel-heading"><h2>Connected profiles</h2><Link href="/settings" className="obs-button">Manage</Link></div><div className="obs-panel-body">{account.profiles.map((p) => <div className="obs-attention" key={p.profile}><div><h3 className="text-[13px] font-medium">{p.displayName || p.profile}</h3><p>{p.enabled ? 'Monitoring enabled' : 'Monitoring disabled'}</p></div></div>)}<p className="obs-muted text-xs mt-4">{account.coverageNote}</p><Link className="obs-button mt-5" href={scopeHref('/sessions', scope)}>Explore activity →</Link></div></section></Section>
  </div>;
  return <div className="obs-page"><AccountHeading account={account} /><ScopeTabs scope={scope} />
    {error && <Alert tone="error">Refresh failed. Showing the last response. {error}</Alert>}{account.error && <Alert>{account.error}</Alert>}
    {account.identityState === 'unresolved' && <Alert>Account identity has not been reported. This view is scoped to its configured profile.</Alert>}
    <Section id="meters" wide><div><div className="obs-toolbar mt-0"><h2 className="obs-section-title">Current allowance</h2><span className="obs-muted text-xs">{roles.shared ? 'Current and weekly share one reported window' : 'Provider-reported reset windows'}</span></div>
      <div className="obs-window-grid">{visibleWindows.map((w) => w && <article key={w.id} className="obs-panel obs-window-card"><h2>{w.label || w.kind || w.id}{w.model && <span className="obs-muted ml-2">{w.model}</span>}</h2><WindowMeter window={w} detailed /><AllowanceTrace history={history.data} windowId={w.id} resetAt={w.resetsAt} durationSeconds={w.durationSeconds} /></article>)}</div>
      {!visibleWindows.length && <div className="obs-panel"><PageState title="Allowance not available">This connection has no current allowance windows. {account.error || 'Check the connection and collection status in Settings.'}</PageState></div>}
      {history.error && <Alert>Allowance history could not refresh. {history.error}</Alert>}{history.data?.errors.map((e) => <Alert key={e.section}>{e.section}: {e.error}</Alert>)}
      {account.provider !== 'claude' && roles.extra.length > 0 && <details className="obs-panel mt-5"><summary className="cursor-pointer p-5 text-secondary">{roles.extra.length} additional model or bucket limits</summary><div className="obs-window-grid px-5 pb-5">{roles.extra.map((w) => <div key={w.id}><h3 className="mb-3">{w.label || w.id}{w.model && ` · ${w.model}`}</h3><WindowMeter window={w} detailed /></div>)}</div></details>}
      {(account.includedUsageAllowed !== undefined || account.codexBuckets.length > 0) && <details className="obs-panel mt-5"><summary className="cursor-pointer p-5 text-secondary">Provider permissions and credits</summary><div className="px-5 pb-5 space-y-4 text-[13px] text-secondary"><p>Included usage permission: {account.includedUsageAllowed === true ? 'Allowed' : account.includedUsageAllowed === false ? 'Not allowed' : 'Not provided'}.</p>{known(account.resetCreditsAvailable) && <p>Reset credits available: {account.resetCreditsAvailable} provider units.</p>}{account.codexBuckets.map((b) => <div key={b.limitId}><h3 className="font-medium text-primary">{b.name || b.limitId}{b.normalModelSlug ? ` · ${b.normalModelSlug}` : ''}</h3>{b.credits && <p>Credits: {b.credits.unlimited ? 'Unlimited' : b.credits.balance !== undefined ? `${b.credits.balance} provider units` : b.credits.hasCredits ? 'Available; balance not reported' : 'Unavailable'}.</p>}{b.individualLimit && <p>Individual usage: {b.individualLimit.used || 'Unknown'} / {b.individualLimit.limit || 'Unknown'} provider units. {b.individualLimit.remainingPercent}% remaining.</p>}{b.spendControlReached !== undefined && <p>Spend control: {b.spendControlReached ? 'Reached' : 'Not reached'}.</p>}{b.rateLimitReachedType && <p>Limit status: {b.rateLimitReachedType}</p>}</div>)}<p className="obs-muted">Credits retain provider units. They are not converted to dollars.</p></div></details>}
    </div></Section>
    <Section id="curve" wide><div className="obs-lower-grid"><BillingHistory account={account} /><section className="obs-panel self-start"><div className="obs-panel-heading"><h2>Connected profiles</h2><Link href="/settings" className="obs-button">Manage</Link></div><div className="obs-panel-body">{account.profiles.map((p) => <div className="obs-attention" key={p.profile}><div><h3 className="text-[13px] font-medium">{p.displayName || p.profile}</h3><p>{p.enabled ? 'Monitoring enabled' : 'Monitoring disabled'}</p></div></div>)}<p className="obs-muted text-xs mt-4">{account.coverageNote}</p><Link className="obs-button mt-5" href={scopeHref('/sessions', scope)}>Explore activity →</Link><p className="obs-muted text-xs mt-4">Recorded token history is separate from live allowance and reset information.</p></div></section></div></Section>
  </div>;
}
