export interface AttrListRow {
  key: string;
  value: number;
}

export interface AttrListProps {
  rows: AttrListRow[];
  // formatted display string per row, e.g. money(row.value); the caller
  // already knows whether this list is money, a count, or something else.
  display: (row: AttrListRow) => string;
  empty: string;
  note?: string;
  // one row's usage line, e.g. Overview's per-model caption, or null for a
  // list with no per-row detail (Health's log volume table).
  metaOf?: (key: string) => string | null;
}

// Attribution/breakdown list: hairline-separated rows with the name left,
// optional meta caption inline, and a right-aligned tabular value. The
// one-measure-per-row shape Overview's attribution panels, Health's
// log-volume list and Spend's breakdowns all share; DataTable stays the
// real multi-column table.
export function AttrList({ rows, display, empty, note, metaOf }: AttrListProps) {
  if (!rows.length) return <p className="m-0 text-muted">{empty}</p>;
  return (
    <div className="flex flex-col">
      {rows.map((r) => {
        const meta = metaOf?.(r.key) ?? null;
        return (
          <div
            key={r.key}
            className="flex items-baseline gap-2 border-b border-hairline py-[7px] last:border-0"
          >
            <span className="min-w-0 flex-1 truncate text-[13px] text-secondary">{r.key}</span>
            {meta ? <span className="whitespace-nowrap text-[12px] text-faint">{meta}</span> : null}
            <span className="text-[13px] font-medium tabular-nums text-primary">{display(r)}</span>
          </div>
        );
      })}
      {note ? <p className="m-0 mt-2 text-[12px] text-muted">{note}</p> : null}
    </div>
  );
}
