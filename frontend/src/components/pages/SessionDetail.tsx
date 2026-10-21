'use client';

import { Code, ConnectError } from '@connectrpc/connect';
import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { obsClient } from '@/src/lib/client';
import { useSessionLinks } from '@/src/lib/useSessionLinks';
import { useScope } from '@/src/lib/useScope';
import { usePoll } from '@/src/lib/usePoll';
import { useAccounts } from '@/src/components/AccountContext';
import { Alert, LoadingPage, PageState, ProviderMark } from '@/src/components/AccountUI';
import { detectedHarness, indexedProvider, inferenceProviders, observedTotal, providerLabel, providerOf, sessionUsageKnown } from '@/src/lib/providers';
import type { GetAnalysisResponse, GetMetaResponse, SessionRow, TrajectoryPoint } from '@/src/gen/claudeobs/v1/obs_pb';
import { Badge } from '@/src/components/ui/Badge';
import { Line } from '@/src/components/charts/Line';
import { IconInsights, IconSessions, IconSpend } from '@/src/components/icons';
import { ageText, agoText, compactInt, fmtClock, fmtInt, money, shortModel } from '@/src/lib/format';
import { deliveryText, projectLabel, totalSessionTokens } from '@/src/lib/sessions';
import { useNow } from '@/src/lib/useNow';
import { hasReport } from '@/src/lib/report';
import { AnalyseControls } from '@/src/components/report/AnalyseControls';
import { AnalysisSummaryCard } from '@/src/components/report/AnalysisSummaryCard';
import { ReportView } from '@/src/components/report/ReportView';
import { isStuck } from '@/src/lib/sessionDetail';

type AnalysisState = 'loading' | 'none' | 'loaded' | 'error';

function timestamp(value: number): string {
  return value > 0 && Number.isFinite(value)
    ? new Date(value * 1000).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
    : 'Not recorded';
}
function duration(row: SessionRow): string {
  return indexedProvider(providerOf(row))
    ? observedTotal(row.durationMs / 1000, row.durationKnown, ageText)
    : row.start > 0 && row.end >= row.start ? ageText(row.end - row.start) : 'Not recorded';
}

function CopyIdChip({ id }: { id: string }) {
  const [status, setStatus] = useState<'idle' | 'copied' | 'error'>('idle');
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  async function copy() {
    clearTimeout(timer.current);
    try { await navigator.clipboard.writeText(id); setStatus('copied'); }
    catch { setStatus('error'); }
    timer.current = setTimeout(() => setStatus('idle'), 3000);
  }
  return <div className="flex items-center gap-2 flex-wrap">
    <button className="obs-button font-mono text-xs!" type="button" title={id} aria-label={`Copy session ID ${id}`} onClick={copy}>
      <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" aria-hidden="true"><rect x="5" y="5" width="8" height="9" rx="1.5" /><path d="M10 5V2H2v9h3" /></svg>
      {id.slice(0, 8)}
    </button>
    <span className="text-xs text-muted" role="status">{status === 'copied' ? 'Copied' : status === 'error' ? 'Copy unavailable; full ID is in Session details.' : ''}</span>
  </div>;
}

function Subtabs({ id, tab, state }: { id: string; tab: string; state?: string }) {
  const href = useSessionLinks();
  return <nav className="obs-tabs" aria-label="Session sections">
    <Link href={href(`/sessions?id=${encodeURIComponent(id)}`)} aria-current={tab === 'overview' ? 'page' : undefined}>Overview</Link>
    <Link href={href(`/sessions?id=${encodeURIComponent(id)}&tab=analysis`)} aria-current={tab === 'analysis' ? 'page' : undefined} className="inline-flex items-center gap-2">
      Analysis{state === 'running' ? <span className="obs-dot text-warn" aria-label="Running" /> : state === 'done' ? <span className="obs-dot text-ok" aria-label="Report available" /> : state === 'error' ? <span className="obs-dot text-warn" aria-label="Needs attention" /> : null}
    </Link>
  </nav>;
}

