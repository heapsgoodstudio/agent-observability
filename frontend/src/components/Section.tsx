import type { ReactNode } from 'react';

export interface SectionProps {
  id?: string;
  title?: string | null;
  // Trailing action rendered right of the title, e.g. Overview's
  // "Open in Spend" link beside the attribution header.
  action?: ReactNode;
  // A wide section stacks its children in one column; otherwise they flow
  // into a responsive multi-column grid
  // (`repeat(auto-fit, minmax(min(100%, 300px), 1fr))`).
  wide?: boolean;
  children: ReactNode;
}

// Page frame: a titled group of content blocks. The layout scaffold every
// page sits in, so the section grid convention is defined once here rather
// than per page. Titles render as the design system's uppercase micro
// label, not a heading-sized line; hierarchy comes from spacing.
export function Section({ id, title, action, wide = false, children }: SectionProps) {
  return (
    <section id={id} className="mb-8 last:mb-0">
      {title || action ? (
        <div className="mb-3 flex items-baseline gap-2">
          {title ? (
            <h2 className="m-0 text-[11px] font-medium uppercase tracking-[0.04em] text-muted">{title}</h2>
          ) : null}
          {action ? <div className="ml-auto text-[12px] text-accent-text">{action}</div> : null}
        </div>
      ) : null}
      <div className={wide ? 'grid grid-cols-1 gap-3' : 'grid grid-cols-[repeat(auto-fit,minmax(min(100%,300px),1fr))] gap-x-12 gap-y-6'}>
        {children}
      </div>
    </section>
  );
}
