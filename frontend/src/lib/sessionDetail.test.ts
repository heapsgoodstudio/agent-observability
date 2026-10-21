import { ConnectError } from '@connectrpc/connect';
import { describe, expect, it } from 'vitest';
import type { AnalysisRun, SectionError } from '../gen/claudeobs/v1/obs_pb';
import {
  connectErrorMessage,
  defaultAnalysisModel,
  isStuck,
  orderAnalysisModels,
  resolveControlsGate,
  resolveRealCostTile,
  stuckAfter,
} from './sessionDetail';

function run(over: Partial<AnalysisRun>): AnalysisRun {
  return {
    $typeName: 'claudeobs.v1.AnalysisRun',
    provider: '', profile: '', sourceProvider: '', sourceProfile: '', sourceAccountKey: '',
    state: 'running', model: 'sonnet', depth: '', startedAt: 0, finishedAt: 0, error: '',
    runSessionId: '', subagents: 0, hasSubagents: false,
    ...over,
  };
}

describe('stuckAfter', () => {
  it('adds the 300s grace to the reported timeout, never a literal', () => {
    expect(stuckAfter(1500)).toBe(1800);
    expect(stuckAfter(2700)).toBe(3000);
  });
});

describe('isStuck', () => {
  it('is false for a non-running state', () => {
    expect(isStuck(run({ state: 'done' }), 1500, 10_000)).toBe(false);
  });

  it('is false within the threshold', () => {
    const r = run({ state: 'running', startedAt: 100 });
    expect(isStuck(r, 1500, 100 + 1799)).toBe(false);
  });

  it('is true past the threshold', () => {
    const r = run({ state: 'running', startedAt: 100 });
    expect(isStuck(r, 1500, 100 + 1801)).toBe(true);
  });
});

describe('orderAnalysisModels', () => {
  it('puts sonnet first, then opus, then haiku, matching v1s fixed order', () => {
    expect(orderAnalysisModels(['haiku', 'opus', 'sonnet'])).toEqual(['sonnet', 'opus', 'haiku']);
  });

  it('appends any unrecognised model alphabetically after the known three', () => {
    expect(orderAnalysisModels(['sonnet', 'zzz-future', 'haiku'])).toEqual(['sonnet', 'haiku', 'zzz-future']);
  });

  it('drops models absent from the list rather than inventing them', () => {
    expect(orderAnalysisModels(['haiku'])).toEqual(['haiku']);
  });
});

describe('defaultAnalysisModel', () => {
  it('prefers sonnet when present', () => {
    expect(defaultAnalysisModel(['haiku', 'opus', 'sonnet'])).toBe('sonnet');
  });

  it('falls back to the first ordered model otherwise', () => {
    expect(defaultAnalysisModel(['haiku', 'opus'])).toBe('opus');
  });
});

describe('resolveControlsGate', () => {
  it('reports aged-out when the transcript is gone, even before meta has loaded', () => {
    expect(resolveControlsGate(false, false)).toBe('aged-out');
    expect(resolveControlsGate(false, true)).toBe('aged-out');
  });

  it('reports loading when the transcript is present but GetMeta has not resolved yet', () => {
    expect(resolveControlsGate(true, false)).toBe('loading');
  });

  it('reports ready once the transcript is present and meta has loaded', () => {
    expect(resolveControlsGate(true, true)).toBe('ready');
  });
});

describe('resolveRealCostTile', () => {
  const overageSectionError: SectionError = { $typeName: 'claudeobs.v1.SectionError', section: 'overage', error: 'vm down' };
  const otherSectionError: SectionError = { $typeName: 'claudeobs.v1.SectionError', section: 'live', error: 'x' };

  it('omits the tile when the overage load itself failed', () => {
    expect(resolveRealCostTile([overageSectionError], false)).toBe('omitted');
  });

  it('reports observed when the load succeeded and the session was seen', () => {
    expect(resolveRealCostTile([], true)).toBe('observed');
  });

  it('reports not-observed when the load succeeded but the session predates it', () => {
    expect(resolveRealCostTile([], false)).toBe('not-observed');
    expect(resolveRealCostTile([otherSectionError], false)).toBe('not-observed');
  });
});

describe('connectErrorMessage', () => {
  it('strips the [code] prefix a ConnectError message carries', () => {
    const err = new ConnectError('another analysis is already running', 6 /* AlreadyExists */);
    expect(connectErrorMessage(err)).toBe('another analysis is already running');
  });

  it('falls back to a plain Error message', () => {
    expect(connectErrorMessage(new Error('boom'))).toBe('boom');
  });

  it('stringifies anything else', () => {
    expect(connectErrorMessage('boom')).toBe('boom');
  });
});
