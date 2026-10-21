import type { ReactNode } from 'react';
import { Banner } from './Banner';

export interface CardProps {
  title?: string | null;
  // Right-aligned caption in the header row: the honest data note that used
  // to sit under the content ("billed figure as of 4m ago", totals, peaks).
  meta?: ReactNode;
  children: ReactNode;
  error?: string | null;
}

// A panel: bordered surface for charts and tables. The de-carded layouts
// (hero bands, attribution lists, health groups) do NOT use this; only
// content that reads better on its own surface does. `error` and `children`
// are accepted together rather than `error` replacing `children` outright,
// so a failed refresh puts a fresh failure above the last good render
// instead of blanking a populated panel.
export function Card({ title, meta, children, error }: CardProps) {
  return (
    <div className="min-w-0 rounded-lg border border-line bg-panel p-4">
      {title || meta ? (
        <div className="mb-3 flex flex-wrap items-baseline gap-2">
          {title ? <p className="m-0 text-[13px] font-medium text-primary">{title}</p> : null}
          {meta ? <div className="ml-auto max-w-[70%] text-right text-[12px] text-muted">{meta}</div> : null}
        </div>
      ) : null}
      <div className="min-w-0">
        {error ? (
          <div className="mb-2.5">
            <Banner tone="error">{error}</Banner>
          </div>
        ) : null}
        {children}
      </div>
    </div>
  );
}
