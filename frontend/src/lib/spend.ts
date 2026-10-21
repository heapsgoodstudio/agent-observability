// Pure presentation helpers for the Spend page (components/pages/Spend.tsx).
// The query-building half of this page (periodBounds, billedTotals,
// notionalTotals, the VictoriaMetrics queries) lives server-side in
// internal/server/spend.go, which
// returns already-shaped rows (ChartSeries, Attribution, ModelUsageRow,
// DeliveryRow). What is left here is the part that was ALWAYS a browser-side
// concern and stays one: turning those rows into chart geometry inputs, and
// the small text tables (period labels, "no data" phrasing) that were plain
// UI strings with no server field to read them from. No DOM, no fetching, no
// module state.
import type { ChartSeries, ModelUsageRow, Point } from '../gen/claudeobs/v1/obs_pb';
import { compactInt, money } from './format';

export type SpendPeriod = '24h' | '7d' | '30d' | 'mtd' | 'all';
export type SpendMeasure = 'notional' | 'billed';

const VALID_PERIODS: ReadonlySet<string> = new Set(['24h', '7d', '30d', 'mtd', 'all']);

// Mirrors internal/server/spend.go's normalizePeriod: an unrecognized or
// absent period reads as "30d". The two must agree, or the page asks for one
// window and labels it as another.
export function normalizePeriod(period: string | null | undefined): SpendPeriod {
  return period && VALID_PERIODS.has(period) ? (period as SpendPeriod) : '30d';
}

// Mirrors internal/server/spend.go's normalizeMeasure: an unrecognized or
// absent measure reads as "notional".
export function normalizeMeasure(measure: string | null | undefined): SpendMeasure {
  return measure === 'billed' ? 'billed' : 'notional';
}

// The period <select>'s option labels.
export const PERIOD_SELECT_OPTIONS: ReadonlyArray<{ value: SpendPeriod; label: string }> = [
  { value: '24h', label: 'Last 24 hours' },
  { value: '7d', label: 'Last 7 days' },
  { value: '30d', label: 'Last 30 days' },
  { value: 'mtd', label: 'Month to date' },
  { value: 'all', label: 'All history' },
];

// Used in every panel title ("Daily cost by model, 7 days") and in the
// model-usage note's tail.
export const PERIOD_SHORT: Record<SpendPeriod, string> = {
  '24h': '24 hours', '7d': '7 days', '30d': '30 days', mtd: 'month to date', all: 'all history',
};

// "no cost recorded <phrase>" reads correctly only with this map, not
// PERIOD_SHORT (mtd/all read as broken sentences after "in the last").
export const PERIOD_PHRASE: Record<SpendPeriod, string> = {
  '24h': 'in the last 24 hours', '7d': 'in the last 7 days', '30d': 'in the last 30 days',
  mtd: 'this month', all: 'at all',
};

// The breakdown panels share two "no data" sentences and choose between them
// by measure: Notional's (period-specific, via PERIOD_PHRASE) or Billed's
// fixed
// "nothing was billed..." (an estimate against the overage bands has no
// period-shaped phrasing of its own, since it is always masked to "this
// window" rather than the period's stated span). The seven attribution
// panels (model, query_source, agent.name, skill.name, effort,
// mcp_server.name, project) are this function's only callers; the daily
// chart, heatmap and model-usage table stay notional-only regardless of the
// toggle and keep their own unconditional "no cost recorded" text.
const NOTHING_BILLED_TEXT = 'nothing was billed at API rates in this window';

export function attrEmptyText(measure: SpendMeasure, period: SpendPeriod): string {
  return measure === 'billed' ? NOTHING_BILLED_TEXT : `no cost recorded ${PERIOD_PHRASE[period]}`;
}

// The categorical series colors, as design tokens (globals.css's
// --chart-1..4, plus --chart-other for the tail bucket).
const CAT = ['var(--chart-1)', 'var(--chart-2)', 'var(--chart-3)', 'var(--chart-4)'];

export interface DailyBucket {
  x: number;
  values: Map<string, number>;
}

export interface DailyChartData {
  buckets: DailyBucket[];
  keys: string[];
  colorOf: Map<string, string>;
  peakIndex: number | null;
  // The peak bucket's own total, alongside its index, for "Peak <date> at
  // <money>". Saved here rather than asking every caller to re-sum the peak
  // bucket's values a second time.
  peakTotal: number | null;
}

