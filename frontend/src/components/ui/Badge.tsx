import type { ReactNode } from 'react';

export type BadgeTone = 'neutral' | 'ok' | 'warn' | 'danger' | 'accent';

export interface BadgeProps {
  tone: BadgeTone;
  children: ReactNode;
}

// Small status chip on one tone vocabulary shared with StatTile and the
// chart tone props. Tinted background plus the readable text step of the
// same tone (Linear's label treatment); "danger" is reserved for
// real-dollar states throughout this design system.
const TONE_CLASS: Record<BadgeTone, string> = {
  neutral: 'bg-selected text-secondary',
  ok: 'bg-ok-bg text-ok-text',
  warn: 'bg-warn-bg text-warn-text',
  danger: 'bg-danger-bg text-danger-text',
  accent: 'bg-accent-bg text-accent-text',
};

export function Badge({ tone, children }: BadgeProps) {
  return (
    <span
      className={`inline-flex items-center whitespace-nowrap rounded-sm px-[7px] py-[2px] text-[11px] font-medium leading-[14px] ${TONE_CLASS[tone]}`}
    >
      {children}
    </span>
  );
}
