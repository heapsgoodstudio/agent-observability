import type { ReactNode } from 'react';

export type StatTileTone = 'default' | 'ok' | 'warn' | 'danger';
export type StatTileSize = 'lg' | 'md';

export interface StatTileProps {
  label: string;
  value: string;
  note?: string;
  tone?: StatTileTone;
  // Beyond the base {label, value, note?, tone?}: a unit suffix ("/hr"), a
  // native-tooltip title (the exact figure behind an abbreviated value), an
  // embedded sparkline (the Spark chart component), and the two numeral
  // sizes: 'lg' for a view's hero band, 'md' for secondary stat rows.
  unit?: string;
  title?: string;
  spark?: ReactNode;
  size?: StatTileSize;
}

const TONE_CLASS: Record<StatTileTone, string> = {
  default: 'text-primary',
  ok: 'text-ok-text',
  warn: 'text-warn-text',
  danger: 'text-danger-text',
};

const SIZE_CLASS: Record<StatTileSize, string> = {
  lg: 'text-[24px] leading-[30px] tracking-[-0.02em]',
  md: 'text-[17px] leading-[22px] tracking-[-0.01em]',
};

// Big-number stat: micro label, Inter numeral with tabular figures (no
// monospace; mono is reserved for code and ids in this design system),
// caption, optional sparkline.
export function StatTile({ label, value, note, tone = 'default', unit, title, spark, size = 'lg' }: StatTileProps) {
  return (
    <div className="min-w-0" title={title}>
      <p className="m-0 mb-1 text-[11px] font-medium uppercase tracking-[0.04em] text-muted">{label}</p>
      <p className={`m-0 font-semibold tabular-nums ${SIZE_CLASS[size]} ${TONE_CLASS[tone]}`}>
        <span>{value}</span>
        {unit ? <span className="ml-1 text-[13px] font-medium tracking-normal text-muted">{unit}</span> : null}
      </p>
      {spark ?? null}
      {note ? <p className="m-0 mt-1 text-[12px] leading-[18px] text-muted">{note}</p> : null}
    </div>
  );
}
