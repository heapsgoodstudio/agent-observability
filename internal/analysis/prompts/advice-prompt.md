Report on how this person could get more out of their coding agents while using less allowance and time.

Findings the dashboard computed, already checked against the stores. Treat these
numbers as given and do not recompute them. The list also carries entries with
severity `clean`, which were checked and found fine, and `insufficient`, which
could not be judged for lack of history: neither kind is a problem and neither
may be presented as one.

{FINDINGS_JSON}

{DEPTH_BRIEF}

What each section means for this report:

- `story`: the shape of the work. The model mix, how much is delegated, how
  sessions tend to run.
- `cost_drivers`: leave empty. The inputs do not establish which requests
  were paid. Explain token, request and time usage in `story` instead.
- `config_changes`: concrete edits to configuration or instruction files you actually inspected.
  Permissions that could be allowlisted, hooks that add avoidable latency,
  instructions that consume context on every request, MCP servers connected and
  unused. Quote the setting you mean. If the configuration is already tight,
  leave the section out rather than inventing work.
- `problems` and `actions`: habits worth changing, ranked by expected saving,
  each naming the finding or the number it comes from.
- `good`: what these sessions already do well, and why that is worth keeping.
- `fine`: anything in the findings that looks alarming and is fine here.
- `coverage`: what you actually read, whenever that limits the answer.

{REPORT_CONTRACT}
