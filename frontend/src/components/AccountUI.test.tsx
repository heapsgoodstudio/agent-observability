import { create } from '@bufbuild/protobuf';
import type { ReactNode } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { AccountSnapshotSchema, AllowanceWindowSchema } from '@/src/gen/claudeobs/v1/obs_pb';
import { Freshness, WindowMeter } from './AccountUI';

const { renderToStaticMarkup } = require('react-dom/server') as { renderToStaticMarkup(node: ReactNode): string };
vi.mock('next/navigation', () => ({ usePathname: () => '/' }));
vi.mock('@/src/lib/useNow', () => ({ useNow: () => 2_000_000_000 }));
const now = 2_000_000_000;

describe('account freshness labels', () => {
  it('explains a one-minute current reading beside a eleven-minute model reading', () => {
    const account = create(AccountSnapshotSchema, { provider: 'claude', state: 'stale', confirmedAt: now - 60, windows: [
      { label: 'Current allowance', state: 'fresh', source: 'statusline', confirmedAt: now - 60 },
      { label: 'Weekly allowance', state: 'fresh', source: 'statusline', confirmedAt: now - 60 },
      { label: 'Model allowance', state: 'stale', source: 'cache', confirmedAt: now - 660 },
    ] });
    const html = renderToStaticMarkup(<Freshness account={account} />);
    expect(html).toContain('1 of 3 windows stale · last confirmed 11m 0s ago');
    expect(html).toContain('Model allowance: Stale reading');
    expect(html).toContain('confirmations are fresh for 10 minutes');
    const meter = renderToStaticMarkup(<WindowMeter window={account.windows[2]} />);
    expect(meter).toContain('Stale reading · Last confirmed 11m 0s ago');
  });
  it('labels a cache value change without pretending it was confirmed', () => {
    const window = create(AllowanceWindowSchema, { state: 'stale', source: 'cache', observedAt: now - 60, usedPercent: 0 });
    const html = renderToStaticMarkup(<WindowMeter window={window} detailed />);
    expect(html).toContain('Confirmation unavailable · Last value change');
    expect(html).not.toContain('Last confirmed');
    expect(html).toContain('data-state="unknown"');
  });
  it('shows why a 301-second Codex reading is stale', () => {
    const account = create(AccountSnapshotSchema, { provider: 'codex', state: 'fresh', windows: [{ source: 'codex', state: 'fresh', confirmedAt: now - 301 }] });
    const html = renderToStaticMarkup(<Freshness account={account} />);
    expect(html).toContain('Stale · last confirmed 5m 1s ago');
    expect(html).toContain('confirmations are fresh for 5 minutes');
  });
});
