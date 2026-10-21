'use client';
import { Code, ConnectError } from '@connectrpc/connect';
import { useCallback, useState } from 'react';
import { obsClient } from '@/src/lib/client';
import { useScope } from '@/src/lib/useScope';
import { usePoll } from '@/src/lib/usePoll';
import { useNow } from '@/src/lib/useNow';
import { ageText, agoText } from '@/src/lib/format';
import { hasReport } from '@/src/lib/report';
import { ReportView } from '@/src/components/report/ReportView';
import { Alert, PageState } from '@/src/components/AccountUI';
import { DEPTH_INFO, defaultAdviceDepth, orderAdviceDepths } from '@/src/lib/advice';
import { connectErrorMessage, isStuck } from '@/src/lib/sessionDetail';

import { EvaluatorSelect, initialEvaluator, evaluatorAvailable, type Evaluator } from '@/src/components/report/EvaluatorSelect';

export function Advice() {
  const scope = useScope(); const now = useNow();
  const validated = usePoll(useCallback(() => obsClient.getUsageSnapshot(scope), [scope]));
  const meta = usePoll(useCallback(() => obsClient.getMeta({}), []), 10000);
  const [generation, setGeneration] = useState(0);
  const advice = usePoll(useCallback(async () => {
    void generation;
    try { return await obsClient.getAdvice(scope); }
    catch (e) { if (e instanceof ConnectError && e.code === Code.NotFound) return null; throw e; }
  }, [scope, generation]), 3000);
  const [evaluator, setEvaluator] = useState<Evaluator | null>(null); const [depth, setDepth] = useState('');
  const targets = meta.data?.analysisTargets ?? [];
  const selected = evaluator ?? initialEvaluator(targets);
  const available = evaluatorAvailable(targets, selected);
  const [dispatching, setDispatching] = useState(false); const [startError, setStartError] = useState<string | null>(null);
  const run = advice.data?.run; const stuck = !!(run && advice.data && isStuck(run, advice.data.timeoutS, now));
  const busy = !!meta.data?.runnerBusy || !!advice.data?.runnerBusy || (run?.state === 'running' && !stuck);
  const selectedDepth = depth || (meta.data ? defaultAdviceDepth(meta.data.adviceDepths) : '');
  async function start() {
    setDispatching(true); setStartError(null);
    try { await obsClient.startAdvice({ ...scope, model: selected.model, depth: selectedDepth, executionProvider: selected.provider, executionProfile: selected.profile }); setGeneration((g) => g + 1); }
    catch (e) { setStartError(connectErrorMessage(e)); }
    finally { setDispatching(false); }
  }
  if (validated.error) return <Alert tone="error">This report scope could not be validated. {validated.error}</Alert>;
  return <div>
    {advice.error && <Alert tone="error">Advice could not refresh. {advice.error}</Alert>}
    {meta.error && <Alert tone="error">Analysis options unavailable: {meta.error}</Alert>}
    <p className="obs-alert" data-tone="info">{advice.data?.scopeNote || (scope.accountKey ? 'This report uses the selected account’s currently linked profile history. Historical account ownership may not be verifiable.' : 'Advice covers the recorded evidence within this view.')}</p>
    <section className="obs-panel mb-6"><div className="obs-panel-heading"><h2>{run?.state === 'done' ? 'Generate a new report' : 'Create an advice report'}</h2></div><div className="obs-panel-body">
      <fieldset disabled={dispatching || busy}><legend className="text-xs text-muted mb-3">Report depth</legend><div className="grid gap-3 lg:grid-cols-3">{orderAdviceDepths(meta.data?.adviceDepths ?? []).map((d) => <label key={d} className={`rounded-lg border p-4 cursor-pointer ${selectedDepth === d ? 'border-accent bg-accent-bg' : 'border-line'}`}><span className="flex items-center gap-2 mb-2"><input className="accent-(--accent-default)" type="radio" name="depth" checked={selectedDepth === d} onChange={() => setDepth(d)} /><span className="capitalize font-medium">{d}</span></span><span className="block text-xs text-secondary">{DEPTH_INFO[d]?.reads}</span><span className="block text-xs text-muted mt-2">{DEPTH_INFO[d]?.cost}</span></label>)}</div></fieldset>
      <div className="mt-5 space-y-4"><EvaluatorSelect targets={targets} value={selected} onChange={setEvaluator} disabled={busy || dispatching} /><button className="obs-button obs-button-primary" onClick={start} disabled={!validated.data || dispatching || busy || !available || !selectedDepth}>{dispatching ? 'Starting…' : busy ? 'Runner busy' : run?.state === 'done' ? 'Generate new report' : 'Generate report'}</button></div>
      <p className="obs-muted text-xs mt-3">The evidence scope stays tied to this view. The selected evaluator supplies the credentials and model.</p>
      {startError && <div className="mt-4"><Alert tone="error">{startError}</Alert></div>}
    </div></section>
    {run?.state === 'running' && !stuck && <div className="obs-alert" role="status">Writing with {run.model} · {ageText(now - run.startedAt)} elapsed. You can leave this page; the report continues in the background.</div>}
    {(run?.state === 'error' || stuck) && <Alert tone="error">{stuck ? 'The run exceeded its expected timeout. Check collection health before retrying.' : run?.error}</Alert>}
    {run?.state === 'done' ? <section className="obs-panel"><div className="obs-panel-body"><p className="obs-muted text-xs mb-6">Written {agoText(run.finishedAt, now)} · {run.provider || 'claude'} · {run.model || 'Profile default'} · {run.depth}{advice.data?.executionProfile ? ` · Executed by ${advice.data.executionProfile}` : ''}{run.hasSubagents ? ` · ${run.subagents} subagents` : ''}</p>{hasReport(advice.data?.report) ? <ReportView report={advice.data!.report!} kind="advice" /> : <Alert>This saved output is not in the supported usage-only format. Generate a new report.</Alert>}</div></section> : !run && !advice.error ? <PageState title={advice.fetchedAt ? 'No report for this scope yet' : 'Loading saved report…'} /> : null}
  </div>;
}
