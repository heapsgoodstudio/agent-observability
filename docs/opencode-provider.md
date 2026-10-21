# opencode session provider

opencode is a coding agent that, like Oh My Pi, sends requests to many inference providers: Baseten, Fireworks, a local LM Studio and others. It follows the Oh My Pi pattern exactly (see `docs/omp-provider.md`): a fixed "opencode" group, no name asked, and one account per inference provider detected in its data, under the opencode group. The same provider used from Oh My Pi and from opencode is two accounts, one under each harness.

## What opencode records

Everything is in one sqlite database, `opencode.db`, in opencode's data directory (`~/.local/share/opencode`), kept in WAL mode and written while opencode runs. `internal/opencode` opens it read-only and query-only and never writes, checkpoints or vacuums it.

| Table | Used for |
|---|---|
| `session` | One row per session: `id`, `parent_id` (a subagent's parent), `directory`, `time_created`, `time_updated` (epoch ms) |
| `message` | `data` JSON. Assistant messages carry `providerID`, `modelID`, `cost`, `tokens{input, output, reasoning, cache{read, write}}` and an optional `error`; user messages count as turns |
| `part` | `data` JSON. `type:"tool"` parts are tool calls, named by `tool` |

Each assistant message is one model step, so message-level tokens and cost are used directly.

## Three facts about its numbers

Verified on 2026-09-23 against opencode 1.18.25, from its code and a live request.

- **`tokens.input` excludes cache reads and writes.** opencode fills it from the AI SDK's `nonCachedInputTokens`.
- **`tokens.output` excludes reasoning, and reasoning is billed at the output rate.** One Fireworks GLM 5.3 Flash request recorded 7,261 input, 3 output and 9 reasoning tokens at $0.00109515, which is exactly 7,261 × $0.15/M + (3 + 9) × $0.50/M. So the stored output is output plus reasoning, the same meaning Codex and Oh My Pi give it.
- **A model opencode has no price for is recorded as $0.** Its cost is `model.cost?.input ?? 0`, so an unpriced model and a free one look the same.

## How $0 is read

Because of the last fact, a $0 cost is interpreted per provider:

| Case | Reading |
|---|---|
| Every request to the provider costs $0 | No cost recorded, like a local LM Studio model. It is an account whose headline is token usage, never a dollar figure |
| The provider has priced requests, and this one costs $0 but used tokens | Not priced. Its cost is unknown, so totals read as a lower bound (`$2.10+`) and the model reads Unavailable |
| A $0 request that used no tokens | Known $0 (for example an immediate error) |

A paid provider opencode never prices at all therefore appears with no cost recorded, showing its tokens rather than any spend. That is the one case this cannot price.
