# Oh My Pi session provider

Oh My Pi (`@oh-my-pi/pi-coding-agent`, command `omp`) is a coding agent that talks to many inference providers: Baseten, Fireworks, LM Studio and others. Fireworks and Baseten only report whole-account billing, and Baseten cannot filter by key or user, so this tool tracks that spend through the harness that makes the calls.

## What omp records

Sessions live under `<home>/sessions/` where `<home>` is `~/.omp/agent`.

| Path | Meaning |
|---|---|
| `sessions/<project>/<stem>.jsonl` | A main session |
| `sessions/<project>/<stem>/<id>.jsonl` | A subagent transcript of that session |
| `sessions/<project>/<stem>/__advisor*.jsonl` | An advisor transcript of that session |
| `sessions/<project>/<stem>/*.log` | Tool output, ignored |

Each line is one JSON entry. The ones this tool reads:

- `{"type":"session","id":"<uuid>","timestamp":"<iso>","cwd":"<abs>","title":"...","parentSession":"<path>"}`. `parentSession` is present on a fork.
- `{"type":"message","id":"<8 hex>","timestamp":"<iso>","message":{"role":"assistant","provider":"baseten","model":"zai-org/GLM-5.3","api":"...","usage":{...},"stopReason":"toolUse|stop|error|aborted","responseId":"...","duration":<ms>,"content":[{"type":"toolCall","name":"read","id":"..."}]}}`
- `{"type":"model_usage","id":"...","timestamp":"<iso>","provider":"...","model":"...","api":"...","usage":{...}}`, which is usage outside an assistant message.
- `{"type":"message","message":{"role":"user","synthetic":<bool>,...}}`
- `{"type":"message","message":{"role":"toolResult","isError":<bool>,...}}`

`usage` is `{input, output, cacheRead, cacheWrite, totalTokens, reasoningTokens, cost:{input, output, cacheRead, cacheWrite, total}}`. `input` excludes cache reads. `output` includes reasoning.

**The cost is omp's estimate, not a bill.** The token counts come from the provider's response. omp multiplies them by the rate card in its bundled model catalog. On 2026-09-22 one Fireworks GLM 5.3 Flash call priced exactly at Fireworks' billed unit rates ($0.15/M input, $0.50/M output). A missing `cost` means omp had no price for the model, so the cost is unknown. A zero `cost` means the request was free, as with a local LM Studio model.

## How it appears

It behaves like Claude and Codex, with one difference: its accounts are detected, not named.

| | Claude, Codex | Oh My Pi |
|---|---|---|
| Group | Claude, Codex | Oh My Pi, fixed (internal key `omp`) |
| Profile name | Entered in Settings | None asked. It is the harness name, plus the directory when there are two |
| Accounts | Provider sign-ins, linked to profiles | One per inference provider in its logs (`omp:baseten`, Fireworks AI, LM Studio), local ones included |
| Primary, Repair | Yes | No. Nothing runs through it and nothing is wired |
| Sidebar figure | % remaining | Estimated spend this month, or tokens this month for a provider with no recorded cost |
| Freshness | Confirmed | Collected |
| Hide | No | Per account, with a switch in Settings; a hidden account leaves the sidebar and Overview, nothing is deleted |

An account's Activity lists only sessions that sent at least one request to that provider. A session that used two providers appears under both, with totals for the whole session.

## Display rule

Nothing about Oh My Pi usage is included in a plan allowance, so its dollars are shown. They are always labelled as Oh My Pi's estimate at public API rates, never as billed spend. Unknown cost is shown as unknown, never as $0, and a sub-cent spend reads `<$0.01`, never `$0.00`.

## Parsing rules

- One row per transcript file, ID `omp:<profile-id>:<session id>`. A nested transcript without its own header uses `<parent session id>/<file stem>` as its session ID. Its `ParentID` is the parent session's row ID.
- A fork's `ParentID` is the row ID of the session its `parentSession` path names, when that file is readable.
- **A request is counted once, across every file of the profile.** Forks copy history, so the same `responseId` can appear in several files. The key is `responseId` when present, otherwise `<file>#<entry id>`. The request belongs to the file whose session started earliest.
- Malformed lines are skipped. A final line without a newline is ignored until it is complete.
- `CostKnown` is false when any counted request has no finite `cost.total`. The row's `CostUSD` keeps the known subtotal.
