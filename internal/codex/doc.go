// Package codex combines documented App Server reads with a narrow local
// rollout adapter. Collect emits replacement db.SessionRow values suitable for
// UPSERT, with stable codex:<profile-id>:<native-thread-id> identities. A failed
// or incomplete collection must not delete older rows. NewCollector avoids
// reparsing unchanged files and reloading unchanged turn histories; its cache
// is expendable and contains no message bodies.
//
// Protocol coverage was verified against codex-cli 0.155.1 on 2026-09-22 using
// read-only copies of its state/history databases in a scratch home. Both
// legacy and paginated histories support thread/list, thread/read, and
// thread/turns/list. thread/items/list works for paginated histories and returns
// -32601 for legacy histories. Listing explicitly includes every documented
// source kind, all model providers, and both archived/non-archived pages, with
// useStateDbOnly=true to avoid scan-and-repair. The adapter itself never opens
// Codex's private SQLite schema. App Server startup can maintain its own runtime
// databases; it is issued only allowlisted read requests and never receives a
// thread/start, thread/resume, turn/start, auth/config write, or model prompt.
// See https://learn.chatgpt.com/docs/app-server.
//
// The public history API has no per-response token usage in that release.
// Local sessions/ and archived_sessions/ rollout JSONL therefore remain the
// token authority for both storage modes. Modern token_usage_record.usage and
// legacy token_count.last_token_usage are per-response values. Their cumulative
// thread_token_usage and total_token_usage ledgers diverge after compaction and
// never share a delta baseline. Adjacent mirrors are reconciled by per-response
// usage and turn identity; modern records supply the richer token subsets.
// Legacy checkpoints deduplicate quota-only repeats, but a cumulative decrease
// or subset correction never imports the full lifetime total. Missing usage,
// replayed checkpoints and ambiguous accounting differences are reported as
// partial coverage. Response IDs deduplicate retries/copies, fork ancestry
// removes copied usage, and subagent_history_start_ordinal excludes inherited
// context. The first session_meta is authoritative even when child history
// embeds a parent's later metadata. Subagent rows carry their own costs; those
// costs are never also added to the parent. Malformed, missing, unsupported, or
// inaccessible sources are reported explicitly through Warnings/Complete and
// per-row UsageKnown. Partial final lines are retried on the next scan.
// Sessions containing only explicitly rejected unsupported-model requests can
// establish zero usage when every started turn was rejected and no activity,
// damaged record, inherited history or conflicting API history exists. An
// interrupted turn never establishes zero by itself. Missing-token reasons
// are per-session Details, displayed once with aggregate history coverage.
//
// Database token buckets partition the total: Input excludes cache reads and
// writes; Output already includes ReasoningTokens. Detail.Models[].Input uses
// Codex's encompassing input count. Token costs use public Standard API rates
// as estimates, including documented context-size tiers, not billing data or
// subscription consumption. CostKnown=false preserves an unknown/partially
// priced session while CostUSD retains its known subtotal. Service-tier,
// regional, negotiated and tool-specific charges are not inferred.
// These all-usage estimates are internal analytical data only. They must not
// be displayed as money spent or outside-plan charges. Detail.Billing exposes
// token activity without that valuation: supported rollout records contain no
// per-request billing route, so every request remains unclassified and the
// outside-allowance dollar estimate stays nil. Historical quota snapshots,
// crossing 100%, credit balances and present-day account settings cannot prove
// an individual request incurred a charge. Billed dollars are not available
// through the supported App Server methods; the account's credit overage is,
// as its individual limit's used amount, and the server reports it there.
//
// ReadUsage fetches only account/rateLimits/read. It never presents historical
// rollout rate limits as current. Buckets preserve nullable permissions,
// credits, spending controls and reset times; credit-only responses are valid.
// Reset credits are separate from purchased credits. Protocol amount strings
// have no declared currency and are never converted to dollars. No usable
// quota/control fields returns an error; callers retain older snapshots with
// their original timestamps. AccountID stays empty
// unless explicitly supplied by the server; multiple homes must not be assumed
// to represent independent allowances. Live authenticated quota fetching was
// not verified in the scratch probe (credentials were not copied).
package codex
