'use client';
import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { useScope } from '@/src/lib/useScope';
import { scopeHref } from '@/src/lib/dashboard';
import { useAccounts } from '@/src/components/AccountContext';
import { AccountHeading, ScopeTabs, PageState, LoadingPage } from '@/src/components/AccountUI';
import { Findings } from './Findings';
import { Advice } from './Advice';

export function Insights() {
  const scope = useScope(); const params = useSearchParams();
  const roster = useAccounts();
  const account = roster.data?.accounts.find((a) => scope.accountKey ? a.key === scope.accountKey : scope.profile ? a.profiles.some((p) => p.profile === scope.profile) : false);
  const tab = params.get('tab') === 'advice' ? 'advice' : 'findings';
  if ((scope.accountKey || scope.profile) && !roster.data) return roster.error ? <div className="obs-page"><PageState title="Insight scope could not be checked" error>{roster.error}</PageState></div> : <LoadingPage title="Checking insight scope…" />;
  if ((scope.accountKey || scope.profile) && (!account || (scope.provider && account.provider !== scope.provider) || (scope.profile && !account.profiles.some((p) => p.profile === scope.profile)))) return <div className="obs-page"><PageState title="Insight scope unavailable">Choose a connected account or profile from the sidebar.</PageState></div>;
  return <div className="obs-page">{account ? <AccountHeading account={account} /> : <header className="obs-heading"><div><div className="obs-eyebrow">Patterns and recommendations</div><h1>Insights</h1><p>Evidence from your recorded activity, with a report for each selected scope.</p></div></header>}<ScopeTabs scope={scope} />
    <div className="obs-toolbar mt-0"><div className="obs-segments"><Link href={scopeHref('/insights', scope)} aria-current={tab === 'findings' ? 'page' : undefined}>Findings</Link><Link href={scopeHref('/insights?tab=advice', scope)} aria-current={tab === 'advice' ? 'page' : undefined}>Advice report</Link></div><span className="obs-muted text-xs">{scope.accountKey ? 'Selected account' : scope.profile ? 'Selected profile' : scope.provider ? 'Selected provider' : 'All recorded activity'}</span></div>
    {tab === 'advice' ? <Advice key={`${scope.provider}/${scope.accountKey}/${scope.profile}`} /> : <Findings />}
  </div>;
}