export function SessionMetrics({ row }: { row: SessionRow }) {
  const codex = indexedProvider(providerOf(row));
  const metrics = [
    { label: 'Recorded tokens', value: row.tokens ? observedTotal(totalSessionTokens(row), sessionUsageKnown(row), compactInt) : 'Unavailable', note: 'Input, output and cache tokens' },
    { label: 'Requests', value: observedTotal(row.requests, sessionUsageKnown(row), fmtInt.format), note: 'Recorded model requests' },
    { label: codex ? 'Recorded duration' : 'Session span', value: duration(row), note: codex ? 'Duration reported by the source' : 'First to last recorded activity' },
    { label: 'Tool calls', value: observedTotal(row.toolCalls, !codex || row.toolsKnown, fmtInt.format), note: 'Recorded tool invocations' },
  ];
  return <section className="session-metrics" aria-label="Session summary">{metrics.map((m) => <div className="obs-panel session-metric" key={m.label}>
    <h2>{m.label}</h2><p className="session-metric-value">{m.value}</p><p className="text-xs text-muted">{m.note}</p>
  </div>)}</section>;
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return <div className="session-fact"><dt>{label}</dt><dd>{children}</dd></div>;
}

export function PropertiesPanel({ row, profileName }: { row: SessionRow; profileName?: string }) {
  const codex = indexedProvider(providerOf(row));
  const complete = sessionUsageKnown(row);
  const total = totalSessionTokens(row);
  const tokens = row.tokens ? [
    { name: 'Input', count: row.tokens.input, color: 'var(--chart-1)' },
    { name: 'Output', count: row.tokens.output, color: 'var(--chart-2)' },
    { name: 'Cache read', count: row.tokens.cacheRead, color: 'var(--chart-4)' },
    ...(!codex ? [{ name: 'Cache write', count: row.tokens.cacheCreation, color: 'var(--chart-5)' }] : []),
  ] : [];
  return <aside className="session-sidebar" aria-label="Session details and recorded usage">
    <section className="obs-panel"><div className="obs-panel-heading"><h2>Session details</h2></div><div className="obs-panel-body">
      <dl className="session-facts">
        <Fact label="Provider">{providerLabel(providerOf(row))}</Fact>
        {detectedHarness(providerOf(row))
          ? <Fact label="Inference providers">{inferenceProviders(row).join(' · ') || 'None recorded'}</Fact>
          : <Fact label="Recorded profile">{profileName || row.profile || 'Unattributed'}</Fact>}
        <Fact label="Started">{timestamp(row.start)}</Fact>
        <Fact label="Last activity">{timestamp(row.end)}</Fact>
        <Fact label="User turns">{row.userTurns > 0 ? fmtInt.format(row.userTurns) : 'Not recorded'}</Fact>
        <Fact label="Transcript">{row.hasTranscript ? 'Available locally' : 'Not available locally'}</Fact>
        {/* Only a detected harness's requests are outside any plan, so only
            they carry a dollar figure, labelled as that harness's estimate. */}
        {detectedHarness(providerOf(row)) && <Fact label="Estimated cost"><span title={`${providerLabel(providerOf(row))}'s estimate: provider-reported tokens at public API rates, not a bill`}>{observedTotal(row.costUsd, row.costKnown, money)}</span></Fact>}
      </dl>
      <details className="session-source-details"><summary>Session identifiers</summary><dl className="mt-3 space-y-3">
        <div><dt className="text-xs text-muted">Session ID</dt><dd className="font-mono text-xs break-all mt-1 select-all">{row.nativeId || row.id}</dd></div>
        {row.parentId && <div><dt className="text-xs text-muted">Parent session</dt><dd className="font-mono text-xs break-all mt-1 select-all">{row.parentId}</dd></div>}
        {row.profile && <div><dt className="text-xs text-muted">Profile ID</dt><dd className="font-mono text-xs break-all mt-1">{row.profile}</dd></div>}
      </dl></details>
    </div></section>
    <section className="obs-panel"><div className="obs-panel-heading"><h2>Tokens</h2><span className="text-xs text-muted">{complete ? 'Recorded usage' : 'Partial coverage'}</span></div><div className="obs-panel-body">
      {tokens.length ? <>{total > 0 && <div className="session-token-bar" aria-hidden="true">{tokens.map((t) => <span key={t.name} style={{ width: `${t.count / total * 100}%`, background: t.color }} />)}</div>}
        <dl className="session-facts">{tokens.map((t) => <Fact key={t.name} label={t.name}><span className="inline-flex items-center gap-2" title={complete || t.count > 0 ? `${observedTotal(t.count, complete, fmtInt.format)} recorded tokens` : 'Token count unavailable'}><span className="obs-dot" style={{ color: t.color }} />{observedTotal(t.count, complete, compactInt)}</span></Fact>)}
          {codex && <Fact label="Reasoning">{observedTotal(row.reasoningTokens, complete, compactInt)}</Fact>}
        </dl>{codex && <p className="text-xs text-muted mt-3">Reasoning tokens are included in output.</p>}</> : <p className="text-[13px] text-muted leading-6">No token breakdown is available for this session.</p>}
      {!complete && <p className="text-xs text-muted mt-3">A + marks the recorded portion. Missing usage remains unknown.</p>}
    </div></section>
    <section className="obs-panel"><div className="obs-panel-heading"><h2>Models</h2></div><div className="obs-panel-body flex flex-wrap gap-2">
      {row.models.length ? [...row.models].sort((a, b) => a.model.localeCompare(b.model)).map((m) => <span className="session-tag" key={m.model} title={m.model}>{shortModel(m.model)}</span>) : <p className="text-[13px] text-muted">No model information recorded.</p>}
    </div></section>
    <section className="obs-panel"><div className="obs-panel-heading"><h2>{codex ? 'Recorded errors' : 'Delivery'}</h2></div><div className="obs-panel-body">
      <dl className="session-facts">{codex ? <Fact label="Errors">{observedTotal(row.errorCount, row.errorsKnown, fmtInt.format)}</Fact> : <><Fact label="Commits">{deliveryText(row.commits) ?? 'Not recorded'}</Fact><Fact label="Pull requests">{deliveryText(row.pullRequests) ?? 'Not recorded'}</Fact></>}</dl>
      {!codex && <p className="text-xs text-muted mt-3">Live collection only. Earlier sessions may not have delivery records.</p>}
    </div></section>
  </aside>;
}

