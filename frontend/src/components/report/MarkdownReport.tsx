import { renderMarkdown } from '@/src/lib/markdown';

// Markdown report typography, moved here from SessionDetail so both report
// surfaces reach it without importing across pages.
//
// list-disc and list-decimal are not decoration: the CSS reset strips list
// markers, so the indent alone was all that marked a list, and every bullet in
// every report written before this rendered as a floating indented line.
export const REPORT_CLASS = 'text-[13px] leading-[1.55] [&_h3]:my-4 [&_h3]:text-[15px] [&_h3]:font-semibold [&_h4]:my-3.5 [&_h4]:text-[13px] [&_h4]:font-medium [&_h4]:text-secondary [&_p]:my-1.5 [&_ul]:my-1.5 [&_ul]:list-disc [&_ul]:pl-5 [&_ol]:my-1.5 [&_ol]:list-decimal [&_ol]:pl-5 [&_li]:marker:text-faint [&_code]:rounded-sm [&_code]:bg-selected [&_code]:px-1 [&_code]:py-0.5 [&_code]:font-mono [&_code]:text-[0.85em] [&_pre]:overflow-x-auto [&_pre]:rounded-md [&_pre]:border [&_pre]:border-hairline [&_pre]:bg-sidebar [&_pre]:p-2.5 [&_pre_code]:bg-transparent [&_pre_code]:p-0';

// The fallback rendering: a report whose run printed something the structured
// parser could not read, or one written before the structured format existed.
// The note is there so a page that looks unlike the rest of the dashboard
// explains itself rather than reading as a regression.
export function MarkdownReport({ md }: { md: string }) {
  return (
    <>
      <p className="m-0 mb-3 text-[12px] text-muted">
        This report is in the older text format.
      </p>
      <div
        className={REPORT_CLASS}
        // renderMarkdown (src/lib/markdown.ts) HTML-escapes every line before
        // adding any tag, so this is safe to assign.
        dangerouslySetInnerHTML={{ __html: renderMarkdown(md) }}
      />
    </>
  );
}
