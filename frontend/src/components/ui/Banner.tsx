import type { ReactNode } from 'react';

export type BannerTone = 'error' | 'warn' | 'info';

export interface BannerProps {
  tone: BannerTone;
  children: ReactNode;
}

const TONE_CLASS: Record<BannerTone, string> = {
  error: 'bg-danger-bg text-danger-text',
  warn: 'bg-warn-bg text-warn-text',
  info: 'bg-accent-bg text-accent-text',
};

// Inline notice strip: error for unreachable stores and failed fetches,
// warn for stale data, info for context notes. Tinted background, no
// border; the one shape every page uses for inline failure text, replacing
// the per-page color-mix paragraphs.
export function Banner({ tone, children }: BannerProps) {
  return (
    <p className={`m-0 rounded-md px-3 py-2 text-[13px] wrap-anywhere ${TONE_CLASS[tone]}`}>
      {children}
    </p>
  );
}
