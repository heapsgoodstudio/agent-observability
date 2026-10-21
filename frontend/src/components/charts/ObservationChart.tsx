'use client';
import { useId, useLayoutEffect, useRef, useState } from 'react';
import { historySegments } from '@/src/lib/dashboard';
import type { Point, Band } from '@/src/gen/claudeobs/v1/obs_pb';

type Series = { label: string; points: Point[]; gaps?: Band[] };
// bars draws one column per point, each spanning bucketSeconds, for figures
// that are per-period totals (a day's estimated spend) rather than readings
// of a running value. Everything else, the pixel geometry, axes, type and the
// recorded-values table, is shared so the two kinds of chart match.
// format, when given, labels bar values (ticks, tooltips, the table) for a
// figure that is not dollars, such as daily tokens.
type Bars = { bucketSeconds: number; valueHeading: string; format?: (v: number) => string };
export function ObservationChart({ series, from, to, label, percent = false, resetAt, bars }: { series: Series[]; from?: number; to?: number; label: string; percent?: boolean; resetAt?: number; bars?: Bars }) {
  const id = useId();
  const container = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(710);
  useLayoutEffect(() => {
    const element = container.current;
    if (!element) return;
    const measure = () => setWidth(element.getBoundingClientRect().width);
    measure();
    const observer = new ResizeObserver(([entry]) => setWidth(entry.contentRect.width));
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  const points = series.flatMap((s) => s.points).filter((p) => Number.isFinite(p.ts) && Number.isFinite(p.value));
  if (!points.length) return <div ref={container} className="min-w-0"><div className="obs-empty"><h3>No retained observations</h3><p>History appears as the collector records this account. Missing intervals remain unknown.</p></div></div>;
  const bounds = points.reduce((b, p) => ({ min: Math.min(b.min, p.ts), max: Math.max(b.max, p.ts), value: Math.max(b.value, p.value) }), { min: Infinity, max: -Infinity, value: 0 });
  const x0 = from ?? bounds.min;
  const x1 = Math.max(to ?? bounds.max, bounds.max, x0 + 1);
  const ymax = percent ? Math.max(100, bounds.value) : Math.max(1, bounds.value) * 1.12;
  const ticks = [0, .5, 1].map((fraction) => ({ value: fraction * ymax, label: bars?.format ? bars.format(fraction * ymax) : percent ? `${Math.round(fraction * ymax)}%` : `$${(fraction * ymax).toFixed(fraction * ymax < 10 ? 2 : 0)}` }));
  const left = Math.max(52, ...ticks.map((tick) => tick.label.length * 7 + 12));
  const plotWidth = Math.max(1, width - left - 16);
  const x = (t: number) => left + (t - x0) / (x1 - x0) * plotWidth;
  const y = (v: number) => 160 - v / ymax * 140;
  const fmt = (v: number) => bars?.format ? bars.format(v) : percent ? `${Number(v.toFixed(1))}%` : `$${v.toFixed(2)}`;
  const date = (t: number) => bars ? new Date(t * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) : new Date(t * 1000).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  const noun = bars ? 'days' : 'observations';
  const barWidth = bars ? Math.max(2, bars.bucketSeconds / (x1 - x0) * plotWidth * 0.62) : 0;
  const segments = series.flatMap((s) => historySegments(s.points, s.gaps).map((p) => { const stride = Math.max(1, Math.ceil(p.length / 600)); return { points: p.filter((_, i) => i % stride === 0 || i === p.length - 1), label: s.label }; }));
  const tableRows = series.flatMap((s) => s.points.map((p) => ({ ...p, label: s.label }))).toSorted((a, b) => b.ts - a.ts).slice(0, 500);
  return <div ref={container} className="min-w-0">
    {/* Native SVG pixels keep type, strokes, and height constant at every card width. */}
    <svg className="obs-chart" data-tone={bars ? 'estimate' : percent ? 'allowance' : 'billing'} width="100%" height="194" role="img" aria-labelledby={id}>
      <title id={id}>{bars ? `${label}. ${points.length} ${noun}.` : `${label}. ${points.length} observations. Gaps and separate reporting periods are disconnected.`}</title>
      <defs><clipPath id={`${id}-plot`}><rect x={left - 3} y="17" width={plotWidth + 6} height="146" /></clipPath></defs>
      {ticks.map((tick) => <g key={tick.value}><line x1={left} x2={left + plotWidth} y1={y(tick.value)} y2={y(tick.value)} /><text x={left - 8} y={y(tick.value) + 4} textAnchor="end">{tick.label}</text></g>)}
      {resetAt !== undefined && resetAt >= x0 && resetAt <= x1 && <g><line x1={x(resetAt)} x2={x(resetAt)} y1="17" y2="160" strokeDasharray="3 4" /><text x={x(resetAt) + (resetAt < (x0 + x1) / 2 ? 5 : -5)} y="13" textAnchor={resetAt < (x0 + x1) / 2 ? 'start' : 'end'}>Reported reset</text></g>}
      {bars ? <g clipPath={`url(#${id}-plot)`}>{points.filter((p) => p.value > 0).map((p, i) => <rect key={i} x={x(p.ts) - barWidth / 2} y={y(p.value)} width={barWidth} height={Math.max(1, y(0) - y(p.value))} rx="2" fill="currentColor"><title>{`${fmt(p.value)} · ${date(p.ts)}`}</title></rect>)}</g> : <g clipPath={`url(#${id}-plot)`}>{segments.map((segment, i) => <g key={i}><polyline points={segment.points.map((p) => `${x(p.ts)},${y(p.value)}`).join(' ')} fill="none" stroke="currentColor" strokeWidth="2" strokeLinejoin="round" />{segment.points.filter((_, j) => segment.points.length < 80 || j === segment.points.length - 1).map((point, j) => <circle key={j} cx={x(point.ts)} cy={y(point.value)} r="2.5" fill="currentColor"><title>{`${segment.label}: ${fmt(point.value)} · ${date(point.ts)}`}</title></circle>)}</g>)}</g>}
      {[0, .5, 1].map((f) => <text key={f} x={x(x0 + f * (x1 - x0))} y="187" textAnchor={f === 0 ? 'start' : f === 1 ? 'end' : 'middle'}>{new Date((x0 + f * (x1 - x0)) * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })}</text>)}
    </svg>
    <details className="obs-chart-summary"><summary className="cursor-pointer">{points.length} {noun} · View recorded values{points.length > 500 ? ' (latest 500)' : ''}</summary><div className="max-h-60 overflow-auto mt-3"><table className="obs-data-table"><caption className="sr-only">{label}</caption><thead><tr><th>Observed</th><th>Series</th><th>{bars ? bars.valueHeading : percent ? 'Used' : 'Reported overage (USD)'}</th></tr></thead><tbody>{tableRows.map((p, i) => <tr key={i}><td>{date(p.ts)}</td><td>{p.label}</td><td>{fmt(p.value)}</td></tr>)}</tbody></table></div></details>
  </div>;
}
