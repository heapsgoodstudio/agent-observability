'use client';

import { Suspense, useCallback, type ReactNode } from 'react';
import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { obsClient } from '@/src/lib/client';
import type { HealthCheck } from '@/src/gen/claudeobs/v1/obs_pb';
import { Banner } from '@/src/components/ui/Banner';
import { fmtInt } from '@/src/lib/format';
import { groupHealthChecks, healthUpdatedAt } from '@/src/lib/presentation';
import { usePoll } from '@/src/lib/usePoll';
import { ignoresSelectionNote } from '@/src/lib/profiles';

// The server resolves each check's state. The response timestamp says when
// the install was checked; counting non-ok checks would imply a different
// measure of response completeness and is deliberately avoided.
const CHECK_STYLE: Record<string, { dot: string; text: string; label: string }> = {
  ok: { dot: 'bg-ok', text: 'text-ok-text', label: 'Healthy' },
  warn: { dot: 'bg-warn', text: 'text-warn-text', label: 'Warning' },
  bad: { dot: 'bg-danger', text: 'text-danger-text', label: 'Needs attention' },
  unknown: { dot: 'bg-muted', text: 'text-muted', label: 'Unknown' },
};

function CheckRow({ c }: { c: Pick<HealthCheck, 'name' | 'state' | 'detail'> }) {
  const state = CHECK_STYLE[c.state] ?? CHECK_STYLE.unknown;
  return (
    <div className="border-b border-hairline py-3 last:border-0">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2 text-[14px] text-primary"><span aria-hidden="true" className={`h-1.5 w-1.5 shrink-0 rounded-full ${state.dot}`} /><span className="wrap-anywhere">{c.name}</span></span>
        <span className={`text-[13px] ${state.text}`}>{state.label}</span>
      </div>
      <p className="mb-0 ml-3.5 mt-1 text-[13px] leading-6 text-muted wrap-anywhere">{c.detail}</p>
    </div>
  );
}

function Group({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="obs-panel min-w-0 overflow-hidden" aria-label={title}>
      <h2 className="m-0 border-b border-hairline bg-raised px-5 py-4 text-[14px] font-medium text-primary">{title}</h2>
      <div className="px-5 py-2">{children}</div>
    </section>
  );
}

function HealthHeading({ codexOnly = false, children }: { codexOnly?: boolean; children?: ReactNode }) {
  return <div className="obs-heading flex-wrap">
    <div><div className="obs-eyebrow">Workspace health</div><h1>Collection health</h1><p className="obs-muted mt-2">{codexOnly ? 'Codex collection and the shared services behind your workspace.' : 'All accounts: connections, collection, and local services.'}</p>{children}</div>
    <Link className="obs-button text-[13px]!" href="/settings">Manage connections</Link>
  </div>;
}

function HealthLoading() {
  return <div><div className="obs-page space-y-6"><HealthHeading /><div className="obs-panel p-6" role="status" aria-busy="true"><p className="m-0 text-[14px] text-muted">Loading collection health…</p></div></div></div>;
}

export function Health() {
  return (
    // useSearchParams needs the boundary under Next's static export.
    <Suspense fallback={<HealthLoading />}>
      <HealthRoute />
    </Suspense>
  );
}

function HealthRoute({ codexOnly = false }: { codexOnly?: boolean }) {
  const searchParams = useSearchParams();
  // Health checks the install and explicitly describes its profile scope.
  const ignored = ignoresSelectionNote(searchParams.get('profile') ?? '', codexOnly ? 'Codex health' : 'Health');
  const fetchHealth = useCallback(() => obsClient.getHealth({}), []);
  const { data: resp, error: err } = usePoll(fetchHealth);

  if (err && !resp) {
    return <div><div className="obs-page space-y-6"><HealthHeading codexOnly={codexOnly} /><div role="alert"><Banner tone="error">{`Failed to load health: ${err}`}</Banner></div><p className="m-0 text-[13px] text-muted">Health checks will refresh automatically.</p></div></div>;
  }
  if (!resp) return <HealthLoading />;

  const groups = groupHealthChecks(resp.checks);
  const profileChecks = groups.profiles.filter((check) => !codexOnly || !check.name.endsWith(' (claude)'));
  const vlCheck = resp.checks.find((check) => check.name === 'VictoriaLogs');
  const vlBad = vlCheck?.state === 'bad';
  const logTotal = resp.logVolume24h.reduce((total, row) => total + row.value, 0);
  const updatedAt = healthUpdatedAt(resp.identity?.generatedAt ?? '');

  return (
    <div>
      <div className="obs-page space-y-6">
        <HealthHeading codexOnly={codexOnly}>
          <div className="mt-3 flex flex-wrap gap-x-4 gap-y-1 text-[13px] text-muted">{ignored ? <span>{ignored}</span> : null}{updatedAt ? <span>{updatedAt}</span> : null}</div>
        </HealthHeading>
        {err ? <div role="alert"><Banner tone="warn">Refresh failed; showing the last health response. {err}</Banner></div> : null}
        <div className="grid items-start gap-5 lg:grid-cols-2">
          <Group title="Store reachability">
            {groups.stores.filter((check) => !codexOnly || check.name !== 'Analysis runner').map((check) => <CheckRow key={check.name} c={check} />)}
          </Group>
          {!codexOnly ? <Group title="Expected Claude series">
            {groups.series.map((check) => <CheckRow key={check.name} c={check} />)}
          </Group> : null}
          {!codexOnly ? <Group title="Claude poller freshness">
            {groups.poller.map((check) => <CheckRow key={check.name} c={check} />)}
          </Group> : null}
          {!codexOnly ? <Group title="Log volume, 24 hours · all providers">
            {vlBad ? <div className="py-3"><Banner tone="error">{vlCheck?.detail ?? 'VictoriaLogs unreachable'}</Banner></div> : resp.logVolume24h.length ? <>
              {resp.logVolume24h.map((row) => <div key={row.key} className="flex items-baseline justify-between gap-4 border-b border-hairline py-3 text-[13px] last:border-0"><span className="min-w-0 break-all text-secondary">{row.key}</span><span className="shrink-0 tabular-nums text-primary">{fmtInt.format(row.value)}</span></div>)}
              <p className="mb-2 mt-3 text-[13px] text-muted">{fmtInt.format(logTotal)} events, 24 hours</p>
            </> : <p className="my-3 text-[13px] text-muted">No events in the last 24 hours.</p>}
          </Group> : null}
          {profileChecks.length > 0 ? <Group title="Profile connections and collection">
            {profileChecks.map((check) => <CheckRow key={check.name} c={check} />)}
          </Group> : null}
          <Group title="Session indexer">
            {groups.indexer.map((check) => <CheckRow key={check.name} c={check} />)}
          </Group>
          <Group title="Retention">
            {!resp.retentionObserved ? <p className="mb-1 mt-3 text-[13px] leading-6 text-muted">Stated from config, not observed from either store.</p> : null}
            <CheckRow c={{ name: 'VictoriaMetrics', state: resp.retentionObserved ? 'ok' : 'unknown', detail: resp.vmRetention }} />
            <CheckRow c={{ name: 'VictoriaLogs', state: resp.retentionObserved ? 'ok' : 'unknown', detail: resp.vlRetention }} />
          </Group>
        </div>
      </div>
    </div>
  );
}
