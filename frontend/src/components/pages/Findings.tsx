'use client';

import { useCallback } from 'react';
import { obsClient } from '@/src/lib/client';
import type { Finding, GetFindingsResponse } from '@/src/gen/claudeobs/v1/obs_pb';
import { Banner } from '@/src/components/ui/Banner';
import { StatTile } from '@/src/components/ui/StatTile';
import { Badge, type BadgeTone } from '@/src/components/ui/Badge';
import { ageText, fmtInt } from '@/src/lib/format';
import { useNow } from '@/src/lib/useNow';
import { useScope } from '@/src/lib/useScope';
import { Alert, PageState } from '@/src/components/AccountUI';
import { usePoll } from '@/src/lib/usePoll';

// The Findings tab of the Insights view: how this profile is using Claude
// Code and what it costs. The "last 7 days" headline band and the ranked
// findings list are both computed server-side by GetFindings, already
// sorted by severity/impact/id (internal/rules/rules.go's Run). This
// component is presentation only; the Insights page owns the view header
// and tabs.
//
// The severity chip beside each finding's title is this design system's
// Badge. It is unrelated to the sidebar nav's findings count, which
// Shell.tsx polls for itself off GetFindingsResponse.badge_count.
const LOUD = new Set(['high', 'medium', 'info']);

const SEV_LABEL: Record<string, string> = { high: 'act on this', medium: 'worth a look', info: 'context' };

const SEV_TONE: Record<string, BadgeTone> = { high: 'danger', medium: 'warn', info: 'neutral' };

function FindingRow({ f }: { f: Finding }) {
  return (
    <div className="obs-panel p-5 flex flex-col gap-3 mb-3">
      <div className="flex flex-wrap items-baseline gap-2">
        <Badge tone={SEV_TONE[f.severity] ?? 'neutral'}>{f.severity}</Badge>
        <span className="text-[13px] font-medium text-primary">{f.title}</span>
        <span className="ml-auto text-[12px] text-faint">{`${SEV_LABEL[f.severity] ?? f.severity} · ${f.window}`}</span>
      </div>
      <p className="m-0 font-mono text-[12px] leading-[18px] text-muted">{f.evidence}</p>
      <p className="m-0 text-[13px] text-secondary">{f.fix}</p>
    </div>
  );
}

// The page's honesty: a rule that ran clean is named, and a rule with too
// little data to judge is named separately rather than folded into "nothing
// to report".
function SummaryLine({ label, ids }: { label: string; ids: string[] }) {
  if (!ids.length) return null;
  return <p className="m-0 mt-2.5 text-[12px] text-faint">{`${label}: ${ids.join(', ')}`}</p>;
}

function HeadlineBand({ resp }: { resp: GetFindingsResponse }) {
  const stats = [
    { label: 'Active hours', value: resp.activeHours7d > 0 ? resp.activeHours7d.toFixed(1) : 'Not recorded', note: 'CLI-active time, 7 days' },
    { label: 'Commits', value: resp.commits7d > 0 ? fmtInt.format(resp.commits7d) : 'Not recorded', note: 'Live collector, 7 days' },
    { label: 'Lines added', value: resp.lines7d > 0 ? fmtInt.format(resp.lines7d) : 'Not recorded', note: 'Live collector, 7 days' },
  ];
  return (
    <div className="flex flex-col gap-6 sm:flex-row sm:gap-0">
      {stats.map((s, i) => (
        <div
          key={s.label}
          className={`min-w-0 flex-1 ${i === 0 ? 'sm:pr-8' : i === stats.length - 1 ? 'border-hairline sm:border-l sm:pl-8' : 'border-hairline sm:border-l sm:px-8'}`}
        >
          <StatTile size="md" label={s.label} value={s.value} note={s.note} />
        </div>
      ))}
    </div>
  );
}

function FindingsList({ resp }: { resp: GetFindingsResponse }) {
  // Ticks once a second so the relative times below age on their own.
  const now = useNow();

  const loud = resp.findings.filter((f) => LOUD.has(f.severity));
  const clean = resp.findings.filter((f) => f.severity === 'clean').map((f) => f.id);
  const insufficient = resp.findings.filter((f) => f.severity === 'insufficient').map((f) => f.id);

  // logsStartTs is proto3's implicit 0 default doubling as "no logs seen"
  // (GetFindingsResponse's own doc comment on the field).
  const coverage = resp.logsStartTs
    ? `Log-store rules cover ${ageText(now - resp.logsStartTs)} of history, the window each card names at most.`
    : 'The log store has no events yet, so log-based rules cannot run.';

  return (
    <>
      {resp.vlOk ? null : (
        <div className="mb-2.5"><Banner tone="error">VictoriaLogs unreachable: log-based findings are missing</Banner></div>
      )}
      {resp.indexOk ? null : (
        <div className="mb-2.5"><Banner tone="error">session index unreachable: history-based findings are missing</Banner></div>
      )}
      <p className="m-0 text-[12px] text-muted">{coverage}</p>
      <div className="mt-1 flex flex-col">
        {loud.map((f) => <FindingRow key={f.id} f={f} />)}
      </div>
      {loud.length ? null : <p className="m-0 mt-3 text-muted">nothing to act on right now</p>}
      <details className="mt-5"><summary className="cursor-pointer text-xs text-muted">Checked, nothing to report ({clean.length})</summary>{resp.findings.filter((f) => f.severity === 'clean').map((f) => <FindingRow key={f.id} f={f} />)}</details>
      <details className="mt-3"><summary className="cursor-pointer text-xs text-muted">Insufficient evidence ({insufficient.length})</summary>{resp.findings.filter((f) => f.severity === 'insufficient').map((f) => <FindingRow key={f.id} f={f} />)}</details>
    </>
  );
}

export function Findings() {
  const scope = useScope();
  const fetchFindings = useCallback(() => obsClient.getFindings(scope), [scope]);
  const { data: resp, error: err } = usePoll(fetchFindings);

  // `err && !resp` rather than `err`: a landed response survives a failed
  // refresh, so one blip cannot replace a working page with an error card.
  if (err && !resp) {
    return <Banner tone="error">{`failed to load findings: ${err}`}</Banner>;
  }
  if (!resp) {
    return <p className="m-0 text-muted">loading…</p>;
  }

  if (resp.errors.some((e) => e.section === 'capability')) return <PageState title="Findings are unavailable for this provider">{resp.scopeNote || resp.errors.find((e) => e.section === 'capability')?.error}</PageState>;
  return (
    <>
      {err && <Alert tone="error">Refresh failed. Showing the last response. {err}</Alert>}
      {resp.scopeNote && <Alert tone="info">{resp.scopeNote}</Alert>}
      {resp.errors.map((e) => <Alert key={e.section}>{e.section}: {e.error}</Alert>)}
      {resp.vmOk ? null : (
        <div className="mb-5"><Banner tone="error">VictoriaMetrics unreachable: the headline figures are missing</Banner></div>
      )}
      {(!resp.provider || resp.provider === 'claude') && <div className="obs-panel p-5 mb-8"><HeadlineBand resp={resp} /></div>}
      <p className="m-0 mb-2 text-[11px] font-medium uppercase tracking-[0.04em] text-muted">How to use the allowance efficiently</p>
      <FindingsList resp={resp} />
    </>
  );
}
