// Axis arithmetic. Pure; used by the Columns and Line chart components for
// gridline/tick placement.

// Round up to a half-decade step so axis labels stay readable.
export function niceCeil(v: number): number {
  if (!(v > 0)) return 1;
  const mag = 10 ** Math.floor(Math.log10(v));
  return (Math.ceil((v / mag) * 2) / 2) * mag;
}

// Evenly spaced gridline values from zero to yMax inclusive.
export function ticks(yMax: number, count = 2): number[] {
  return Array.from({ length: count + 1 }, (_, i) => (yMax * i) / count);
}
