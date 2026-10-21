# Allowance and overage accounting

## Display contract

Included usage and usage with unknown billing classification have no displayed
dollar value. Activity views use tokens, requests, context, tools and elapsed
time. This applies to both providers, session details, findings, generated
reports, notifications and the terminal status line.

Money has three separate sources:

- Provider-reported overage: an explicit provider financial field, with its
  reporting period and observation freshness.
- Estimated overage: public API token pricing applied only to requests that
  the source explicitly identifies as outside included allowance. An estimate
  must identify its pricing source and cannot be added to reported spending
  that may already include the same usage.
- Priced credit overage: credits the provider reports as spent beyond included
  allowance, times a configured price per credit. It is an estimate, named as
  one, and the credits stay visible beside it.

Unknown amounts remain absent. A missing balance, charge or classification
must never become a measured zero. Provider credit units are not assumed to
be dollars.

## Codex evidence boundary

The installed App Server supports `account/rateLimits/read`. The adapter keeps
its separate limit buckets, primary/secondary windows, included-use permission,
credit metadata, individual limits, spend-control state and reset-credit count.
Missing booleans and amounts remain distinct from false and zero.

Supported local token records do not identify a request's billing route.
Neither a 100% window, an adjacent quota snapshot nor today's plan state proves
that an earlier request consumed paid credits. The provider can allow an active
turn to finish after a limit is reached. The installed account protocol does
not provide billed-overage dollars or credit transactions. Its optional thread
credit/USD estimates cover activity without establishing outside-cap
attribution, so they are not used as overage, and no recorded request is
priced.

Codex overage is read at the account instead. `individualLimit.used` is the
credits spent under the member's individual credit limit in its current period,
and Codex spends credits only once the included allowance is used up
([Codex limits and pricing](https://learn.chatgpt.com/docs/pricing)). It is
reported as `reported_overage_credits`, in provider units, with the period's
reset in `period_resets_at`. The `codex` limit bucket is the account's own
meter; buckets are never summed, since one limit can be mirrored in several.
An unreadable or missing amount leaves the overage unavailable.

Codex reports no dollars, and the price per credit depends on the plan.
`CODEX_CREDIT_USD` in `config.env` supplies it, and `estimated_overage_usd` is
the credits times that price, with `credit_price_usd` beside it. Every view
names that figure an estimate at the configured price. With no price, the
overage stays in credits and the workspace total marks itself partial.

## Token-history coverage

Historical token coverage is independent of live plan limits and billing.
The activity panel gives the number of sessions with known totals and lists
affected sessions with a reason when usage is unknown. It shows this coverage
once, rather than repeating a generic warning at the top of the dashboard.

An intact session containing only explicitly rejected unsupported-model
requests, with no model output or tools, has known zero model usage. This
requires matching started and rejected turns, an intact header and records,
and no conflicting activity from another log copy or App Server history.
An interrupted turn without token records stays unknown: interruption and an
absence of output do not establish zero usage. Previously indexed usage is
retained if a later read loses its records.

History import in Settings retries the available sources. Restoring a missing
session log can recover its totals; repeated imports cannot recreate token
records that Codex never saved. No session deletion or authentication change
is needed to handle a historical gap.

## Scheduling and state

- Account limits refresh every minute, independently of five-minute history
  indexing. The same daemon lease prevents the scheduled CLI from duplicating
  server work.
- `codex.quota.<profile>` and `codex.snapshot.<profile>` have separate writers.
  `monitor.ReadSnapshot` merges them, retaining legacy snapshot compatibility.
  Conditional writes preserve Settings invalidations during in-flight reads.
- A failed read retains its last successful observation time. Snapshots older
  than two minutes, failed refreshes and changed profile directories cannot
  imply current headroom or permission. The browser also ages retained data.
- Shared accounts use their newest whole snapshot. A newer credit-only response
  supersedes an older response's windows. Unidentified accounts remain separate.
- Notifications warn at 75%, 90%, 95% and 100%. Durable markers distinguish
  account/profile, bucket, window and reset period, tolerating reset jitter.
  One observation crossing several thresholds sends the most urgent notice.
- Passing a reset time never establishes renewed permission. No wrapper,
  inference, reset-credit redemption or billing-setting change is required.

## Other displays

Claude's explicit extra-usage field remains the source of reported overage.
Unfiltered reported billing sums labelled accounts after deduplicating profiles;
an unresolved selected account cannot inherit another account's billing.

Findings use activity measurements rather than dollar weights. Report schema 2
requests usage-focused prose without monetary valuations; schema 1 and markdown
reports remain on disk and require regeneration before dashboard display.
Internal historical token-cost fields remain compatibility data, not bills.

## References

- [Codex App Server](https://learn.chatgpt.com/docs/app-server)
- [Codex limits and pricing](https://learn.chatgpt.com/docs/pricing)
- [Workspace usage and cost](https://learn.chatgpt.com/docs/enterprise/chatgpt-work-usage-and-cost)
- [Public API pricing](https://developers.openai.com/api/docs/pricing)

The app provides warnings and visibility. Provider spending controls remain the
enforcement mechanism for restricting paid usage.