function ContextTimeline({ points, provider }: { points: TrajectoryPoint[]; provider: string }) {
  // Zero context in the legacy trajectory is also used for an absent sample.
  // It cannot establish an observed zero-size context.
  const samples = points.filter((p) => p.ts > 0 && Number.isFinite(p.ts) && Number.isFinite(p.contextTokens) && p.contextTokens > 0).toSorted((a, b) => a.ts - b.ts);
  const peak = samples.reduce((best, p) => Math.max(best, p.contextTokens), 0);
  const stride = Math.max(1, Math.ceil(samples.length / 500));
  const peakIndex = samples.findIndex((p) => p.contextTokens === peak);
  const chart: [number, number][] = samples.filter((_, i) => i % stride === 0 || i === samples.length - 1 || i === peakIndex).map((p) => [p.ts, p.contextTokens]);
  const sameDay = samples.length > 0 && new Date(samples[0].ts * 1000).toDateString() === new Date(samples[samples.length - 1].ts * 1000).toDateString();
  return <section className="obs-panel" aria-labelledby="context-title"><div className="obs-panel-heading"><div><h2 id="context-title">Context over the session</h2><p className="text-xs text-muted mt-1">{provider === 'claude' ? 'Main conversation input at each observed request' : 'Session input at each observed request'}</p></div>{samples.length > 0 && <div className="text-right"><p className="text-xs text-muted">Peak context</p><p className="text-[17px] font-medium tabular-nums">{compactInt(peak)} <span className="text-xs text-muted">tokens</span></p></div>}</div>
    {chart.length ? <div className="obs-panel-body"><Line points={chart} xMin={chart[0][0]} xMax={Math.max(chart[chart.length - 1][0], chart[0][0] + 1)} fmtY={compactInt} fmtX={(t) => sameDay ? fmtClock.format(new Date(t * 1000)) : timestamp(t)} label="Recorded context tokens over the session" />
      <details className="session-source-details"><summary>{fmtInt.format(samples.length)} observations · View recorded values{samples.length > 100 ? ' (latest 100)' : ''}</summary><div className="max-h-60 overflow-auto mt-3"><table className="obs-data-table"><caption className="sr-only">Recorded context observations</caption><thead><tr><th>Observed</th><th>Context tokens</th></tr></thead><tbody>{samples.slice(-100).map((p, i) => <tr key={i}><td>{timestamp(p.ts)}</td><td>{fmtInt.format(p.contextTokens)}</td></tr>)}</tbody></table></div></details>
    </div> : <div className="session-chart-empty"><span className="session-empty-icon"><IconSpend size={22} /></span><h3>No context observations recorded</h3><p>This source has no context samples for the session. Recorded token totals are available separately.</p></div>}
    <p className="obs-panel-note">Input context includes uncached tokens, cache reads, and cache writes. {provider === 'claude' && 'Subagent and background contexts are excluded. '}Real decreases after compaction or context changes are preserved; this is separate from cumulative session tokens and live plan allowance.</p>
  </section>;
}

