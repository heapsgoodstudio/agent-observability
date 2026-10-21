'use client';
import { Suspense, useCallback } from 'react';
import Link from 'next/link';
import { useRouter, useSearchParams } from 'next/navigation';
import { obsClient } from '@/src/lib/client';
import { usePoll } from '@/src/lib/usePoll';
import { useScope } from '@/src/lib/useScope';
import { useAccounts } from '@/src/components/AccountContext';
import { accountLabel, providerName, scopeHref } from '@/src/lib/dashboard';
import { AccountHeading, Alert, LoadingPage, PageState, ScopeTabs } from '@/src/components/AccountUI';
import { detectedHarness, indexedProvider, inferenceProviders, observedTotal, providerOf, sessionUsageKnown } from '@/src/lib/providers';
import { ageText, compactInt, fmtInt } from '@/src/lib/format';
import { filterSessions, projectLabel, sortSessions, totalSessionTokens, type SessionSort } from '@/src/lib/sessions';
import { Badge } from '@/src/components/ui/Badge';

export function Sessions() { return <Suspense fallback={<LoadingPage title="Loading activity…" />}><Activity /></Suspense>; }
function Activity() {
  const scope = useScope(); const params = useSearchParams(); const router = useRouter(); const roster = useAccounts();
  const result = usePoll(useCallback(() => obsClient.listSessions(scope), [scope]));
  const account = roster.data?.accounts.find((a) => a.key === scope.accountKey);
  const query = params.get('q') ?? ''; const project = params.get('project') ?? ''; const model = params.get('model') ?? ''; const status = params.get('status') ?? ''; const days = Number(params.get('days') || '0'); const sort: SessionSort = params.get('sort') === 'tokens' ? 'tokens' : 'recent';
  function update(key: string, value: string) { const next = new URLSearchParams(params.toString()); if (value) next.set(key, value); else next.delete(key); next.delete('id'); next.delete('tab'); router.replace(`/sessions${next.size ? `?${next}` : ''}`, { scroll: false }); }
  function sessionHref(id: string, analysis = false) { const next = new URLSearchParams(params.toString()); next.set('id', id); if (analysis) next.set('tab', 'analysis'); else next.delete('tab'); return `/sessions?${next}`; }
  const sessions = result.data?.sessions ?? [];
  const filtered = sortSessions(filterSessions(sessions, query).filter((r) => (!project || projectLabel(r) === project) && (!model || r.models.some((m) => m.model === model)) && (!status || (status === 'live' ? r.live : status === 'analysed' ? r.analysed : !r.live)) && (!days || r.end >= Date.now() / 1000 - days * 86400)), sort);
  const profiles = [...new Map((account ? account.profiles : (roster.data?.accounts ?? []).flatMap((a) => a.profiles)).map((p) => [p.profile, p])).values()];
  const active = [['q', query], ['project', project], ['model', model], ['status', status], ['days', days ? `${days} days` : '']].filter(([, v]) => v);
  return <div className="obs-page">
    {account ? <AccountHeading account={account} /> : <header className="obs-heading"><div><div className="obs-eyebrow">Recorded sessions</div><h1>Activity</h1><p>Explore how your agents work, across projects and profiles.</p></div><Link href={scopeHref('/spend', { ...scope, accountKey: '' })} className="obs-button">Token history →</Link></header>}
    <ScopeTabs scope={scope} />
    {result.error && <Alert tone="error">{result.data ? 'Activity refresh failed. Last response retained. ' : 'Activity unavailable. '}{result.error}</Alert>}
    {result.data?.scopeNote && <p className="obs-alert" data-tone="info">{result.data.scopeNote}</p>}
    {result.data?.errors.map((e) => <Alert key={e.section}>{e.section}: {e.error}</Alert>)}
    {result.data?.indexStale && <Alert>Historical indexing is stale. Live activity and previously indexed sessions may have different coverage.</Alert>}
    <div className="obs-toolbar"><div className="flex flex-wrap gap-2 items-center"><input className="obs-input min-w-[230px]" aria-label="Search sessions" type="search" placeholder="Search project, session, or profile…" value={query} onChange={(e) => update('q', e.target.value)} />
      <select className="obs-input max-w-[230px]" aria-label="Activity account" value={scope.accountKey} onChange={(e) => { const a = roster.data?.accounts.find((r) => r.key === e.target.value); const next = new URLSearchParams(params.toString()); next.delete('profile'); if (a) { next.set('accountKey', a.key); next.set('provider', a.provider); } else { next.delete('accountKey'); next.delete('provider'); } router.replace(`/sessions?${next}`, { scroll: false }); }}><option value="">All accounts</option>{roster.data?.accounts.map((a) => <option key={a.key} value={a.key}>{providerName(a.provider)} · {accountLabel(a)}</option>)}</select>
      <select className="obs-input max-w-[180px]" aria-label="Activity profile" value={scope.profile} onChange={(e) => update('profile', e.target.value)}><option value="">All linked profiles</option>{profiles.map((p) => <option key={p.profile} value={p.profile}>{p.displayName || p.profile}</option>)}</select>
      <select className="obs-input" aria-label="Sessions active during" value={days} onChange={(e) => update('days', e.target.value === '0' ? '' : e.target.value)}><option value={0}>All recorded time</option><option value={1}>Active in 24 hours</option><option value={7}>Active in 7 days</option><option value={30}>Active in 30 days</option></select>
    </div><select className="obs-input" aria-label="Sort sessions" value={sort} onChange={(e) => update('sort', e.target.value)}><option value="recent">Most recent</option><option value="tokens">Most recorded tokens</option></select></div>
    <details className="mb-4"><summary className="text-xs text-muted cursor-pointer">More filters</summary><div className="flex flex-wrap gap-2 mt-3"><select className="obs-input max-w-[300px]" aria-label="Project" value={project} onChange={(e) => update('project', e.target.value)}><option value="">All projects</option>{[...new Set(sessions.map(projectLabel))].sort().map((p) => <option key={p}>{p}</option>)}</select><select className="obs-input" aria-label="Model" value={model} onChange={(e) => update('model', e.target.value)}><option value="">All models</option>{[...new Set(sessions.flatMap((r) => r.models.map((m) => m.model)))].sort().map((m) => <option key={m}>{m}</option>)}</select><select className="obs-input" aria-label="Session status" value={status} onChange={(e) => update('status', e.target.value)}><option value="">All statuses</option><option value="live">Live</option><option value="ended">Not live</option><option value="analysed">Analysed</option></select></div></details>
    {active.length > 0 && <div className="flex gap-2 flex-wrap mb-4">{active.map(([key, value]) => <button className="obs-button" key={key} onClick={() => update(key, '')} aria-label={`Remove ${key} filter: ${value}`}>{value}<span aria-hidden="true">×</span></button>)}</div>}
    <section className="obs-panel" aria-label="Session activity">{!result.data ? <PageState title={result.error ? 'No activity response' : 'Loading sessions…'} /> : !filtered.length ? <PageState title={sessions.length ? 'No matching sessions' : 'No sessions recorded'}>{sessions.length ? 'Remove a filter or choose another scope.' : 'Recorded sessions appear after collection. Manage history import in Settings.'}</PageState> : <div className="overflow-auto"><table className="obs-data-table min-w-[830px]"><thead><tr><th>Project / session</th><th>Provider / profile</th><th>Last activity</th><th>Recorded tokens</th><th>Duration</th><th>Status</th></tr></thead><tbody>{filtered.slice(0, 200).map((r) => <tr key={r.id}>
      <td className="max-w-[340px]"><Link href={sessionHref(r.id)} className="font-medium block truncate" title={r.project}>{projectLabel(r)}</Link><span className="block text-[11px] font-mono text-muted truncate" title={r.nativeId || r.id}>{(r.nativeId || r.id).slice(0, 32)}</span></td>
      <td>{providerName(providerOf(r))}<span className="block text-xs text-muted">{detectedHarness(providerOf(r)) ? inferenceProviders(r).join(' · ') || 'No provider recorded' : profiles.find((p) => p.profile === r.profile)?.displayName || r.profile || 'Profile unknown'}</span></td>
      <td className="whitespace-nowrap text-xs">{r.end ? new Date(r.end * 1000).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : 'Unknown'}</td>
      <td className="tabular-nums"><span title={observedTotal(totalSessionTokens(r), sessionUsageKnown(r), fmtInt.format)}>{observedTotal(totalSessionTokens(r), sessionUsageKnown(r), compactInt)}</span><span className="block text-xs text-muted">{observedTotal(r.requests, sessionUsageKnown(r), fmtInt.format)} requests</span></td>
      <td>{indexedProvider(providerOf(r)) ? observedTotal(r.durationMs / 1000, r.durationKnown, ageText) : r.start && r.end ? ageText(r.end - r.start) : 'Unknown'}</td>
      <td>{r.live ? <Badge tone="ok">Live</Badge> : <span className="text-xs text-muted">Recorded</span>}{r.analysed && providerOf(r) === 'claude' && <Link className="block text-xs mt-1" href={sessionHref(r.id, true)}>Read analysis →</Link>}</td>
    </tr>)}</tbody></table></div>}</section>
    <div className="obs-caption"><span>{result.data ? `${filtered.length} sessions${filtered.length > 200 ? ' · showing first 200' : ''}` : 'Loading'}</span><span>Whole-session recorded totals. “+” indicates incomplete coverage.</span></div><p className="obs-muted text-xs mt-3">Date filters select sessions by their last recorded activity; totals cover each whole session. Historical usage does not measure the current allowance window.</p>
  </div>;
}
