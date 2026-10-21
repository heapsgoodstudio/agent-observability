import type { ReactNode } from 'react';

export interface ShareBarProps {
  label: string;
  // Bar width, 0 to 100. Callers scale this against their own largest value;
  // `share` is the text printed beside it, which is not the same number.
  pct: number;
  share: string;
  value: string;
  note?: ReactNode;
}

// One row of a "where the money went" breakdown: label, share and money on a
// line, a 4px track under them, an optional plain-language note below.
//
// Meter is deliberately not reused. It models how close a value is to a limit,
// with warn and danger bands at 90% and 100% of max, and a share of spend has
// no limit and no danger state: 80% of a session's cost going to helper agents
// is a fact, not an alarm.
export function ShareBar({ label, pct, share, value, note }: ShareBarProps) {
  const width = Math.max(0, Math.min(100, pct));
  return (
    <div className="min-w-0 border-b border-hairline py-2 last:border-0">
      {/* The track is a fixed-width column rather than the full row width. A
          full-width 4px track reads as a horizontal rule between rows instead
          of as a bar, which is exactly what it looked like when tried. */}
      <div className="flex items-center gap-3">
        <span className="min-w-0 flex-1 truncate text-[13px] text-secondary">{label}</span>
        <div className="h-1.5 w-[140px] flex-none overflow-hidden rounded-full bg-selected">
          <div className="h-full rounded-full bg-accent" style={{ width: `${width}%` }} />
        </div>
        <span className="w-9 flex-none text-right text-[12px] tabular-nums text-faint">{share}</span>
        <span className="w-[76px] flex-none text-right text-[13px] font-medium tabular-nums text-primary">{value}</span>
      </div>
      {note ? <p className="m-0 mt-1 text-[12px] leading-normal text-muted">{note}</p> : null}
    </div>
  );
}
