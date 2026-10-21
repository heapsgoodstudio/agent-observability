import { describe, expect, it } from 'vitest';
import type { ChartSeries, ModelUsageRow, Point } from '../gen/claudeobs/v1/obs_pb';
import {
  attrEmptyText,
  bucketHeatmap,
  dailyChartData,
  hottestCells,
  modelUsageDerived,
  normalizeMeasure,
  normalizePeriod,
} from './spend';

function pt(ts: number, value: number): Point {
  return { $typeName: 'claudeobs.v1.Point', ts, value };
}

function series(label: string, points: Point[]): ChartSeries {
  return { $typeName: 'claudeobs.v1.ChartSeries', label, points };
}

describe('normalizePeriod', () => {
  it('accepts a known period', () => {
    expect(normalizePeriod('7d')).toBe('7d');
  });
  it('falls back to 30d for anything else', () => {
    expect(normalizePeriod('bogus')).toBe('30d');
    expect(normalizePeriod(null)).toBe('30d');
  });
});

describe('normalizeMeasure', () => {
  it('accepts billed', () => {
    expect(normalizeMeasure('billed')).toBe('billed');
  });
  it('falls back to notional for anything else', () => {
    expect(normalizeMeasure('bogus')).toBe('notional');
    expect(normalizeMeasure(null)).toBe('notional');
  });
});

describe('bucketHeatmap', () => {
  it('buckets points by local weekday and hour, shifting nothing further (the server already shifted -1h)', () => {
    // A fixed instant: Wednesday 14:00 local time (bucketHeatmap reads local
    // Date fields, which is what the "hour of day, local time" caption
    // promises).
    const d = new Date(2026, 7, 19, 14, 0, 0); // Wed 19 Aug 2026, 14:00 local
    const ts = Math.floor(d.getTime() / 1000);
    const { cells, max } = bucketHeatmap([pt(ts, 12.5)]);
    expect(cells).toHaveLength(7);
    expect(cells[0]).toHaveLength(24);
    expect(cells[2][14]).toBe(12.5); // Monday-first: Wed is row 2
    expect(max).toBe(12.5);
  });

  it('sums multiple points landing in the same cell', () => {
    const d = new Date(2026, 7, 17, 9, 0, 0); // Monday
    const ts = Math.floor(d.getTime() / 1000);
    const { cells, max } = bucketHeatmap([pt(ts, 1), pt(ts, 2)]);
    expect(cells[0][9]).toBe(3);
    expect(max).toBe(3);
  });

  it('ignores non-finite or non-positive values', () => {
    const d = new Date(2026, 7, 17, 9, 0, 0);
    const ts = Math.floor(d.getTime() / 1000);
    const { max } = bucketHeatmap([pt(ts, 0), pt(ts, -1), pt(ts, NaN)]);
    expect(max).toBe(0);
  });

  it('returns an all-zero grid for no points', () => {
    const { cells, max } = bucketHeatmap([]);
    expect(max).toBe(0);
    expect(cells.every((row) => row.every((v) => v === 0))).toBe(true);
  });
});

describe('hottestCells', () => {
  it('names the top three cells descending by value', () => {
    const cells = Array.from({ length: 7 }, () => new Array(24).fill(0));
    cells[0][9] = 5;
    cells[2][14] = 12.5;
    cells[5][20] = 8;
    const text = hottestCells(cells, (v) => `$${v}`);
    expect(text).toBe('Wed 14:00 $12.5, Sat 20:00 $8, Mon 09:00 $5');
  });

  it('returns an empty string when every cell is zero', () => {
    const cells = Array.from({ length: 7 }, () => new Array(24).fill(0));
    expect(hottestCells(cells, (v) => `$${v}`)).toBe('');
  });
});

describe('dailyChartData', () => {
  it('pivots per-model series into per-bucket maps, colors by position, and finds the peak', () => {
    const daily = [
      series('sonnet-5', [pt(100, 1), pt(200, 4)]),
      series('other', [pt(100, 2), pt(200, 1)]),
    ];
    const { buckets, keys, colorOf, peakIndex } = dailyChartData(daily);
    expect(keys).toEqual(['sonnet-5', 'other']);
    expect(buckets).toEqual([
      { x: 100, values: new Map([['sonnet-5', 1], ['other', 2]]) },
      { x: 200, values: new Map([['sonnet-5', 4], ['other', 1]]) },
    ]);
    expect(colorOf.get('sonnet-5')).toBe('var(--chart-1)');
    expect(colorOf.get('other')).toBe('var(--chart-other)');
    expect(peakIndex).toBe(1); // bucket 200: 4 + 1 = 5, versus bucket 100: 1 + 2 = 3
  });

  it('returns an empty shape for no series', () => {
    const { buckets, keys } = dailyChartData([]);
    expect(buckets).toEqual([]);
    expect(keys).toEqual([]);
  });
});

describe('attrEmptyText', () => {
  it('reads the period-specific phrase for notional', () => {
    expect(attrEmptyText('notional', '7d')).toBe('no cost recorded in the last 7 days');
    expect(attrEmptyText('notional', 'mtd')).toBe('no cost recorded this month');
  });

  it('reads the fixed billed sentence regardless of period', () => {
    expect(attrEmptyText('billed', '7d')).toBe('nothing was billed at API rates in this window');
    expect(attrEmptyText('billed', 'all')).toBe('nothing was billed at API rates in this window');
  });
});

describe('modelUsageDerived', () => {
  const row: ModelUsageRow = {
    $typeName: 'claudeobs.v1.ModelUsageRow',
    model: 'sonnet-5', costUsd: 10, output: 1_000_000, cacheRead: 4_000_000, cacheCreation: 1_000_000, input: 4_000_000,
  };

  it('computes share against the grand total', () => {
    expect(modelUsageDerived(row, 20).share).toBe('50.0%');
  });

  it('reports the per-million-output rate when output is positive', () => {
    expect(modelUsageDerived(row, 20).perOutput).toBe('$10.00');
  });

  it('says "no output" instead of dividing by zero', () => {
    const noOutput = { ...row, output: 0 };
    expect(modelUsageDerived(noOutput, 20).perOutput).toBe('no output');
  });

  it('computes cache-read share of all tokens', () => {
    expect(modelUsageDerived(row, 20).cached).toBe('40.0%');
  });

  it('computes write:read and says n/a when there are no reads', () => {
    expect(modelUsageDerived(row, 20).writeRead).toBe('25.0%');
    const noRead = { ...row, cacheRead: 0 };
    expect(modelUsageDerived(noRead, 20).writeRead).toBe('n/a');
  });
});
