import type { ReactNode } from 'react';

export interface ViewHeaderProps {
  title: ReactNode;
  children?: ReactNode;
}

// The 48px local header every view renders instead of a big page title:
// view name (or breadcrumb) on the left, that view's own controls and
// status pills right-aligned. The hairline below is the page's only
// full-width divider; content sections use their own.
export function ViewHeader({ title, children }: ViewHeaderProps) {
  return (
    <header className="flex h-12 items-center gap-2 border-b border-hairline px-6">
      <div className="flex min-w-0 items-center gap-2 text-[14px] font-medium tracking-[-0.01em] text-primary">
        {title}
      </div>
      {children ? <div className="ml-auto flex flex-wrap items-center gap-2">{children}</div> : null}
    </header>
  );
}
