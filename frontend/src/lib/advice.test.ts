import { describe, expect, it } from 'vitest';
import { ConnectError } from '@connectrpc/connect';
import type { AnalysisRun } from '../gen/claudeobs/v1/obs_pb';
import { isStuck, stuckAfter, connectErrorMessage } from './sessionDetail';
import {
  DEPTH_INFO,
  defaultAdviceDepth,
  orderAdviceDepths,
} from './advice';

function run(over: Partial<AnalysisRun>): AnalysisRun {
  return {
    $typeName: 'claudeobs.v1.AnalysisRun',
    provider: '', profile: '', sourceProvider: '', sourceProfile: '', sourceAccountKey: '',
    state: 'running', model: 'sonnet', depth: 'standard', startedAt: 0, finishedAt: 0, error: '',
    runSessionId: '', subagents: 0, hasSubagents: false,
    ...over,
  };
}

describe('orderAdviceDepths', () => {
  it('puts standard first (the default), then shallow, then evidence, matching v1s fixed picker order', () => {
    // GetMeta.adviceDepths arrives alphabetical (server.go's sortedKeys).
    expect(orderAdviceDepths(['evidence', 'shallow', 'standard'])).toEqual(['standard', 'shallow', 'evidence']);
  });

  it('appends any unrecognised depth alphabetically after the known three', () => {
    expect(orderAdviceDepths(['evidence', 'standard', 'zzz-future'])).toEqual(['standard', 'evidence', 'zzz-future']);
  });

  it('drops depths absent from the list rather than inventing them', () => {
    expect(orderAdviceDepths(['shallow'])).toEqual(['shallow']);
  });
});

describe('defaultAdviceDepth', () => {
  it('prefers standard when present, matching advice.js\'s module-level default', () => {
    expect(defaultAdviceDepth(['evidence', 'shallow', 'standard'])).toBe('standard');
  });

  it('falls back to the first ordered depth otherwise', () => {
    expect(defaultAdviceDepth(['evidence', 'shallow'])).toBe('shallow');
  });
});

describe('DEPTH_INFO', () => {
  it('describes bounded evidence instead of promising complete coverage', () => {
    expect(DEPTH_INFO.standard.reads).toContain('200');
    expect(DEPTH_INFO.evidence.reads).toContain('ten');
    expect(DEPTH_INFO.shallow.reads).not.toContain('transcripts');
  });
});

// The stuck chooser. GetAdviceResponse.timeoutS resolves the depth choice
// server-side (internal/server/analysis.go's GetAdvice: the evidence timeout
// when the recorded run's depth is "evidence", the analysis timeout
// otherwise), so this suite proves the generic isStuck/stuckAfter
// (lib/sessionDetail.ts, already unit-tested there in the abstract) produces
// the right real-world thresholds when fed that resolved timeoutS, rather
// than re-deriving the depth choice a second time from GetMeta's raw
// fields.
describe('the stuck chooser, applied to advice-shaped runs', () => {
  it('is not stuck for a standard/shallow run within 1800s of its analysis-timeout-derived threshold', () => {
    // GetAdvice would set timeoutS=1500 (analysis.DefaultAnalysisTimeout)
    // for a non-evidence depth; stuckAfter adds the 300s grace.
    expect(stuckAfter(1500)).toBe(1800);
    const r = run({ state: 'running', depth: 'standard', startedAt: 0 });
    expect(isStuck(r, 1500, 1799)).toBe(false);
    expect(isStuck(r, 1500, 1801)).toBe(true);
  });

  it('is not stuck for an evidence run within 3000s of its evidence-timeout-derived threshold', () => {
    // GetAdvice would set timeoutS=2700 (analysis.DefaultEvidenceTimeout)
    // when the recorded run's depth is "evidence".
    expect(stuckAfter(2700)).toBe(3000);
    const r = run({ state: 'running', depth: 'evidence', startedAt: 0 });
    expect(isStuck(r, 2700, 2999)).toBe(false);
    expect(isStuck(r, 2700, 3001)).toBe(true);
  });
});

// Connect code mapping (StartAdvice, internal/server/analysis.go):
// AlreadyExists carries analysis.ErrBusy's own wording ("another run is
// already in progress"), ResourceExhausted carries ErrFindingsTooLarge's
// ("findings payload too large"), InvalidArgument carries ErrBadModel's or
// ErrBadDepth's. connectErrorMessage (sessionDetail.ts) is code-agnostic by
// design (it only strips the ConnectError "[code] " prefix), so this proves
// all three of Advice's specific codes come through unprefixed.
describe('connectErrorMessage over StartAdvice\'s three error codes', () => {
  it('strips the prefix for AlreadyExists (the busy 409)', () => {
    const err = new ConnectError('another run is already in progress', 6 /* AlreadyExists */);
    expect(connectErrorMessage(err)).toBe('another run is already in progress');
  });

  it('strips the prefix for ResourceExhausted (findings payload too large)', () => {
    const err = new ConnectError('findings payload too large', 8 /* ResourceExhausted */);
    expect(connectErrorMessage(err)).toBe('findings payload too large');
  });

  it('strips the prefix for InvalidArgument (bad model or depth)', () => {
    const err = new ConnectError('depth must be one of shallow, standard, evidence', 3 /* InvalidArgument */);
    expect(connectErrorMessage(err)).toBe('depth must be one of shallow, standard, evidence');
  });
});