function ToolsPanel({ tools }: { tools: { [key: string]: bigint } }) {
  const [expanded, setExpanded] = useState(false);
  const rows = Object.entries(tools).map(([name, count]) => ({ name, count: Number(count) })).sort((a, b) => b.count - a.count);
  const max = rows[0]?.count || 1;
  return <section className="obs-panel" aria-labelledby="tools-title"><div className="obs-panel-heading"><h2 id="tools-title">Tool activity <span className="obs-count">{rows.length}</span></h2><span className="text-xs text-muted">Recorded calls</span></div>
    {rows.length ? <div className="obs-panel-body"><div className="session-tools">{rows.slice(0, expanded ? undefined : 8).map((r) => <div key={r.name} className="session-tool-row"><span className="font-mono text-xs break-all">{r.name}</span><div className="session-tool-track" aria-hidden="true"><span style={{ width: `${r.count / max * 100}%` }} /></div><span className="text-xs tabular-nums text-secondary text-right">{fmtInt.format(r.count)}</span></div>)}</div>{rows.length > 8 && <button className="obs-button mt-5" aria-expanded={expanded} onClick={() => setExpanded(!expanded)}>{expanded ? 'Show fewer tools' : `Show all ${rows.length} tools`}</button>}</div>
      : <PageState title="No tool breakdown recorded">The source does not provide individual tool counts for this session.</PageState>}
  </section>;
}

function ExtensionsPanel({ row }: { row: SessionRow }) {
  return <div className="session-extensions">{[{ title: 'Skills', items: row.skills, empty: 'No skill usage recorded.' }, { title: 'MCP servers', items: row.mcpServers, empty: 'No MCP server activity recorded.' }].map(({ title, items, empty }) => <section className="obs-panel" key={title}><div className="obs-panel-heading"><h2>{title}</h2></div><div className="obs-panel-body flex flex-wrap gap-2">{items.length ? items.map((s) => <span className="session-tag" key={s}>{s}</span>) : <p className="text-[13px] text-muted">{empty}</p>}</div></section>)}</div>;
}

