import type { ReactNode } from 'react';

export type MeterBandTone = 'warn' | 'danger';

export interface MeterBand {
  at: number;
  tone: MeterBandTone;
}

export interface MeterProps {
  value: number | null;
  max: number;
  bands?: MeterBand[];
  // Left-hand label rendered on the same line as the percentage; caption
  // text below the track stays the caller's job.
  label?: ReactNode;
}

// Horizontal plan-window meter: label and percentage on one line, a 4px
// track under them. Takes any value/max pair plus an explicit band list, so
// it is not tied to plan windows; when bands is omitted it defaults to
// 90%/100%-of-max thresholds.
function defaultBands(max: number): MeterBand[] {
  return [
    { at: max * 0.9, tone: 'warn' },
    { at: max, tone: 'danger' },
  ];
}

type MeterTone = 'ok' | MeterBandTone | 'unknown';

function toneFor(value: number, bands: MeterBand[]): 'ok' | MeterBandTone {
  const sorted = [...bands].sort((a, b) => a.at - b.at);
  let tone: 'ok' | MeterBandTone = 'ok';
  for (const band of sorted) {
    if (value >= band.at) tone = band.tone;
  }
  return tone;
}

const FILL_COLOR: Record<MeterTone, string> = {
  ok: 'var(--accent-default)',
  warn: 'var(--warn-default)',
  danger: 'var(--danger-default)',
  unknown: 'var(--text-faint)',
};
const PCT_CLASS: Record<MeterTone, string> = {
  ok: 'text-primary',
  warn: 'text-warn-text',
  danger: 'text-danger-text',
  unknown: 'text-muted',
};

export function Meter({ value, max, bands, label }: MeterProps) {
  const known = value !== null && Number.isFinite(value);
  const clamped = known ? Math.max(0, Math.min(max, value)) : 0;
  const pct = max > 0 ? (clamped / max) * 100 : 0;
  const tone = known ? toneFor(clamped, bands ?? defaultBands(max)) : 'unknown';

  return (
    <div className="min-w-0">
      <div className="flex items-baseline justify-between gap-3">
        {label ? <span className="min-w-0 truncate text-[12px] text-secondary">{label}</span> : <span />}
        <span className={`text-[12px] font-medium tabular-nums ${PCT_CLASS[tone]}`}>
          {known ? `${Math.round(pct)}%` : 'n/a'}
        </span>
      </div>
      <div
        className="mt-1.5 h-1 overflow-hidden rounded-full bg-selected"
        role="meter"
        aria-valuenow={known ? Math.round(pct) : undefined}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label={known ? `${Math.round(pct)} percent used` : 'no data'}
      >
        <div
          className="h-full rounded-full transition-[width] duration-150 ease-out"
          style={{ width: `${pct}%`, background: FILL_COLOR[tone] }}
        />
      </div>
    </div>
  );
}
