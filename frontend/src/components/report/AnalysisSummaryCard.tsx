'use client';

import Link from 'next/link';
import { useSessionLinks } from '@/src/lib/useSessionLinks';
import type { GetAnalysisResponse, GetMetaResponse, SessionRow } from '@/src/gen/claudeobs/v1/obs_pb';
import { Badge } from '@/src/components/ui/Badge';
import { IconInsights } from '@/src/components/icons';
import { Alert } from '@/src/components/AccountUI';
import { ageText, agoText } from '@/src/lib/format';
import { hasReport, topAction, verdictLabel, VERDICT_TONE } from '@/src/lib/report';
import { isStuck } from '@/src/lib/sessionDetail';

// Overview gives the useful takeaway; the Analysis tab owns model selection
// and run controls. Links preserve the complete Activity/account scope.
export function AnalysisSummaryCard({ id, row, meta, analysisState, analysis, analysisErr, now }: {
  id: string;
  row: SessionRow;
  meta: GetMetaResponse | null;
  analysisState: 'loading' | 'none' | 'loaded' | 'error';
  analysis: GetAnalysisResponse | null;
  analysisErr?: string | null;
  now: number;
  refetch?: () => void;
}) {
  const sessionLink = useSessionLinks();
  const run = analysis?.run;
  const stuck = !!(run && meta && isStuck(run, meta.analysisTimeoutS, now));
  const report = run?.state === 'done' && hasReport(analysis?.report) && analysis.report.allowanceAware ? analysis.report : null;
  const action = report ? topAction(report) : null;
  const verdict = report ? verdictLabel(report.verdict) : null;
  const running = run?.state === 'running' && !stuck;
  const failed = run?.state === 'error' || stuck || analysisState === 'error';
  const href = sessionLink(`/sessions?id=${encodeURIComponent(id)}&tab=analysis`);

  return <section className="obs-panel session-analysis-summary" aria-labelledby="analysis-summary-title">
    <div className="obs-panel-heading">
      <h2 id="analysis-summary-title" className="flex items-center gap-2"><span className="text-accent-text"><IconInsights /></span>Session analysis</h2>
      {verdict ? <Badge tone={VERDICT_TONE[verdict]}>{verdict}</Badge> : <span className="text-xs text-muted">{analysisState === 'loading' ? 'Checking report' : running ? 'In progress' : failed ? 'Needs attention' : run?.state === 'done' ? 'Report available' : 'Not analysed'}</span>}
    </div>
    <div className="obs-panel-body">
      {analysisErr && <Alert tone="error">Analysis could not refresh. {analysis ? 'Showing the last response. ' : ''}{analysisErr}</Alert>}
      <h3 className="text-[16px] font-medium tracking-[-.2px] leading-snug">{report?.headline || (analysisState === 'loading' ? 'Loading saved analysis…' : running ? 'Your analysis is running' : failed ? 'Review the analysis status' : !row.hasTranscript ? 'Session records are available' : 'Understand what worked and what to improve')}</h3>
      <p className="text-[13px] leading-6 text-secondary mt-2">{report?.summary || (analysisState === 'loading' ? 'Checking whether a report has been generated for this session.' : running ? `Analysing with ${run?.model}. ${ageText(now - (run?.startedAt ?? now))} elapsed. You can continue exploring while it runs.` : failed ? 'Open Analysis for the error details and available retry controls.' : !row.hasTranscript ? 'The original transcript is no longer on disk. Recorded activity remains available; a new analysis needs the transcript.' : 'Review the session’s approach, usage patterns, and practical next steps using its recorded transcript.')}</p>
      {action && <div className="session-top-action"><span className="obs-eyebrow mb-1!">Suggested next step</span><p className="text-[13px] text-secondary leading-6">{action.doThis}</p></div>}
      <div className="flex flex-wrap items-center justify-between gap-3 mt-5">
        <Link className="obs-button" href={href}>{report ? 'Read full analysis' : running ? 'View progress' : failed ? 'Review analysis' : run?.state === 'done' ? 'Review saved report' : 'Open analysis'} <span aria-hidden="true">→</span></Link>
        {run?.state === 'done' && run.finishedAt > 0 && <span className="text-xs text-muted">{agoText(run.finishedAt, now)} · {run.model}</span>}
      </div>
    </div>
  </section>;
}
