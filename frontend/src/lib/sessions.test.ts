import { describe, expect, it } from 'vitest';
import type { SessionRow } from '../gen/claudeobs/v1/obs_pb';
import { deliveryText, filterSessions, indexAgeText, profileLabel, projectLabel, sortSessions } from './sessions';

function row(over: Partial<SessionRow>): SessionRow {
  return {
    $typeName: 'claudeobs.v1.SessionRow',
    id: 'id', project: 'proj', workspace: '', profile: '', start: 0, end: 0, costUsd: 0, requests: 0,
    models: [], sidechainCostUsd: 0, toolCalls: 0, userTurns: 0, skills: [], mcpServers: [],
    hasTranscript: true, analysed: false, live: false, billedOverlap: false,
    commits: 0, pullRequests: 0,
    provider: '', nativeId: '', parentId: '', costKnown: false, usageKnown: false,
    reasoningTokens: 0, errorCount: 0, durationMs: 0,
    toolsKnown: false, errorsKnown: false, durationKnown: false,
    ...over,
  };
}

describe('sortSessions', () => {
  const rows = [
    row({ id: 'a', costUsd: 999, end: 300, tokens: { $typeName: 'claudeobs.v1.TokenTotals', input: 1, output: 0, cacheRead: 0, cacheCreation: 0 } }),
    row({ id: 'b', costUsd: 0, end: 100, tokens: { $typeName: 'claudeobs.v1.TokenTotals', input: 5, output: 0, cacheRead: 0, cacheCreation: 0 } }),
    row({ id: 'c', costUsd: 0, end: 200, tokens: { $typeName: 'claudeobs.v1.TokenTotals', input: 3, output: 0, cacheRead: 0, cacheCreation: 0 } }),
  ];

  it('sorts by recorded tokens without using modeled cost', () => {
    expect(sortSessions(rows, 'tokens').map((r) => r.id)).toEqual(['b', 'c', 'a']);
  });

  it('sorts most recent first', () => {
    expect(sortSessions(rows, 'recent').map((r) => r.id)).toEqual(['a', 'c', 'b']);
  });

  it('does not mutate the input array', () => {
    const copy = [...rows];
    sortSessions(rows, 'tokens');
    expect(rows).toEqual(copy);
  });
});

describe('filterSessions', () => {
  const rows = [
    row({ id: 'abc-123', project: 'Projects-app' }),
    row({ id: 'def-456', project: 'Other-thing' }),
  ];

  it('matches by session id substring', () => {
    expect(filterSessions(rows, 'abc').map((r) => r.id)).toEqual(['abc-123']);
  });

  it('matches by project substring, case-insensitively', () => {
    expect(filterSessions(rows, 'other').map((r) => r.id)).toEqual(['def-456']);
  });

  it('returns every row for a blank filter', () => {
    expect(filterSessions(rows, '   ')).toHaveLength(2);
  });

  it('matches by the displayed workspace name', () => {
    const withWorkspace = [
      row({ id: 'abc-123', project: '-Users-alice-Development-GitHub-acme', workspace: 'acme/api' }),
      row({ id: 'def-456', project: '-Users-alice-Development-GitHub-acme', workspace: 'acme/terraform' }),
    ];
    expect(filterSessions(withWorkspace, 'api').map((r) => r.id)).toEqual(['abc-123']);
  });

  it('still matches the underlying project key', () => {
    const withWorkspace = [row({ id: 'abc-123', project: '-Users-alice-Development-GitHub-acme', workspace: 'acme/api' })];
    expect(filterSessions(withWorkspace, 'development-github').map((r) => r.id)).toEqual(['abc-123']);
  });
});

describe('projectLabel', () => {
  it('prefers the derived workspace name', () => {
    expect(projectLabel(row({ project: '-Users-alice-Development-GitHub-acme', workspace: 'acme/api' })))
      .toBe('acme/api');
  });

  it('falls back to the shortened project key for a row indexed before the field existed', () => {
    expect(projectLabel(row({ project: '-Users-alice-Development-GitHub-acme', workspace: '' })))
      .toBe('Development-GitHub-acme');
  });
});

describe('deliveryText', () => {
  it('formats a recorded count', () => {
    expect(deliveryText(3)).toBe('3');
  });

  // Zero is indistinguishable from "no telemetry for this session", since
  // these counters have no backfill, so it must not render as "0".
  it('returns null for zero and for a missing value', () => {
    expect(deliveryText(0)).toBeNull();
    expect(deliveryText(undefined)).toBeNull();
  });

  it('rounds the floating point dust increase() leaves behind', () => {
    expect(deliveryText(2.9999999999999996)).toBe('3');
  });
});

describe('indexAgeText', () => {
  it('formats the age since the index was generated', () => {
    const now = 1_000_000;
    const generatedAt = new Date((now - 300) * 1000).toISOString();
    expect(indexAgeText(generatedAt, now)).toBe('5m');
  });

  it('returns null for an empty or unparseable timestamp', () => {
    expect(indexAgeText('', 1000)).toBeNull();
    expect(indexAgeText('not a date', 1000)).toBeNull();
  });
});

describe('profileLabel', () => {
  // A row indexed before the profile column existed genuinely does not know
  // which profile it came from. Null is what the UI renders as a dash, the
  // same rule the delivery counters follow: naming a profile would be
  // inventing one.
  it('is null for a row that predates the column', () => {
    expect(profileLabel(row({ profile: '' }))).toBeNull();
  });

  it('is the profile name otherwise', () => {
    expect(profileLabel(row({ profile: 'personal' }))).toBe('personal');
  });
});

describe('filterSessions by profile', () => {
  // The filter already narrows by project, so narrowing by profile through
  // the same box is one affordance rather than two controls.
  it('matches on the profile name', () => {
    const rows = [row({ id: 'a', profile: 'acme' }), row({ id: 'b', profile: 'personal' })];
    expect(filterSessions(rows, 'personal').map((r) => r.id)).toEqual(['b']);
  });

  it('leaves unattributed rows out of a profile search', () => {
    const rows = [row({ id: 'a', profile: '' }), row({ id: 'b', profile: 'acme' })];
    expect(filterSessions(rows, 'acme').map((r) => r.id)).toEqual(['b']);
  });
});