export function AnalysisBody({ id, row, profileName, meta, analysisState, analysis, analysisErr, refetch }: {
  id: string; row: SessionRow; profileName?: string; meta: GetMetaResponse | null;
  analysisState: AnalysisState; analysis: GetAnalysisResponse | null; analysisErr: string | null; refetch: () => void;
}) {
  const now = useNow();
  const run = analysis?.run;
  const stuck = !!(run && meta && isStuck(run, meta.analysisTimeoutS, now));
  const running = run?.state === 'running' && !stuck;
  const done = run?.state === 'done';
  const codex = indexedProvider(providerOf(row));
  const report = done && hasReport(analysis?.report) ? analysis.report : null;
  const failed = run?.state === 'error' || stuck;
  const title = running ? 'Analysis in progress' : failed ? 'Analysis needs attention' : !row.hasTranscript ? 'Transcript unavailable' : 'Understand this session';

  function controls(label: string) {
    if (!row.hasTranscript) return <p className="text-[13px] text-muted leading-6">A new analysis needs the original transcript on disk. Any saved report remains available.</p>;
    if (!meta) return <p className="text-[13px] text-muted" role="status">Analysis options are loading…</p>;
    return <><AnalyseControls id={id} targets={meta.analysisTargets} label={label} busy={!!meta.runnerBusy || !!analysis?.runnerBusy} onStarted={refetch} /><p className="text-xs text-muted leading-5 mt-3">Choose the provider and account that should evaluate this session. Profile hooks remain active.</p></>;
  }
  return <div className="session-analysis-layout"><div className="session-main-column">
    {analysisErr && <Alert tone="error">Analysis could not refresh. {analysisErr} <button className="underline ml-1" onClick={refetch}>Retry loading</button></Alert>}
    {analysisState === 'loading' ? <section className="obs-panel"><PageState title="Loading saved analysis…">Checking for a report generated for this session.</PageState></section>
      : analysisState === 'error' && !analysis ? <section className="obs-panel"><PageState title="Saved analysis is unavailable">Retry loading to check the latest report and runner state.</PageState></section>
      : done ? <>
        <div className="session-report-meta"><span className="inline-flex items-center gap-2"><span className="obs-dot text-ok" />Analysis complete</span><span>{run?.provider ? `${providerLabel(run.provider)} · ${run.profile} · ` : ''}{run?.model || 'Profile default'}{run && run.finishedAt > 0 ? ` · ${agoText(run.finishedAt, now)}` : ''}</span></div>
        {report ? <ReportView report={report} kind="analysis" /> : <section className="obs-panel"><PageState title="Generate a current usage report">{analysis?.reportMd ? 'This saved output uses an unsupported format. Run analysis again to generate a structured usage-only report.' : 'The run completed, but its report could not be loaded.'}</PageState></section>}
        <section className="obs-panel"><div className="obs-panel-heading"><h2>Run another analysis</h2></div><div className="obs-panel-body"><p className="text-[13px] text-muted mb-4">Choose a model to generate a fresh report for this session.</p>{controls('Run analysis again')}</div></section>
      </> : <section className="obs-panel">
        <div className="session-analysis-state"><span className={`session-empty-icon ${running ? 'session-running-icon' : ''}`}><IconInsights size={24} /></span><div className="obs-eyebrow">{running ? `${providerLabel(run?.provider || 'claude')} is reviewing this session` : failed ? 'Run status' : 'Session review'}</div><h2>{title}</h2><p>{running ? 'The runner is reading the transcript and preparing a report. You can leave this page and return when it is ready.' : failed ? 'The run did not produce a usable report. Review the details below before retrying.' : !row.hasTranscript ? 'The original transcript is no longer available locally. Indexed session facts remain available on Overview.' : 'Get a clear account of the work, how the agent used its context, and practical improvements for your next session.'}</p></div>
        {running ? <div className="session-run-facts"><div><span>Model</span><strong>{run?.model || 'Profile default'}</strong></div><div><span>Elapsed</span><strong>{ageText(Math.max(0, now - (run?.startedAt ?? now)))}</strong></div><div><span>Started</span><strong>{timestamp(run?.startedAt ?? 0)}</strong></div></div>
          : <div className="px-6 pb-6">{failed && <Alert tone="error">{stuck ? 'This run has exceeded its expected timeout. The runner may have been interrupted.' : run?.error || 'Analysis failed.'}</Alert>}
            {!failed && row.hasTranscript && <div className="session-review-features"><div><IconSessions /><h3>What happened</h3><p>The work and decisions made during the session.</p></div><div><IconSpend /><h3>How usage added up</h3><p>Context, tools, and repeated work in the recorded evidence.</p></div><div><IconInsights /><h3>What to improve</h3><p>Concrete changes to carry into the next session.</p></div></div>}
            {controls(failed ? 'Retry analysis' : 'Analyse session')}
          </div>}
      </section>}
  </div><aside className="session-sidebar"><section className="obs-panel"><div className="obs-panel-heading"><h2>Report context</h2></div><div className="obs-panel-body"><dl className="session-facts">
    <Fact label="Evidence">This session’s transcript</Fact><Fact label="Provider">{providerLabel(providerOf(row))}</Fact><Fact label="Recorded profile">{profileName || row.profile || 'Unattributed'}</Fact><Fact label="Transcript">{row.hasTranscript ? 'Available locally' : 'Not available locally'}</Fact>
    {run?.provider && <Fact label="Evaluator">{providerLabel(run.provider)} · {run.profile}</Fact>}{run && <Fact label="Analysis model">{run.model || 'Profile default'}</Fact>}{run && run.startedAt > 0 && <Fact label="Started">{timestamp(run.startedAt)}</Fact>}{run && run.finishedAt > 0 && <Fact label="Finished">{timestamp(run.finishedAt)}</Fact>}
  </dl></div></section><section className="obs-panel"><div className="obs-panel-heading"><h2>Reading this report</h2></div><div className="obs-panel-body space-y-3 text-[13px] leading-6 text-muted"><p>Findings describe the recorded session. Expand their evidence to inspect the supporting observations.</p><p>Recommendations are suggestions. Generating a report does not apply configuration changes.</p><p>Included usage stays in tokens, with no assigned dollar value.</p></div></section></aside></div>;
}

