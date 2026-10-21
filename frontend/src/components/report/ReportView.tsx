import type { ReactNode } from 'react';
import type { Report } from '@/src/gen/claudeobs/v1/obs_pb';
import { Badge } from '@/src/components/ui/Badge';
import { Banner } from '@/src/components/ui/Banner';
import {
  actionMeta,
  avoidableText,
  costLabel,
  COST_TONE,
  HEADINGS,
  verdictLabel,
  VERDICT_TONE,
} from '@/src/lib/report';

// Shared by session analysis and scoped advice. Keep the report's own order
// within each section; actions lead so the reader can act before reading the
// supporting detail. Compatibility monetary fields are never rendered.
export function ReportView({ report, kind }: { report: Report; kind: 'analysis' | 'advice' }) {
  if (!report.allowanceAware) return <Banner tone="info">{report.schema < 2 ? 'This report uses an older reporting format.' : 'This report does not meet the usage-only accounting requirements.'} Generate a new report to review usage without assigning dollar values to included tokens.</Banner>;
  const headings = HEADINGS[kind];
  const verdict = verdictLabel(report.verdict);
  const avoidable = avoidableText(report.avoidable);

  return (
    <article className="min-w-0 space-y-6 wrap-break-word" aria-label={kind === 'analysis' ? 'Session analysis report' : 'Usage advice report'}>
      <header className="obs-panel">
        <div className="obs-panel-body">
          <div className="mb-5 flex flex-wrap items-center justify-between gap-3">
            <p className="m-0 text-[11px] font-medium uppercase tracking-[0.12em] text-muted">{kind === 'analysis' ? 'Session analysis' : 'Usage advice'}</p>
            {verdict ? <Badge tone={VERDICT_TONE[verdict]}>{verdict}</Badge> : null}
          </div>
          <h2 className="m-0 max-w-[30em] text-[23px] font-medium leading-[1.35] tracking-[-0.55px] text-primary sm:text-[27px]">{report.headline}</h2>
          <p className="m-0 mt-4 max-w-[72ch] whitespace-pre-line text-[14px] leading-[1.8] text-secondary">{report.summary}</p>
          {avoidable ? <p className="m-0 mt-5 text-[12px] leading-5 text-muted"><span className="font-medium text-secondary">Opportunity to reduce usage: </span>{avoidable}.</p> : null}
        </div>
        {report.coverage ? (
          <div className="border-t border-hairline bg-raised px-[22px] py-4">
            <p className="m-0 mb-1.5 text-[11px] font-medium uppercase tracking-[0.06em] text-muted">Evidence coverage</p>
            <p className="m-0 max-w-[85ch] whitespace-pre-line text-[12px] leading-[1.75] text-secondary">{report.coverage}</p>
          </div>
        ) : null}
      </header>

      {report.actions.length ? (
        <ReportSection title={headings.actions} count={report.actions.length}>
          <ol role="list" className="m-0 list-none divide-y divide-hairline p-0">
            {report.actions.map((action, index) => {
              const meta = actionMeta(action);
              return (
                <li key={`${index}:${action.doThis}`} className={`flex gap-4 px-[22px] py-5 ${index === 0 ? 'bg-accent-bg' : ''}`}>
                  <span aria-hidden="true" className={`mt-0.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-full border font-mono text-[11px] tabular-nums ${index === 0 ? 'border-accent text-accent-text' : 'border-line text-muted'}`}>{String(index + 1).padStart(2, '0')}</span>
                  <div className="min-w-0 flex-1">
                    <h4 className="m-0 max-w-[72ch] text-[14px] font-medium leading-[1.65] text-primary">{action.doThis}</h4>
                    {meta ? <p className="m-0 mt-1.5 text-[11px] leading-5 text-muted">{meta}</p> : null}
                    {action.verification ? <p className="m-0 mt-2 text-[12px] leading-5 text-secondary"><span className="font-medium">Verify the change: </span>{action.verification}</p> : null}
                    {action.why ? <p className="m-0 mt-2 max-w-[75ch] whitespace-pre-line text-[13px] leading-[1.75] text-secondary">{action.why}</p> : null}
                  </div>
                </li>
              );
            })}
          </ol>
        </ReportSection>
      ) : null}

      {report.story.length ? (
        <ReportSection title={headings.story}>
          <div className="obs-panel-body">
            <ul className="m-0 max-w-[80ch] list-disc space-y-3 pl-5 text-[13px] leading-[1.8] text-secondary marker:text-muted">
              {report.story.map((story, index) => <li key={`${index}:${story}`} className="whitespace-pre-line pl-1">{story}</li>)}
            </ul>
          </div>
        </ReportSection>
      ) : null}

      {report.problems.length ? (
        <ReportSection title={headings.problems} count={report.problems.length}>
          <div className="divide-y divide-hairline">
            {report.problems.map((problem, index) => {
              const impact = costLabel(problem.cost);
              return (
                <section key={`${index}:${problem.title}`} className="obs-panel-body" aria-label={problem.title}>
                  <div className="mb-4 flex flex-wrap items-start justify-between gap-3">
                    <h4 className="m-0 min-w-0 flex-1 text-[14px] font-medium leading-[1.6] text-primary">{problem.title}</h4>
                    {impact ? <Badge tone={COST_TONE[problem.cost]}>{impact}</Badge> : null}
                  </div>
                  {(problem.confidence || problem.category) && <p className="text-xs text-muted mb-3">{problem.category}{problem.category && problem.confidence ? " · " : ""}{problem.confidence ? `Confidence: ${problem.confidence} (evidence assessment)` : ""}</p>}
                  <dl className="m-0 space-y-3.5">
                    {problem.whatHappened ? <Detail label="What happened">{problem.whatHappened}</Detail> : null}
                    {problem.whyItCosts ? <Detail label="Usage impact">{problem.whyItCosts}</Detail> : null}
                    {problem.fix ? <Detail label="Suggested change" highlighted>{problem.fix}</Detail> : null}
                  </dl>
                  {problem.evidence.length ? (
                    <details className="mt-5 rounded-md border border-hairline bg-raised">
                      <summary className="cursor-pointer rounded-md px-3.5 py-3 text-[12px] font-medium text-secondary focus-visible:outline-solid focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent">
                        Supporting evidence <span className="ml-1 font-normal text-muted">({problem.evidence.length})</span>
                      </summary>
                      <ul className="m-0 list-disc space-y-2 border-t border-hairline py-3 pl-8 pr-4 font-mono text-[11px] leading-[1.8] text-muted marker:text-faint">
                        {problem.evidence.map((evidence, evidenceIndex) => <li key={`${evidenceIndex}:${evidence}`} className="whitespace-pre-wrap wrap-anywhere">{evidence}</li>)}
                      </ul>
                    </details>
                  ) : null}
                </section>
              );
            })}
          </div>
        </ReportSection>
      ) : null}

      {report.configChanges.length ? (
        <ReportSection title={headings.config} count={report.configChanges.length}>
          <div className="divide-y divide-hairline">
            {report.configChanges.map((change, index) => (
              <div key={`${index}:${change.file}:${change.setting}`} className="obs-panel-body">
                {change.file ? <p className="m-0 mb-2 font-mono text-[11px] leading-[1.8] text-muted wrap-anywhere">{change.file}</p> : null}
                {change.setting ? <h4 className="m-0 font-mono text-[13px] font-medium leading-[1.7] text-primary wrap-anywhere">{change.setting}</h4> : null}
                <div className="mt-3 rounded-md border border-hairline bg-raised px-4 py-3.5">
                  <p className="m-0 mb-1.5 text-[10px] font-medium uppercase tracking-[0.06em] text-muted">Proposed change</p>
                  <p className="m-0 max-w-[80ch] whitespace-pre-wrap text-[13px] leading-[1.75] text-secondary wrap-anywhere">{change.change}</p>
                </div>
                {change.why ? <p className="m-0 mt-3 max-w-[80ch] whitespace-pre-line text-[12px] leading-[1.8] text-muted">{change.why}</p> : null}
              </div>
            ))}
          </div>
        </ReportSection>
      ) : null}

      {report.good.length || report.fine.length ? (
        <div className={`grid min-w-0 items-start gap-6 ${report.good.length && report.fine.length ? 'xl:grid-cols-2' : ''}`}>
          {report.good.length ? (
            <ReportSection title={headings.good} count={report.good.length}>
              <div className="divide-y divide-hairline">
                {report.good.map((good, index) => (
                  <div key={`${index}:${good.title}`} className="obs-panel-body">
                    <h4 className="m-0 text-[13px] font-medium leading-[1.7] text-primary">{good.title}</h4>
                    {good.what ? <p className="m-0 mt-2 max-w-[80ch] whitespace-pre-line text-[13px] leading-[1.8] text-secondary">{good.what}</p> : null}
                    {good.whyGood ? <p className="m-0 mt-2 max-w-[80ch] whitespace-pre-line text-[12px] leading-[1.8] text-muted">{good.whyGood}</p> : null}
                  </div>
                ))}
              </div>
            </ReportSection>
          ) : null}
          {report.fine.length ? (
            <ReportSection title={headings.fine} count={report.fine.length}>
              <div className="divide-y divide-hairline">
                {report.fine.map((fine, index) => (
                  <div key={`${index}:${fine.title}`} className="obs-panel-body">
                    <h4 className="m-0 text-[13px] font-medium leading-[1.7] text-primary">{fine.title}</h4>
                    {fine.whyFine ? <p className="m-0 mt-2 max-w-[80ch] whitespace-pre-line text-[13px] leading-[1.8] text-secondary">{fine.whyFine}</p> : null}
                  </div>
                ))}
              </div>
            </ReportSection>
          ) : null}
        </div>
      ) : null}
    </article>
  );
}

function ReportSection({ title, count, children }: { title: string; count?: number; children: ReactNode }) {
  return (
    <section className="obs-panel" aria-label={title}>
      <div className="obs-panel-heading pb-4!">
        <h3 className="m-0 text-[14px] font-medium tracking-[-0.15px] text-primary">{title}</h3>
        {count !== undefined ? <span className="obs-count" aria-label={`${count} items`}>{count}</span> : null}
      </div>
      <div className="border-t border-hairline">{children}</div>
    </section>
  );
}

function Detail({ label, children, highlighted = false }: { label: string; children: ReactNode; highlighted?: boolean }) {
  return (
    <div className={`grid gap-1.5 sm:grid-cols-[116px_minmax(0,1fr)] sm:gap-5 ${highlighted ? 'rounded-md bg-raised px-3 py-3' : ''}`}>
      <dt className="text-[11px] font-medium leading-[1.8] text-muted">{label}</dt>
      <dd className="m-0 max-w-[75ch] whitespace-pre-line text-[13px] leading-[1.8] text-secondary">{children}</dd>
    </div>
  );
}
