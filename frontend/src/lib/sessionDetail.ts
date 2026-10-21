// Pure helpers for the session detail Analysis tab
// (components/pages/SessionDetail.tsx): the stuck-run check, the model
// <select>'s fixed order, and the runner-busy error wording. No DOM, no
// fetching, no module state.
import { ConnectError } from '@connectrpc/connect';
import type { AnalysisRun, SectionError } from '../gen/claudeobs/v1/obs_pb';

// The stuck threshold is the runner's own timeout plus a grace margin, taken
// as a parameter rather than hardcoded: it must be computed from
// GetMeta.analysis_timeout_s so that a runtime override of ANALYSIS_TIMEOUT_S
// is honored here too. A hardcoded copy drifted below the real timeout once
// already, and reported healthy runs as stuck.
const STUCK_GRACE_S = 300;

export function stuckAfter(analysisTimeoutS: number): number {
  return analysisTimeoutS + STUCK_GRACE_S;
}

export function isStuck(run: Pick<AnalysisRun, 'state' | 'startedAt'>, analysisTimeoutS: number, now: number): boolean {
  return run.state === 'running' && now - run.startedAt > stuckAfter(analysisTimeoutS);
}

// The model <select>'s display order (sonnet, the default, first): a UI
// convention with no server counterpart, since StartAnalysisRequest.model has
// no default and the server rejects an unrecognised or empty model outright.
// GetMeta.analysis_models is the real list of what exists, but arrives
// alphabetical, so this re-orders it and appends anything beyond the known
// three alphabetically after them, letting a future fourth model appear
// without a fixed position.
const PREFERRED_MODEL_ORDER = ['sonnet', 'opus', 'haiku'];

export function orderAnalysisModels(models: string[]): string[] {
  const known = PREFERRED_MODEL_ORDER.filter((m) => models.includes(m));
  const rest = models.filter((m) => !PREFERRED_MODEL_ORDER.includes(m)).sort();
  return [...known, ...rest];
}

// The <select>'s initial value: sonnet when the server actually offers it,
// otherwise the first model in the same ordered list.
export function defaultAnalysisModel(models: string[]): string {
  const ordered = orderAnalysisModels(models);
  return ordered.includes('sonnet') ? 'sonnet' : ordered[0];
}

export type ControlsGate = 'aged-out' | 'loading' | 'ready';

// Whether the (re-)run controls can be offered. The has_transcript check
// comes first, so a done run's report still renders in full while only the
// controls beneath it are replaced by the aged-out note. 'aged-out' takes
// precedence over 'loading', so the note wins even before GetMeta has
// resolved rather than flashing a spinner for something that can never run.
export function resolveControlsGate(hasTranscript: boolean, metaLoaded: boolean): ControlsGate {
  if (!hasTranscript) return 'aged-out';
  if (!metaLoaded) return 'loading';
  return 'ready';
}

export type RealCostTileState = 'omitted' | 'observed' | 'not-observed';

// The real-cost tile has three states, but GetSessionResponse collapses two
// of them onto one bool: a failed overage load and a genuinely-unobserved
// session both leave overageObserved false. They are told apart here by
// checking for a SectionError{section:"overage"}, so a load failure omits the
// tile entirely rather than claiming the session was not billed.
export function resolveRealCostTile(errors: SectionError[], overageObserved: boolean): RealCostTileState {
  if (errors.some((e) => e.section === 'overage')) return 'omitted';
  return overageObserved ? 'observed' : 'not-observed';
}

// ConnectError.message carries a "[code] " prefix by design, so that a raw
// console log always shows the code. The page wants the bare text instead, so
// this reads rawMessage: that lets the server's own per-code wording
// (internal/server/analysis.go's errAnalysisBusy et al.) reach the user
// unprefixed and unchanged.
export function connectErrorMessage(err: unknown): string {
  if (err instanceof ConnectError) return err.rawMessage;
  if (err instanceof Error) return err.message;
  return String(err);
}
