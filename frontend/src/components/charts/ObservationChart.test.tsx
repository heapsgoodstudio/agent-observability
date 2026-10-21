import { create } from '@bufbuild/protobuf';
import type { ReactNode } from 'react';
import { describe, expect, it } from 'vitest';
import { PointSchema } from '@/src/gen/claudeobs/v1/obs_pb';
import { ObservationChart } from './ObservationChart';

const { renderToStaticMarkup } = require('react-dom/server') as { renderToStaticMarkup(node: ReactNode): string };

describe('observation chart sizing and missing data', () => {
  it('uses native pixels and a fixed height so a wider card cannot magnify SVG labels', () => {
    const html = renderToStaticMarkup(<ObservationChart label="Allowance" percent series={[{ label: 'Weekly', points: [create(PointSchema, { ts: 100, value: 0 }), create(PointSchema, { ts: 200, value: 50 })] }]} />);
    const svg = html.match(/<svg\b[^>]*>/)?.[0];
    expect(svg).toContain('width="100%"');
    expect(svg).toContain('height="194"');
    expect(svg).not.toContain('viewBox');
    expect(html).toContain('Weekly: 0%');
    expect(html).toContain('Weekly: 50%');
    expect(html).not.toContain('$');
  });

  it('retains unknown history instead of drawing a zero baseline for an empty series', () => {
    const html = renderToStaticMarkup(<ObservationChart label="Allowance" percent series={[]} />);
    expect(html).toContain('No retained observations');
    expect(html).not.toContain('<svg');
    expect(html).not.toContain('0%');
  });

  it('draws per-period bars with the same native-pixel geometry and table', () => {
    const day = 86400;
    const points = [0, 1, 2].map((i) => create(PointSchema, { ts: 1_000_000 + i * day, value: i === 1 ? 7.37 : 0 }));
    const html = renderToStaticMarkup(<ObservationChart label="Daily estimated spend" series={[{ label: 'Estimated spend', points }]} bars={{ bucketSeconds: day, valueHeading: 'Estimated spend (USD)' }} />);
    const svg = html.match(/<svg\b[^>]*>/)?.[0];
    expect(svg).toContain('height="194"');
    expect(svg).not.toContain('viewBox');
    expect(svg).toContain('data-tone="estimate"');
    expect((html.match(/<rect /g) ?? []).length).toBe(2); // the clip rect and one non-zero day
    expect(html).not.toContain('<polyline');
    expect(html).toContain('3 days');
    expect(html).toContain('Estimated spend (USD)');
  });
});
