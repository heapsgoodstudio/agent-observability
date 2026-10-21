'use client';

import Link from 'next/link';
import { useScope } from '@/src/lib/useScope';
import { scopeHref } from '@/src/lib/dashboard';
import { PageState } from '@/src/components/AccountUI';
import { Suspense, useCallback } from 'react';
import { useRouter, useSearchParams } from 'next/navigation';
import { obsClient } from '@/src/lib/client';
import { useProfile } from '@/src/lib/useProfile';
import { usePoll } from '@/src/lib/usePoll';
import { useScrollTarget } from '@/src/lib/scrollTarget';
import { compactInt, fmtInt } from '@/src/lib/format';
import { normalizePeriod, PERIOD_SELECT_OPTIONS, PERIOD_SHORT } from '@/src/lib/spend';
import { ProviderPage } from '@/src/components/CodexMonitoring';
import { Section } from '@/src/components/Section';
import { ViewHeader } from '@/src/components/ViewHeader';
import { Banner } from '@/src/components/ui/Banner';
import { DataTable } from '@/src/components/ui/DataTable';
import { StatTile } from '@/src/components/ui/StatTile';
import type { GetSpendResponse } from '@/src/gen/claudeobs/v1/obs_pb';

export function Spend() {
  return <Suspense fallback={<ViewHeader title="Token history" />}><HistoryScope /></Suspense>;
}

function HistoryScope() {
  const scope = useScope();
  if (scope.accountKey || (scope.provider && !['claude', 'codex'].includes(scope.provider))) return <div className="obs-page"><PageState title="Explore token history in Activity">Account-scoped historical sessions and recorded token totals are available in Activity. Choose the relevant date period there.</PageState><Link className="obs-button" href={scopeHref('/sessions', scope)}>Open scoped Activity →</Link></div>;
  return <ProviderPage title="Token history" historyOnly><SpendRoute /></ProviderPage>;
}

function SpendRoute() {
  const router = useRouter();
  const search = useSearchParams();
  const { profile } = useProfile();
  const period = normalizePeriod(search.get('period'));
  const fetchUsage = useCallback(() => obsClient.getSpend({ period, profile }), [period, profile]);
  const usage = usePoll(fetchUsage);
  return <>
    <ViewHeader title="Token history · Claude">
      <label className="sr-only" htmlFor="usage-period">Usage period</label>
      <select id="usage-period" value={period} className="h-7 rounded-md border border-line bg-raised px-2 text-[13px] text-primary" onChange={(event) => {
        const next = new URLSearchParams(search.toString());
        next.set('period', event.target.value); next.delete('measure');
        router.replace(`/spend?${next.toString()}`, { scroll: false });
      }}>{PERIOD_SELECT_OPTIONS.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select>
    </ViewHeader>
    <div className="obs-page">
      <p className="mb-6 mt-0 text-[13px] text-muted">Recorded usage is shown in tokens. This is historical coverage, separate from the live allowance and reset windows on Overview.</p>
      {usage.error ? <Banner tone="warn">{usage.error}</Banner> : null}
      {usage.data ? <UsageContent resp={usage.data} periodLabel={PERIOD_SHORT[period]} /> : <p className="text-muted">Loading usage…</p>}
    </div>
  </>;
}

export function UsageContent({ resp, periodLabel }: { resp: GetSpendResponse; periodLabel: string }) {
  useScrollTarget();
  const rows = resp.modelUsage.map((row) => ({ cells: { model: row.model, input: compactInt(row.input), output: compactInt(row.output), cacheRead: compactInt(row.cacheRead), cacheWrite: compactInt(row.cacheCreation), total: compactInt(row.input + row.output + row.cacheRead + row.cacheCreation) } }));
  return <>
    <Section id="daily" title={`Recorded model usage · ${periodLabel}`} wide>
      {resp.errors.map((e) => <Banner key={e.section} tone="warn">{e.section}: {e.error}</Banner>)}
      <DataTable columns={[{ key: 'model', label: 'Model' }, { key: 'input', label: 'Input tokens', num: true }, { key: 'output', label: 'Output tokens', num: true }, { key: 'cacheRead', label: 'Cache read', num: true }, { key: 'cacheWrite', label: 'Cache write', num: true }, { key: 'total', label: 'Total tokens', num: true }]} rows={rows} empty="No token usage recorded in this period." />
    </Section>
    <Section id="delivery" title={`Delivery · ${periodLabel}`} wide>
      <div className="mb-5 grid grid-cols-2 gap-6"><StatTile label="Pull requests" value={resp.deliveryPrs > 0 ? fmtInt.format(resp.deliveryPrs) : 'Not recorded'} /><StatTile label="Commits" value={resp.deliveryCommits > 0 ? fmtInt.format(resp.deliveryCommits) : 'Not recorded'} /></div>
      <DataTable columns={[{ key: 'project', label: 'Project' }, { key: 'sessions', label: 'Sessions', num: true }, { key: 'prs', label: 'PRs', num: true }, { key: 'commits', label: 'Commits', num: true }]}
        rows={resp.deliveryRows.map((row) => ({ cells: { project: row.project, sessions: fmtInt.format(row.sessions), prs: fmtInt.format(row.prs), commits: fmtInt.format(row.commits) } }))}
        empty="No delivery counters recorded in this period." note="Delivery counters come from the live collector; earlier sessions may have no delivery records." />
    </Section>
  </>;
}
