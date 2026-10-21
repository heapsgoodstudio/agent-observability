export interface DataTableColumn {
  key: string;
  label: string;
  num?: boolean;
  title?: string;
}

export interface DataTableRow {
  cells: Record<string, string>;
  title?: string;
  cellTitles?: Record<string, string>;
}

export interface DataTableProps {
  columns: DataTableColumn[];
  rows: DataTableRow[];
  empty: string;
  // A footer caption. Nearly every call site passes one, e.g. Spend's
  // model-usage table summary line.
  note?: string;
}

// Read-only multi-column table of pre-formatted strings: every cell is text
// the caller has already formatted (money(), compactInt(), etc.), num
// right-aligns and sets tabular figures, and the table scrolls within its own
// wrapper (min-width floor) rather than widening the page. `empty` is
// required and has no default, so each table states its own contextual
// message rather than a generic "no data".
export function DataTable({ columns, rows, empty, note }: DataTableProps) {
  if (!rows.length) {
    return <p className="m-0 text-muted">{empty}</p>;
  }
  return (
    <div>
      <div className="overflow-x-auto">
        <table className="w-full min-w-[720px] border-collapse">
          <thead>
            <tr>
              {columns.map((c) => (
                <th
                  key={c.key}
                  title={c.title}
                  className={`whitespace-nowrap border-b border-hairline px-3 pb-2 text-[12px] font-medium text-muted ${
                    c.num ? 'text-right' : 'text-left'
                  }`}
                >
                  {c.label}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((r, i) => {
              const isLastRow = i === rows.length - 1;
              // Index as key: rows carry no stable id of their own, only
              // plain formatted strings.
              return (
                <tr key={i} title={r.title}>
                  {columns.map((c, ci) => (
                    <td
                      key={c.key}
                      title={r.cellTitles?.[c.key]}
                      className={`px-3 py-2 align-baseline text-[13px] ${isLastRow ? 'border-0' : 'border-b border-hairline'} ${
                        c.num ? 'text-right tabular-nums text-secondary' : ci === 0 ? 'text-primary' : 'text-secondary'
                      }`}
                    >
                      {r.cells[c.key] ?? ''}
                    </td>
                  ))}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {note ? <p className="m-0 mt-2.5 text-[12px] text-muted">{note}</p> : null}
    </div>
  );
}
