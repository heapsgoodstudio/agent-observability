export type ChartTone = 'accent' | 'hot' | 'neutral';

// 'tile' is the default embedded-in-StatTile size. 'large' is used for the
// session-detail trajectory charts. Only the CSS box changes size; the SVG's
// internal geometry (viewBox 0 0 120 28, the W/H/PAD constants below) is
// identical for both, since `.d-spark-lg` only overrides max-width/height/
// margin-top and the viewBox scales via preserveAspectRatio="none" to fill
// whatever box it's given.
export type SparkSize = 'tile' | 'large';

export interface SparkProps {
  points: Array<[number, number]>;
  tone?: ChartTone;
  size?: SparkSize;
  // Names the metric and window for screen readers (e.g. "live burn, 14 day
  // trend"). Every sparkline on a page needs a distinct one; no default of
  // its own: the prop is required, so every caller states one.
  label: string;
}

// 'neutral' (the gray chart-other token) is what stat-tile trend lines use;
// 'hot' stays reserved for real-dollar series per the palette rule.
const TONE_CLASS: Record<ChartTone, string> = {
  accent: 'text-chart-1',
  hot: 'text-chart-real',
  neutral: 'text-chart-other',
};

// Two complete, mutually exclusive class sets rather than a base plus an
// override, since a wrapper cannot reliably override same-specificity
// Tailwind utilities on the same element.
const SIZE_CLASS: Record<SparkSize, string> = {
  tile: 'mt-2.5 h-[28px] w-full max-w-[120px]',
  large: 'mt-2 h-[100px] w-full',
};

// Axis-free trend line for a stat tile. Renders nothing (not an empty box)
// when
// there is not enough data to draw a line, so a caller can embed
// `<Spark ... />` directly as StatTile's `spark` prop either way.
export function Spark({ points, tone = 'accent', size = 'tile', label }: SparkProps) {
  if (!points || points.length < 2) return null;

  const W = 120, H = 28, PAD = 2;
  const t0 = points[0][0];
  const span = Math.max(points[points.length - 1][0] - t0, 1);
  const vals = points.map(([, v]) => v);
  const vMax = Math.max(...vals, 0) || 1;
  const x = (t: number) => PAD + ((t - t0) / span) * (W - 2 * PAD);
  const y = (v: number) => PAD + (H - 2 * PAD) * (1 - v / vMax);
  const line = points.map(([t, v]) => `${x(t).toFixed(2)},${y(v).toFixed(2)}`).join(' ');
  const lastT = points[points.length - 1][0];

  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      preserveAspectRatio="none"
      role="img"
      aria-label={label}
      className={`block ${SIZE_CLASS[size]} ${TONE_CLASS[tone]}`}
    >
      <polygon
        points={`${line} ${x(lastT).toFixed(2)},${H} ${x(t0).toFixed(2)},${H}`}
        fill="currentColor"
        opacity={0.08}
        stroke="none"
      />
      <polyline
        points={line}
        fill="none"
        stroke="currentColor"
        strokeWidth={1.25}
        strokeLinejoin="round"
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}
