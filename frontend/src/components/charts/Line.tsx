import { useId } from 'react';
import { niceCeil, ticks } from '../../lib/scale';
import type { ChartTone } from './Spark';

export interface LineProps {
  points: Array<[number, number]>;
  bands?: Array<[number, number]>;
  xMin: number;
  xMax: number;
  fmtY: (v: number) => string;
  fmtX: (v: number) => string;
  label: string;
  tone?: ChartTone;
  // Optional, but never defaulted to a shared literal: a fixed id is a
  // hazard (a second line chart on the same page silently steals
  // the first one's gradient unless every caller remembers a distinct id).
  // Here it defaults to React's useId(), unique per mounted instance, so
  // the footgun is closed for any caller that doesn't need to override it;
  // an explicit gradientId is still honoured when passed.
  gradientId?: string;
}

const W = 760, H = 240;
const PAD = { l: 58, r: 12, t: 12, b: 28 };

const TONE_CLASS: Record<ChartTone, string> = {
  accent: 'text-chart-1',
  hot: 'text-chart-real',
  neutral: 'text-chart-other',
};

// Line with a gradient wash beneath it. Bands shade stretches where a plan
// window was exhausted; interior dots appear only for a short series (<=60 points),
// which would otherwise read as a near-invisible line at one edge; the
// final point carries a native tooltip with the latest reading: a browser
// `<title>`, not a custom crosshair layer.
export function Line({ points, bands = [], xMin, xMax, fmtY, fmtX, label, tone = 'accent', gradientId }: LineProps) {
  const autoId = useId();
  const gid = gradientId ?? `line-wash-${autoId}`;

  const plotW = W - PAD.l - PAD.r;
  const plotH = H - PAD.t - PAD.b;
  const span = Math.max(xMax - xMin, 1);
  const yMax = niceCeil(Math.max(...points.map(([, v]) => v), 0) * 1.08);
  const x = (t: number) => PAD.l + ((t - xMin) / span) * plotW;
  const y = (v: number) => PAD.t + plotH - (v / yMax) * plotH;

  const line = points.map(([t, v]) => `${x(t).toFixed(2)},${y(v).toFixed(2)}`).join(' ');
  const lastPoint = points[points.length - 1];
  const lastX = x(lastPoint[0]);
  const firstX = x(points[0][0]);

  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      role="img"
      aria-label={label}
      className={`block h-auto w-full ${TONE_CLASS[tone]}`}
    >
      <defs>
        <linearGradient id={gid} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="currentColor" stopOpacity={0.2} />
          <stop offset="100%" stopColor="currentColor" stopOpacity={0} />
        </linearGradient>
      </defs>

      {bands.map(([b0, b1], i) => {
        const x0 = x(b0);
        // Index as key: bands are plain [start, end] pairs with no id of
        // their own.
        return (
          <rect
            key={i}
            x={x0}
            y={PAD.t}
            width={Math.max(x(b1) - x0, 1.5)}
            height={plotH}
            className="fill-warn"
            opacity={0.1}
          />
        );
      })}

      {ticks(yMax).map((v) => (
        <g key={v}>
          <line x1={PAD.l} x2={W - PAD.r} y1={y(v)} y2={y(v)} className="stroke-chart-grid" strokeWidth={1} />
          <text x={PAD.l - 8} y={y(v) + 3.5} textAnchor="end" className="fill-faint text-[10.5px]">
            {fmtY(v)}
          </text>
        </g>
      ))}

      {Array.from({ length: 5 }, (_, i) => {
        const t = xMin + (span * i) / 4;
        const anchor = i === 0 ? 'start' : i === 4 ? 'end' : 'middle';
        return (
          <text key={i} x={x(t)} y={H - 8} textAnchor={anchor} className="fill-faint text-[10.5px]">
            {fmtX(t)}
          </text>
        );
      })}

      <polygon
        fill={`url(#${gid})`}
        points={`${line} ${lastX.toFixed(2)},${y(0).toFixed(2)} ${firstX.toFixed(2)},${y(0).toFixed(2)}`}
      />
      <polyline points={line} fill="none" stroke="currentColor" strokeWidth={1.5} strokeLinecap="round" strokeLinejoin="round" />
      {points.length <= 60
        ? points.map(([t, v]) => (
            <circle
              key={t}
              cx={x(t)}
              cy={y(v)}
              r={2.6}
              fill="currentColor"
              className="stroke-panel"
              strokeWidth={1.2}
            />
          ))
        : null}
      <circle cx={x(lastPoint[0])} cy={y(lastPoint[1])} r={4} fill="currentColor" className="stroke-panel" strokeWidth={2}>
        <title>{`latest: ${fmtY(lastPoint[1])}`}</title>
      </circle>
    </svg>
  );
}