export function SessionDetail({ id }: { id: string }) {
  const scope = useScope(); const roster = useAccounts(); const params = useSearchParams(); const href = useSessionLinks();
  const tab = params.get('tab') === 'analysis' ? 'analysis' : 'overview';
  const now = useNow();
  const session = usePoll(useCallback(() => obsClient.getSession({ id }), [id]));
  const meta = usePoll(useCallback(() => obsClient.getMeta({}), []), 10000);
  const [revision, setRevision] = useState(0);
  const [interval, setIntervalMs] = useState(20000);
  const row = session.data?.row;
  const supported = !!row;
  const analysisResult = usePoll(useCallback(async () => {
    void revision;
    if (!supported) return null;
    try { return await obsClient.getAnalysis({ id }); }
    catch (e) { if (e instanceof ConnectError && e.code === Code.NotFound) return null; throw e; }
  }, [id, supported, revision]), interval);
  const analysis = analysisResult.data;
  const analysisState: AnalysisState = analysis ? 'loaded' : analysisResult.error ? 'error' : analysisResult.fetchedAt ? 'none' : 'loading';
  useEffect(() => setIntervalMs(analysis?.run?.state === 'running' ? 3000 : 20000), [analysis?.run?.state]);
  const refetch = () => setRevision((r) => r + 1);

  if (!session.data || !row) return session.error ? <div className="obs-page"><Link className="session-back" href={href('/sessions')}>← Back to Activity</Link><PageState title="Session could not be loaded" error>{session.error}</PageState></div> : <LoadingPage title="Loading session…" />;
  if (scope.accountKey && !roster.data) return roster.error ? <div className="obs-page"><Alert tone="error">Session scope could not be checked. {roster.error}</Alert></div> : <LoadingPage title="Checking session scope…" />;
  const scopedAccount = roster.data?.accounts.find((a) => a.key === scope.accountKey);
  if ((scope.provider && providerOf(row) !== scope.provider) || (scope.profile && row.profile !== scope.profile) || (scope.accountKey && (!scopedAccount || scopedAccount.provider !== providerOf(row) || !scopedAccount.profiles.some((p) => p.profile === row.profile)))) return <div className="obs-page"><Alert tone="error">This session does not belong to the selected scope. Open Activity to choose a session in this account.</Alert><Link className="obs-button" href={href('/sessions')}>Back to Activity</Link></div>;
  const codex = indexedProvider(providerOf(row));
  const profileName = roster.data?.accounts.flatMap((a) => a.profiles).find((p) => p.profile === row.profile && p.provider === providerOf(row))?.displayName;

  return <div className="obs-page session-page">
    <Link className="session-back" href={href('/sessions')}><span aria-hidden="true">←</span> Back to Activity</Link>
    <header className="obs-heading session-heading"><div className="min-w-0"><div className="obs-eyebrow">Session detail</div><h1 className="wrap-break-word">{projectLabel(row)}</h1><div className="session-heading-meta"><ProviderMark provider={providerOf(row)} /><span>{providerLabel(providerOf(row))}</span><span className="session-meta-divider" /><span>{profileName || row.profile || 'Profile unattributed'}</span><span className="session-meta-divider" /><span>{timestamp(row.start)}</span>{session.data.live && <Badge tone="ok">Live</Badge>}</div></div><CopyIdChip id={row.nativeId || row.id} /></header>
    <Subtabs id={id} tab={tab} state={analysis?.run?.state} />
    {session.error && <Alert tone="error">Session refresh failed; showing the last response. {session.error}</Alert>}
    {session.data.errors.map((e, i) => <Alert key={`${e.section}-${i}`}>{e.section}: {e.error}. Session coverage may be partial.</Alert>)}
    {meta.error && <Alert tone="error">Analysis options could not refresh. {meta.error}</Alert>}
    {tab === 'analysis' ? <AnalysisBody id={id} row={row} profileName={profileName} meta={meta.data} analysisState={analysisState} analysis={analysis} analysisErr={analysisResult.error} refetch={refetch} /> : <>
      <SessionMetrics row={row} />
      <div className="session-overview-layout"><div className="session-main-column">
        {<AnalysisSummaryCard id={id} row={row} meta={meta.data} analysisState={analysisState} analysis={analysis} analysisErr={analysisResult.error} now={now} />}
        <ContextTimeline provider={providerOf(row)} points={session.data.trajectory} /><ToolsPanel tools={session.data.tools} /><ExtensionsPanel row={row} />
      </div><PropertiesPanel row={row} profileName={profileName} /></div>
      <p className="session-footnote">Recorded token history is separate from live plan allowance and reported overage.{scope.accountKey ? ' This account view uses its currently linked profiles; historical account ownership may be unverified.' : ''}</p>
    </>}
  </div>;
}
