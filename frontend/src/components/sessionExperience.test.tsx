import { create } from '@bufbuild/protobuf';
import type { ReactNode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { GetAnalysisResponseSchema, GetMetaResponseSchema, SessionRowSchema } from '@/src/gen/claudeobs/v1/obs_pb';
import { AnalysisBody, PropertiesPanel, SessionMetrics } from './pages/SessionDetail';
import { AnalysisSummaryCard } from './report/AnalysisSummaryCard';
import { AnalyseControls } from './report/AnalyseControls';

const { renderToStaticMarkup } = require('react-dom/server') as { renderToStaticMarkup(node: ReactNode): string };
const selection = vi.hoisted(() => ({ query: '' }));
vi.mock('next/navigation', () => ({ useSearchParams: () => new URLSearchParams(selection.query) }));
vi.mock('@/src/lib/useNow', () => ({ useNow: () => 2_000_000_000 }));
vi.mock('@/src/lib/client', () => ({ obsClient: {} }));
beforeEach(() => { selection.query = ''; });

const meta = create(GetMetaResponseSchema, { analysisModels: ['sonnet', 'opus'], analysisTimeoutS: 1500 });
const row = create(SessionRowSchema, { id: 'session-1', provider: 'claude', profile: 'work', hasTranscript: true });
const saved = create(GetAnalysisResponseSchema, {
  run: { state: 'done', model: 'sonnet', finishedAt: 1_999_999_900 },
  report: { allowanceAware: true, headline: 'Keep the useful context', summary: 'The session completed its recorded work.', actions: [{ doThis: 'Reuse the focused checks', why: 'They gave relevant feedback.' }] },
});

function analysis(overrides: Partial<Parameters<typeof AnalysisBody>[0]> = {}) {
  return renderToStaticMarkup(<AnalysisBody id={row.id} row={row} meta={meta} analysisState="none" analysis={null} analysisErr={null} refetch={() => {}} {...overrides} />);
}

describe('session analysis states', () => {
  it('keeps a saved report readable after its transcript is gone, while preventing a new run', () => {
    const html = analysis({ row: create(SessionRowSchema, { ...row, hasTranscript: false }), analysis: saved, analysisState: 'loaded' });
    expect(html).toContain('Keep the useful context');
    expect(html).toContain('Reuse the focused checks');
    expect(html).toContain('original transcript on disk');
    expect(html).not.toContain('<select');
  });

  it('keeps the last report visible when its refresh fails and offers retry', () => {
    const html = analysis({ analysis: saved, analysisState: 'loaded', analysisErr: 'Connection lost' });
    expect(html).toContain('Keep the useful context');
    expect(html).toContain('Connection lost');
    expect(html).toContain('Retry loading');
  });

  it('offers transcript analysis for a Codex session', () => {
    const html = analysis({ row: create(SessionRowSchema, { ...row, provider: 'codex' }) });
    expect(html).toContain('Run analysis with');
    expect(html).toContain('Analysis provider');
    expect(html).toContain('Analyse session');
  });

  it('names the chosen evaluator while keeping source provenance separate', () => {
    const html = analysis({ analysisState: 'loaded', analysis: create(GetAnalysisResponseSchema, { run: { state: 'running', provider: 'codex', profile: 'review-account', startedAt: 1_999_999_970 } }) });
    expect(html).toContain('Codex is reviewing this session');
    expect(html).toContain('Codex · review-account');
    expect(html).toContain('Recorded profile');
    expect(html).toContain('Profile default');
  });

  it('shows real run metadata without offering a second run while one is active', () => {
    const html = analysis({ analysisState: 'loaded', analysis: create(GetAnalysisResponseSchema, { run: { state: 'running', model: 'opus', startedAt: 1_999_999_970 } }) });
    expect(html).toContain('Analysis in progress');
    expect(html).toContain('opus');
    expect(html).toContain('Elapsed');
    expect(html).not.toContain('<select');
  });

  it('preserves model choice and retry controls after a failed run', () => {
    const html = analysis({ analysisState: 'loaded', analysis: create(GetAnalysisResponseSchema, { run: { state: 'error', error: 'Runner exited unexpectedly' } }) });
    expect(html).toContain('Runner exited unexpectedly');
    expect(html).toContain('Retry analysis');
    expect(html).toContain('<select');
  });

  it('respects the configured timeout and shared runner when a run appears stuck', () => {
    const html = analysis({ analysisState: 'loaded', analysis: create(GetAnalysisResponseSchema, { runnerBusy: true, run: { state: 'running', startedAt: 1_999_998_199 } }) });
    expect(html).toContain('exceeded its expected timeout');
    expect(html).toContain('Runner busy');
    expect(html).toMatch(/<button[^>]*disabled=""/);
  });

  it('does not present old monetary report prose as a current usage analysis', () => {
    const html = analysis({ analysisState: 'loaded', analysis: create(GetAnalysisResponseSchema, { run: { state: 'done' }, report: { headline: 'Included tokens worth $900', summary: 'Estimated value $800' } }) });
    expect(html).toContain('Generate a new report');
    expect(html).not.toContain('$900');
    expect(html).not.toContain('$800');
  });

  it('cannot dispatch without an available evaluator profile', () => {
    const html = renderToStaticMarkup(<AnalyseControls id="session-1" targets={[]} label="Analyse session" busy={false} onStarted={() => {}} />);
    expect(html).toContain('Add a profile in Settings');
    expect(html).toMatch(/<button[^>]*disabled=""/);
  });
});

describe('session overview continuity', () => {
  it('keeps account, provider, profile and Activity filters in its analysis link', () => {
    selection.query = 'accountKey=claude%3Awork&provider=claude&profile=work&q=compiler&days=7&sort=tokens&id=old&tab=overview';
    const html = renderToStaticMarkup(<AnalysisSummaryCard id="session-1" row={row} meta={meta} analysisState="loaded" analysis={saved} now={2_000_000_000} />);
    const link = html.match(/href="([^"]+)"/)?.[1].replaceAll('&amp;', '&');
    const params = new URL(link!, 'http://localhost').searchParams;
    expect(Object.fromEntries(params)).toEqual({ accountKey: 'claude:work', provider: 'claude', profile: 'work', q: 'compiler', days: '7', sort: 'tokens', id: 'session-1', tab: 'analysis' });
  });

  it('marks a retained overview summary as stale after a refresh error', () => {
    const html = renderToStaticMarkup(<AnalysisSummaryCard id={row.id} row={row} meta={meta} analysisState="loaded" analysis={saved} analysisErr="Connection lost" now={2_000_000_000} />);
    expect(html).toContain('Keep the useful context');
    expect(html).toContain('Showing the last response');
    expect(html).toContain('Connection lost');
  });

  it('distinguishes missing Codex metrics from measured zero and partial observations', () => {
    const unknown = create(SessionRowSchema, { provider: 'codex' });
    const missing = renderToStaticMarkup(<SessionMetrics row={unknown} />);
    expect(missing.match(/Unavailable/g)).toHaveLength(4);
    const partial = renderToStaticMarkup(<SessionMetrics row={create(SessionRowSchema, { provider: 'codex', tokens: { input: 100 }, requests: 2 })} />);
    expect(partial).toContain('100+');
    expect(partial).toContain('2+');
    const measured = renderToStaticMarkup(<SessionMetrics row={create(SessionRowSchema, { provider: 'codex', tokens: {}, usageKnown: true, toolsKnown: true, durationKnown: true })} />);
    expect(measured).not.toContain('Unavailable');
    expect(measured).not.toContain('+');
    expect(renderToStaticMarkup(<PropertiesPanel row={unknown} />)).toContain('No token breakdown is available');
    const missingCounts = renderToStaticMarkup(<PropertiesPanel row={create(SessionRowSchema, { provider: 'codex', tokens: {} })} />);
    expect(missingCounts).toContain('Token count unavailable');
    expect(missingCounts).not.toContain('0 recorded tokens');
  });
});
