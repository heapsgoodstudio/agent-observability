import { niceCeil } from '../../lib/scale';
import { topRoundedRect } from './geometry';

export interface ColumnsBucket {
  x: number;
  values: Map<string, number>;
}

export interface ColumnsProps {
  buckets: ColumnsBucket[];
  // The ordered stack, top segment first. colorOf maps each key to a CSS
  // color (a `var(--chart-N)` categorical token, see topNWithOther's future
  // React port in the page task that builds it).
  keys: string[];
  colorOf: Map<string, string>;
  fmtY: (v: number) => string;
  fmtX: (v: number) => string;
  label: string;
  // Index into buckets to direct-label with its total, or null/undefined
  // for none.
  peak?: number | null;
}

const W = 720, H = 240;
const PAD = { l: 54, r: 10, t: 10, b: 30 };
// ~2px on screen at the rendered width: the surface gap between touching
// segments, per the dataviz skill's mark spec (never a stroke around the
// segment).
const SEG_GAP = 1.25;

// Stacked columns: one band per bucket, one segment per key.
export function Columns({ buckets, keys, colorOf, fmtY, fmtX, label, peak }: ColumnsProps) {
  const plotW = W - PAD.l - PAD.r;
  const plotH = H - PAD.t - PAD.b;
  const totals = buckets.map((b) => keys.reduce((a, k) => a + (b.values.get(k) ?? 0), 0));
  const yMax = niceCeil(Math.max(...totals, 0) * 1.05);
  const bandW = plotW / Math.max(buckets.length, 1);
  const barW = Math.max(bandW * 0.62, 1);
  const y = (v: number) => PAD.t + plotH - (v / yMax) * plotH;

  // Roughly the original's "every 5th of 35" spacing, generalised to any
  // bucket count so a 24-hour or 730-day window does not crowd its axis.
  const tickEvery = Math.max(1, Math.ceil(buckets.length / 7));

  const hasPeak = peak !== null && peak !== undefined && buckets[peak] !== undefined && totals[peak] > 0;

  return (
    <svg viewBox={`0 0 ${W} ${H}`} role="img" aria-label={label} className="block h-auto w-full">
      {[0, yMax / 2, yMax].map((v) => (
        <g key={v}>
          <line x1={PAD.l} x2={W - PAD.r} y1={y(v)} y2={y(v)} className="stroke-chart-grid" strokeWidth={1} />
          <text x={PAD.l - 8} y={y(v) + 3.5} textAnchor="end" className="fill-faint text-[10.5px]">
            {fmtY(v)}
          </text>
        </g>
      ))}

      {buckets.map((b, i) => {
        const x0 = PAD.l + i * bandW + (bandW - barW) / 2;
        const segs = keys.map((k): [string, number] => [k, b.values.get(k) ?? 0]).filter(([, v]) => v > 0);
        let acc = 0;
        const segEls = segs.map(([key, v], si) => {
          const yTop = y(acc + v);
          const yBot = y(acc) - (si > 0 ? SEG_GAP : 0);
          const h = Math.max(yBot - yTop, 0.6);
          const isTop = si === segs.length - 1;
          acc += v;
          const fill = colorOf.get(key);
          const title = `${fmtX(b.x)}: ${key} ${fmtY(v)}`;
          return isTop ? (
            <path key={key} d={topRoundedRect(x0, yTop, barW, h, 2)} style={{ fill }}>
              <title>{title}</title>
            </path>
          ) : (
            <rect key={key} x={x0} y={yTop} width={barW} height={h} style={{ fill }}>
              <title>{title}</title>
            </rect>
          );
        });
        const showTick = i % tickEvery === 0 || i === buckets.length - 1;
        return (
          <g key={b.x}>
            {segEls}
            {showTick ? (
              <text x={x0 + barW / 2} y={H - 10} textAnchor="middle" className="fill-faint text-[10.5px]">
                {fmtX(b.x)}
              </text>
            ) : null}
          </g>
        );
      })}

      {hasPeak ? (
        <text
          x={PAD.l + (peak as number) * bandW + bandW / 2}
          y={y(totals[peak as number]) - 5}
          textAnchor="middle"
          className="fill-faint text-[10.5px]"
        >
          {fmtY(totals[peak as number])}
        </text>
      ) : null}
    </svg>
  );
}
