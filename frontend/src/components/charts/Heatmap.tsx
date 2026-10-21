import { Fragment } from 'react';

export interface HeatmapProps {
  // 7x24, cells[weekday][hour], Monday first.
  cells: number[][];
  // The largest cell, used to scale the opacity ramp.
  max: number;
  fmtV: (v: number) => string;
  label: string;
}

const WEEKDAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];
const W = 720, H = 208;
const PAD = { l: 34, r: 6, t: 16, b: 20 };
// Surface gap between cells.
const GAP = 2;
// Opacity ramp floor: a zero-adjacent low cell must still read as a mark,
// not vanish into the empty-cell fill.
const FLOOR = 0.1;
const LEGEND_STEPS = [0, 0.05, 0.25, 0.5, 0.75, 1];

function opacityFor(v: number, max: number): number {
  return FLOOR + (1 - FLOOR) * Math.sqrt(v / max);
}

// Weekday-by-hour heatmap: a sequential single-hue scale (one hue,
// opacity-ramped by value, never a
// rainbow, per the dataviz skill's sequential rule), with a relief legend
// beneath it for the color-only value scale, since color alone is never
// the only channel a magnitude reads through here.
export function Heatmap({ cells, max, fmtV, label }: HeatmapProps) {
  const cw = (W - PAD.l - PAD.r) / 24;
  const ch = (H - PAD.t - PAD.b) / 7;

  return (
    <Fragment>
      <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={label} className="block h-auto w-full">
        {Array.from({ length: 8 }, (_, i) => i * 3).map((h) => (
          <text
            key={h}
            x={PAD.l + h * cw + cw / 2}
            y={PAD.t - 5}
            textAnchor="middle"
            className="fill-faint text-[10.5px]"
          >
            {String(h).padStart(2, '0')}
          </text>
        ))}
        {cells.map((row, r) => (
          <g key={WEEKDAYS[r]}>
            <text
              x={PAD.l - 5}
              y={PAD.t + r * ch + ch / 2 + 3}
              textAnchor="end"
              className="fill-faint text-[10.5px]"
            >
              {WEEKDAYS[r]}
            </text>
            {row.map((v, h) => (
              <rect
                key={h}
                x={PAD.l + h * cw + GAP / 2}
                y={PAD.t + r * ch + GAP / 2}
                width={Math.max(cw - GAP, 0.5)}
                height={Math.max(ch - GAP, 0.5)}
                rx={1.5}
                className={v > 0 ? 'fill-chart-1' : 'fill-selected'}
                fillOpacity={v > 0 ? opacityFor(v, max).toFixed(3) : 1}
              >
                <title>{`${WEEKDAYS[r]} ${String(h).padStart(2, '0')}:00, ${fmtV(v)}`}</title>
              </rect>
            ))}
          </g>
        ))}
        <text x={PAD.l} y={H - 6} className="fill-faint text-[10.5px]">
          hour of day, local time
        </text>
      </svg>
      <div className="mt-3 flex items-center gap-2 text-[12.5px] text-secondary">
        <span>{fmtV(0)}</span>
        <span className="inline-flex gap-0.5">
          {LEGEND_STEPS.map((f) => (
            <span
              key={f}
              className={`h-2.5 w-4 flex-none rounded-xs ${f > 0 ? 'bg-chart-1' : 'bg-selected'}`}
              style={f > 0 ? { opacity: opacityFor(f * max, max).toFixed(3) } : undefined}
            />
          ))}
        </span>
        <span>{fmtV(max)}</span>
      </div>
    </Fragment>
  );
}
