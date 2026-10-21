import { create } from '@bufbuild/protobuf';
import type { ReactNode } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AccountSnapshotSchema, GetSessionResponseSchema, type AccountSnapshot } from '@/src/gen/claudeobs/v1/obs_pb';
import { usePoll } from '@/src/lib/usePoll';
import { SessionDetail } from './SessionDetail';

const { renderToStaticMarkup } = require('react-dom/server') as { renderToStaticMarkup(node: ReactNode): string };
const state = vi.hoisted(() => ({ query: '', accounts: [] as AccountSnapshot[] }));
vi.mock('next/navigation', () => ({ useSearchParams: () => new URLSearchParams(state.query) }));
vi.mock('@/src/components/AccountContext', () => ({ useAccounts: () => ({ data: { accounts: state.accounts }, error: null }) }));
vi.mock('@/src/lib/usePoll', () => ({ usePoll: vi.fn() }));
vi.mock('@/src/lib/useNow', () => ({ useNow: () => 2_000_000_000 }));
vi.mock('@/src/lib/client', () => ({ obsClient: {} }));

const id = '55a28b14-70f9-49d3-ad3f-df73e9ebd9e8';
const scopeError = 'This session does not belong to the selected scope';

beforeEach(() => {
  state.query = `provider=claude&accountKey=claude%3A5e6f7a8b&id=${id}`;
  state.accounts = [create(AccountSnapshotSchema, {
    key: 'claude:5e6f7a8b', provider: 'claude',
    profiles: [{ profile: 'stable-id', provider: 'claude', displayName: 'Original name' }],
  })];
});

function render(profile = 'stable-id', provider = 'claude') {
  vi.mocked(usePoll).mockReset().mockReturnValue({ data: null, error: null, fetchedAt: 1 });
  vi.mocked(usePoll).mockReturnValueOnce({
    data: create(GetSessionResponseSchema, { live: true, row: { id, provider, profile, live: true, workspace: 'Session project', usageKnown: true, toolsKnown: true } }),
    error: null, fetchedAt: 1,
  });
  return renderToStaticMarkup(<SessionDetail id={id} />);
}

describe('session detail scope identity', () => {
  it('keeps the same account and profile URLs working after a display-name change', () => {
    for (const profileFilter of ['', '&profile=stable-id']) {
      state.query += profileFilter;
      for (const name of ['Original name', 'Renamed profile']) {
        state.accounts[0].profiles[0].displayName = name;
        const html = render();
        expect(html).not.toContain(scopeError);
        expect(html).toContain('Session project');
        expect(html).toContain(name);
        expect(html).toContain('accountKey=claude%3A5e6f7a8b');
        expect(html).not.toContain('profile=Renamed');
      }
    }
  });

  it('rejects a different profile even when it has the same display name', () => {
    state.accounts.push(create(AccountSnapshotSchema, {
      key: 'claude:other', provider: 'claude',
      profiles: [{ profile: 'other-id', provider: 'claude', displayName: 'Original name' }],
    }));
    expect(render('other-id')).toContain(scopeError);
    expect(render('Original name')).toContain(scopeError);
  });

  it('retains account and provider checks', () => {
    expect(render('stable-id', 'codex')).toContain(scopeError);
    state.query = `provider=claude&accountKey=claude%3Aother&id=${id}`;
    expect(render()).toContain(scopeError);
  });

  it('does not infer ownership for an unattributed live session', () => {
    expect(render('')).toContain(scopeError);
  });
});