// internal/server/spend.go's buildDailyCost already ran topNWithOther
// server-side: resp.dailyCost is at most 4 model series plus one "other"
// series, in that fixed order. What is left is browser work: pivot the
// per-series arrays (one per model, sharing one x-axis) into per-bucket
// maps (one per x, keyed by model) the Columns chart wants, assign each key
// a color positionally ("other" always gets the achromatic token, never a
// numbered one, matching topNWithOther's own colorOf), and find the peak
// bucket (a client-side reduction; the server returns no peak field).
export function dailyChartData(daily: ChartSeries[]): DailyChartData {
  if (daily.length === 0) return { buckets: [], keys: [], colorOf: new Map(), peakIndex: null, peakTotal: null };

  const keys = daily.map((s) => s.label);
  const colorOf = new Map<string, string>();
  let catIndex = 0;
  for (const k of keys) colorOf.set(k, k === 'other' ? 'var(--chart-other)' : CAT[catIndex++]);

  const xs = daily[0].points.map((p) => p.ts);
  const buckets: DailyBucket[] = xs.map((x, i) => {
    const values = new Map<string, number>();
    for (const s of daily) values.set(s.label, s.points[i]?.value ?? 0);
    return { x, values };
  });

  let peakIndex: number | null = null;
  let peakTotal: number | null = null;
  buckets.forEach((b, i) => {
    const total = [...b.values.values()].reduce((a, v) => a + v, 0);
    if (peakTotal === null || total > peakTotal) {
      peakTotal = total;
      peakIndex = i;
    }
  });

  return { buckets, keys, colorOf, peakIndex, peakTotal };
}

const WEEKDAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

export interface HeatmapBuckets {
  cells: number[][];
  max: number;
}

// Heatmap bucketing: local weekday and hour of day (Date's own
// getDay()/getHours(), browser-local time; the heatmap's own "hour of day,
// local time" caption depends on this). The server has
// already applied the sum_over_time bucket shift (heatmapPointsToProto's
// `ts - 3600`, since a sample at t covers the hour ENDING at t); this
// function only buckets the already-shifted timestamps by weekday/hour.
export function bucketHeatmap(points: Point[]): HeatmapBuckets {
  const cells: number[][] = Array.from({ length: 7 }, () => new Array(24).fill(0));
  let max = 0;
  for (const p of points) {
    const v = p.value;
    if (!Number.isFinite(v) || v <= 0) continue;
    const d = new Date(p.ts * 1000);
    const row = (d.getDay() + 6) % 7; // Monday-first
    const col = d.getHours();
    cells[row][col] += v;
    max = Math.max(max, cells[row][col]);
  }
  return { cells, max };
}

// The "hottest cells" caption: the top three non-zero cells, descending by
// value.
export function hottestCells(cells: number[][], fmtV: (v: number) => string): string {
  const flat: Array<[number, number, number]> = [];
  cells.forEach((row, r) => row.forEach((v, h) => { if (v > 0) flat.push([r, h, v]); }));
  flat.sort((a, b) => b[2] - a[2]);
  return flat
    .slice(0, 3)
    .map(([r, h, v]) => `${WEEKDAYS[r]} ${String(h).padStart(2, '0')}:00 ${fmtV(v)}`)
    .join(', ');
}

export interface ModelUsageDerived {
  cost: string;
  share: string;
  output: string;
  cacheRead: string;
  cacheWrite: string;
  input: string;
  perOutput: string;
  cached: string;
  writeRead: string;
}

const pct = (n: number, d: number): string => (d > 0 ? `${((100 * n) / d).toFixed(1)}%` : 'n/a');

// Model-usage row building. internal/server/spend.go deliberately returns
// only the raw cost/token figures on ModelUsageRow, so every derived column
// (share of the grand total, cache-read/write ratios, cost per million
// output tokens) is computed here, the browser work Go's own comment
// assigns to this layer.
export function modelUsageDerived(row: ModelUsageRow, grand: number): ModelUsageDerived {
  const total = row.output + row.cacheRead + row.input + row.cacheCreation;
  return {
    cost: money(row.costUsd),
    share: pct(row.costUsd, grand),
    output: compactInt(row.output),
    cacheRead: compactInt(row.cacheRead),
    cacheWrite: compactInt(row.cacheCreation),
    input: compactInt(row.input),
    perOutput: row.output > 0 ? money(row.costUsd / (row.output / 1e6)) : 'no output',
    cached: pct(row.cacheRead, total),
    writeRead: row.cacheRead > 0 ? pct(row.cacheCreation, row.cacheRead) : 'n/a',
  };
}
