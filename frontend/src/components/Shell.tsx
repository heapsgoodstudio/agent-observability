'use client';

import { Fragment, Suspense, useCallback, useEffect, useState, type ReactNode } from 'react';
import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { applyTheme, nextTheme, readTheme, type Theme } from '@/lib/theme';
import { obsClient } from '@/src/lib/client';
import { IconInsights, IconOverview, IconSessions } from '@/src/components/icons';
import { usePoll } from '@/src/lib/usePoll';
import { useScope } from '@/src/lib/useScope';
import { accountLabel, accountScope, billedTotal, providerName, scopeHref, sidebarFigure, sidebarGroups } from '@/src/lib/dashboard';
import { providerMark } from '@/src/lib/providers';
import { AccountProvider, useAccounts } from './AccountContext';
import type { AccountSnapshot } from '@/src/gen/claudeobs/v1/obs_pb';

const NAV = [{ href: '/', label: 'Overview', Icon: IconOverview }, { href: '/sessions', label: 'Activity', Icon: IconSessions }, { href: '/insights', label: 'Insights', Icon: IconInsights }];
export function Shell({ children }: { children: ReactNode }) {
  return <AccountProvider><Suspense fallback={<div className="obs-page">Loading workspace…</div>}><ShellChrome>{children}</ShellChrome></Suspense></AccountProvider>;
}
function ShellChrome({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  const scope = useScope();
  const snapshot = useAccounts();
  const accounts = (snapshot.data?.accounts ?? []).toSorted((a, b) => a.provider.localeCompare(b.provider) || accountLabel(a).localeCompare(accountLabel(b)));
  const selected = accounts.find((a) => a.key === scope.accountKey);
  const [theme, setTheme] = useState<Theme>('dark');
  useEffect(() => setTheme(readTheme()), []);
  const health = usePoll(useCallback(() => obsClient.getHealth({}), []));
  const roster = usePoll(useCallback(() => obsClient.listProfiles({}), []));
  const groups = sidebarGroups(accounts, roster.data?.profiles ?? []);
  const healthy = health.error ? undefined : health.data?.healthy;
  const pageName = pathname === '/' ? 'Overview' : pathname === '/sessions' ? 'Activity' : pathname === '/insights' ? 'Insights' : pathname === '/settings' ? 'Settings' : pathname === '/health' ? 'Collection health' : 'Usage history';
  const accountPath = pathname === '/sessions' ? '/sessions' : pathname === '/insights' ? '/insights' : '/';
  return <div className="obs-app">
    <a href="#main-content" className="obs-skip">Skip to content</a>
    <aside className="obs-sidebar" aria-label="Workspace navigation">
      <Link href="/" className="obs-brand"><span className="obs-brand-mark" aria-hidden="true"><svg width="23" height="24" viewBox="0 0 28 28"><path d="M4 18V10h4v8M12 22V6h4v16M20 16v-4h4v4" fill="currentColor" /></svg></span><span>Agent<small>Observability</small></span></Link>
      <nav className="obs-nav" aria-label="All accounts">
        {NAV.map(({ href, label, Icon }) => <Link key={href} href={href} aria-current={pathname === href && !scope.accountKey && !scope.profile ? 'page' : undefined}><Icon /><span>{label}</span></Link>)}
      </nav>
      <div className="obs-sidebar-accounts"><div className="obs-side-heading">Accounts <span>{snapshot.data ? groups.reduce((n, g) => n + g.accounts.length, 0) : '…'}</span></div>
        {!snapshot.data && <p className="px-3 text-xs text-muted">{snapshot.error ? 'Accounts unavailable' : 'Loading accounts…'}</p>}
        {snapshot.data && !groups.length && <Link href="/settings" className="obs-account-link">Add a connection</Link>}
        {groups.map(({ provider, accounts: members, hidden }) => <div key={provider}>
          <div className="obs-provider-heading"><span aria-hidden="true">{providerMark(provider)}</span>{providerName(provider)}</div>
          {members.length ? <nav aria-label={`${providerName(provider)} accounts`}>{members.map((a) => {
            return <Link key={a.key} className="obs-account-link" href={scopeHref(accountPath, accountScope(a))} aria-current={scope.accountKey === a.key ? 'page' : undefined} title={accountLabel(a)}><span>{accountLabel(a)}</span><SidebarFigure account={a} /></Link>;
          })}</nav> : hidden ? <Link href="/settings" className="obs-account-link obs-muted">{hidden} hidden</Link> : <p className="obs-account-link obs-muted m-0" title="Accounts appear with the first request to a provider.">No usage yet</p>}
        </div>)}
      </div>
      <div className="obs-footer"><div className="obs-side-heading">Local workspace</div>
        <Link href="/settings" aria-current={pathname === '/settings' ? 'page' : undefined}><span aria-hidden="true">⚙</span>Settings</Link>
        <Link href="/health" aria-current={pathname === '/health' ? 'page' : undefined}><span className={`obs-dot ${healthy === true ? 'text-ok' : healthy === false ? 'text-warn' : 'text-muted'}`} /><span>{healthy === true ? 'Collection healthy' : healthy === false ? 'Collection needs attention' : 'Collection health'}</span></Link>
        <button onClick={() => { const next = nextTheme(theme); applyTheme(next); setTheme(next); }} aria-label={`Theme: ${theme}. Change theme.`}>◐ <span className="capitalize">{theme} theme</span></button>
      </div>
    </aside>
    <div className="obs-workspace"><div className="obs-topbar"><div className="obs-crumbs"><IconOverview size={14} /><span>Workspace</span><span aria-hidden="true">/</span>{scope.accountKey && <><span>{selected ? accountLabel(selected) : 'Account'}</span><span aria-hidden="true">/</span></>}<span>{pageName}</span></div><div className="obs-topbar-end"><span className="obs-local">Local observability</span>{snapshot.data && <BilledTotal accounts={snapshot.data.accounts} />}</div></div>
      <main id="main-content" tabIndex={-1}>{children}</main>
    </div>
  </div>;
}

function SidebarFigure({ account }: { account: AccountSnapshot }) {
  const { text, title, windows } = sidebarFigure(account);
  return <small title={title}>{windows ? windows.map((window, i) => <Fragment key={window.id}>{i > 0 && ' / '}<span data-usage={window.tone}>{window.text}</span></Fragment>) : text}</small>;
}

function BilledTotal({ accounts }: { accounts: AccountSnapshot[] }) {
  const total = billedTotal(accounts);
  if (!total) return null;
  return <span className="obs-billed-total" title={total.title}><span>Billed MTD</span><strong data-stale={total.stale || undefined}>{total.text}</strong></span>;
}
