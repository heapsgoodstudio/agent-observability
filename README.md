# Agent Observability

Agent Observability monitors Claude Code and Codex on your machine. It stores session history and telemetry locally and serves a dashboard at [http://localhost:8427](http://localhost:8427), with provider and profile filters for sessions, token usage, remaining plan allowance, reset times, reported overage, and collection health. Claude also provides billed overage and optional agent-written analyses.

Included and unclassified usage is shown in tokens and allowance percentages, with no dollar valuation. Money appears only for provider-reported overage or an explicitly labelled estimate of usage known to be outside the allowance. [Reading the dollar figures](#reading-the-dollar-figures) explains the source and coverage rules.

Four moving parts get the numbers there:

- **Telemetry wiring.** It points Claude Code's built-in OpenTelemetry (OTel) export, the metrics and logs Claude Code can already emit, at a local pair of stores: VictoriaMetrics for numbers over time, VictoriaLogs for log events.
- **Backfill.** History from before the wiring existed is imported from the session transcripts Claude Code has already written to disk.
- **Polling.** A timed job reads the plan and rate-limit figures that OTel does not cover, such as how much of your subscription's current allowance is spent.
- **Alerts.** Threshold crossings fire macOS notification banners that open the dashboard at the figure they are about.

Dashboard and store endpoints bind to `127.0.0.1`. Monitoring uses the agents' existing local history and authenticated read APIs; it does not start or resume model sessions. Live account-limit reads may contact the provider through its installed CLI. Optional analyses use the selected Claude, Codex, OMP or OpenCode evaluator and incur usage at its configured inference endpoint.

The [repository](https://github.com/heapsgoodstudio/agent-observability) is self-contained: its own Go module, nix devshell, justfile and test suite. Everything below is run from its root.

## Quickstart

For all four agent CLIs, authentication, profile directories, evaluator selection and verification, follow the [complete history and analysis setup guide](docs/analysis-setup.md).

macOS only, on Apple Silicon or Intel. Xcode Command Line Tools are optional for the notification app; installation continues without them.

### With Homebrew

```bash
brew install heapsgoodstudio/tap/agent-observability
claude-obs install
```

Homebrew installs the `claude-obs` command; `claude-obs install` then sets up the dashboard and its background services. After each `brew upgrade agent-observability`, run `claude-obs install --upgrade` so the services use the new version. Before `brew uninstall agent-observability`, run `claude-obs uninstall`.

### From source

Building requires nix with flakes.

```bash
git clone https://github.com/heapsgoodstudio/agent-observability.git
cd agent-observability
nix develop        # or `direnv allow` once
just install
```

`just install` builds `claude-obs`, shows the installation plan, and installs to `~/.agent-observability/`. `claude-obs install` behaves the same from either route: the scripts and notifier sources it places are embedded in the binary. A fresh installation checks only `~/.claude` and `~/.codex`. It never scans custom profile directories or discovers them through environment overrides, and it asks no profile-selection or backfill question. Installation also succeeds when neither directory nor agent executable exists. Add profiles later in **Settings**.

The install directory is `~/.agent-observability` and launchd labels use `com.agent-observability`. The `claude-obs` command, `claude-obs.db` database filename, and `claudeobs.v1` API namespace retain their existing names for the Claude Code collector.

Non-interactive: `claude-obs install --yes`. Initial history import runs in the background, with status and a manual import action in Settings. `--no-backfill` skips the initial history import; `--backfill` queues one on an upgrade. Check a running install with `claude-obs doctor`. `claude-obs uninstall` disconnects owned wiring and stops services while retaining history.

## Upgrading

With Homebrew:

```bash
brew upgrade agent-observability
claude-obs install --upgrade
```

From source:

```bash
just upgrade        # rebuild and reinstall over the existing install, no questions
```

`just upgrade` first checks that this checkout includes the installed Git revision, preventing an older branch from replacing a newer dashboard. Fetch missing history and move your work onto the installed release or a newer descendant before upgrading. Check that no analysis is running before restarting services.

Use it whenever you pull changes. `just upgrade` asks nothing and refuses if there is no readable installation at the target path. It retains the registry's configured profiles, including custom directories, disabled entries, stable identifiers, and primary selections. When no registry exists yet, legacy `PROFILE_DIR`/`PROFILE_DIRS` configuration is migrated once without changing its Claude roster or history labels, and an existing default `~/.codex` is added. Once `profiles.json` exists, later upgrades do not rediscover removed or disabled profiles. The recorded Claude executable is reused while it still works.

### Profiles are configured in Settings

Settings owns the provider, display name, directory, enabled state, and primary profile for each provider. It also shows connection/freshness information and offers repair, history import, and disconnect. Changes take effect without reinstalling. Renaming a profile does not relabel its history; disabling or disconnecting preserves indexed sessions and stored telemetry. Existing telemetry destinations and custom statuslines are preserved. A conflicting setting is reported for manual resolution.

`profiles.json` under the install directory is the source of truth. Shell profile values in `config.env` are generated from it and may be explicitly empty. Additional directories are entered manually in Settings. Existing `--profile-dir`, `--profile-dirs`, and `--claude-bin` CLI overrides remain available for deliberate installer changes.

`just install` is also safe to re-run against an existing installation; it retains configured profiles and confirms the plan.

### What each step does on a re-run

Every step either detects that its work is already done or redoes it safely.

| Step | On a re-run |
|---|---|
| create layout | `MkdirAll`, no-op when the directories exist |
| copy bin, notifier, binary | Replaces each file atomically, via a temp file and a rename. This is the step that must not truncate in place: the server agent is running `bin/claude-obs`, and overwriting it in place invalidates its code signature so the next exec dies with SIGKILL |
| write config.env | Merged, not overwritten. A key you have tuned keeps your value, a key the example has gained since is added at its default, and a key you added that the example does not list is carried to the end. Any key of yours that differs from the shipped default is named in the output, so a changed default is visible rather than silent. The installer rewrites machine values; profile directories and IDs are projected from `profiles.json` under the registry lock |
| fetch store binaries | Skipped when both binaries are already present and executable |
| sqlite migrate | Applies only the migrations not yet applied, and reports how many |
| profile registry and telemetry | Migrates legacy configuration once, preserves stable IDs, and repairs owned JSON/TOML wiring. Conflicting exporters remain unchanged and appear as conflicts in Settings |
| statusline wiring | Adds the default Claude statusline only when none exists. Custom statuslines remain untouched |
| launchd agents | Each agent is booted out and bootstrapped again, so the new binary and plist take effect |
| notifier | Rebuilds when its inputs changed, then registers and posts a confirmation banner when available |
| history import | Queued on a fresh installation unless `--no-backfill`; an upgrade queues it with `--backfill`. The background worker reports progress in Settings |
| verify | Checks the three endpoints and all six loaded agents. Indexing and history import continue in the background |

### What survives an upgrade

Profile configuration and registry writes use private `<filename>.agent-observability.bak.<unixnano>` backups, a transaction journal, and atomic replacement. Disconnect removes only values owned by this installation and leaves subsequent user edits intact. The store data in `state/vmdata` and `state/vldata` is never touched by an upgrade, so metrics and logs history survives.

One limit of the `config.env` merge is worth stating plainly: nothing records whether a value differs because you tuned it or because we changed the shipped default, so your value always wins. If a release bumps a default you have already customised, the installer names the key and both values in its output, and picking up the new default is a manual edit.

## How it works

Six launchd agents run the local services and scheduled jobs without containers. launchd is macOS's own per-user service manager: it starts each agent at login (`RunAtLoad`) and re-runs the timed ones on a fixed interval. All six are named from `LABEL_PREFIX`.

Binaries, scripts, and state live in `~/.agent-observability/`, not the checkout; the repo is source only. The two stores run from binaries that `bin/fetch-stores.sh` downloads and checksum-verifies into `state/bin` at install time, pinned by `VM_VERSION` and `VL_VERSION`.

### Three agents run continuously

| Label | Program | Port | Data | Purpose |
|---|---|---|---|---|
| `<LABEL_PREFIX>.victoriametrics` | `state/bin/victoria-metrics-prod` | 127.0.0.1:8428 | `state/vmdata` | Metrics store (OTel counters, poller series, backfilled history). 24 month retention |
| `<LABEL_PREFIX>.victorialogs` | `state/bin/victoria-logs-prod` | 127.0.0.1:9428 | `state/vldata` | Log store (OTel logs: per-request events, tool decisions, skill activations). 52 week retention |
| `<LABEL_PREFIX>.server` | `claude-obs serve` | 127.0.0.1:8427 | `state/claude-obs.db`, `dashboard/data` | Serves the embedded frontend and Connect API; schedules background collection/imports and runs analyse/advice jobs using the selected provider |

### Three agents run on a timer

| Label | Plist | Script | Job |
|---|---|---|---|
| `<LABEL_PREFIX>.limits-poller` | `~/Library/LaunchAgents/<LABEL_PREFIX>.limits-poller.plist` | `bin/limits-poller.sh` | `StartInterval` 60s. Reads the cached plan/rate-limit utilization from `$PROFILE_DIR/.claude.json` and the statusline tap file, writes `claude_plan_window_pct`, `claude_plan_window_reset_timestamp_seconds`, `claude_extra_usage_usd`, `claude_in_overage`, and related series to VictoriaMetrics; renders the statusline segment file |
| `<LABEL_PREFIX>.usage-alerts` | `~/Library/LaunchAgents/<LABEL_PREFIX>.usage-alerts.plist` | `bin/usage-alert.sh` | `StartInterval` 60s. Edge-triggered threshold checks against VictoriaMetrics, meaning they fire on the crossing rather than on every check while above the line. Fires notification banners that open the dashboard when clicked, tracks dedup state in `state/alerts-state.json` |
| `<LABEL_PREFIX>.session-indexer` | `~/Library/LaunchAgents/<LABEL_PREFIX>.session-indexer.plist` | `claude-obs index --once` | `StartInterval` 300s. Fallback collection while the server collector is unavailable; a shared lease prevents duplicate scheduled work |

There is no separate agent for analysis: the four analysis RPCs run in-process inside `claude-obs serve` above.

### Alert banners come from a small app bundle outside the checkout

`claude-obs install` builds `notifier/` into a small app bundle at `~/Applications/Agent Observability.app` and asks once for notification permission. Alert titles identify the provider and account/profile name. Clicking opens that account directly: Claude allowance crossings use `#now`, and reported-overage/stale-figure alerts use `#overage`. Codex uses its reported account key, falling back to the exact configured profile when account identity is unavailable. Readings and deduplication stay account-scoped, so profiles sharing an account do not create separate allowances and one account's alert cannot suppress another's. Claude's unlabelled/unknown current identities do not generate account alerts; historical ownership assignments never supply current alert identity. The former notional daily-budget alert is retired.

The bundle has to live outside this checkout because macOS refuses notification authorization to bundles inside it. It is the one part of the stack that needs a compiler (`swiftc`, from the Xcode Command Line Tools); without it the installer skips the notifier and alerts fall back to plain `osascript` banners that do nothing when clicked. The same fallback covers the app being deleted or its permission revoked, so an alert is never lost, only its click target.

### Telemetry wiring merges ten keys into settings.json

`claude-obs install` merges an env block into the instrumented profile's `settings.json` pointing Claude Code's own OTel exporter at VictoriaMetrics (metrics) and VictoriaLogs (logs): `CLAUDE_CODE_ENABLE_TELEMETRY`, `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER`, `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`, `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`, `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE`, `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_LOG_TOOL_DETAILS`, and `OTEL_RESOURCE_ATTRIBUTES`. The registry merge preserves unrelated keys. If a telemetry key already has a conflicting value, the profile reports a conflict and its existing destination remains unchanged; the dashboard installation still succeeds.

### The statusline is set if you have none, forked if you do

The statusline is the line Claude Code renders at the bottom of its terminal. If the instrumented profile has no `statusLine` configured, `claude-obs install` sets it to `bin/statusline.sh` directly: model, cwd, branch, context %, plan-window %, session cost, and a pre-rendered month-to-date overage tail.

If the profile already has its own statusline script, the installer leaves it alone. To integrate it manually, pipe stdin to `bin/statusline-tap.sh` in the background, so the poller sees live rate limits, and `cat state/statusline-segment.<profile>.txt` at the end of the line, the poller's pre-rendered month-to-date overage tail. The tap and default statusline resolve the current directory from `CLAUDE_CONFIG_DIR` (or `~/.claude`) to the stable registry ID. Display-name and directory edits therefore retain the same telemetry label and per-profile files. Gate either of them to one profile and the rest get no live statusline source, so their plan windows fall back to the much slower `.claude.json` cache.

## What the dashboard shows

The dark-first desktop dashboard opens on all accounts. Direct sidebar links switch the displayed account without changing CLI authentication. Provider/account/profile scope and Activity filters are carried in the URL.

| Page | Route | Shows |
|---|---|---|
| Overview | `/` | Aligned current/weekly allowance, resets, freshness, and provider-reported overage |
| Account | `/?accountKey=…&provider=…` | Consistent allowance, observed history, billing, and linked profiles |
| Activity | `/sessions` | Scoped sessions, filters, recorded tokens, detail and analysis |
| Insights | `/insights` | Scoped findings and separately saved advice reports for all four providers |
| Token history | `/spend` | Retained provider token coverage, separate from live allowances |
| Collection health | `/health` | Connection freshness, capability gaps and shared service health |
| Settings | `/settings` | Add/manage profiles, enable/disable, primary, repair, disconnect and import |

Session context charts show per-request input, including cache reads and writes. Claude charts follow the main conversation; subagent and background requests retain their usage totals but do not become main-context observations. Codex, Oh My Pi and opencode keep child sessions separate. Real decreases, including compaction, remain visible. Existing Claude transcripts are reindexed after upgrading to correct older context samples; unavailable transcripts cannot be reconstructed.

Settings retains every configured profile even if its directory or provider data is unavailable. Only default profiles are automatically detected; custom directories are manually added. Theme choices persist, with dark as the default and light/system available. Tables contain horizontal overflow in narrow windows; keyboard controls and chart data tables provide alternatives to pointer inspection.

Existing notification hashes (`#now`, `#overage`, `#history`) still resolve to the relevant page and scroll target. Older Findings and Advice routes resolve into Insights.

### Account API and retained observations

`GetUsageSnapshot` adds `schemaVersion: 2`, provider-neutral `accounts`, and optional `provider`, `accountKey`, `profile` filters. The legacy Claude `profiles` projection remains compatible. Account records keep optional usage, permission, reset and billing fields absent when unknown. `currentWindowId` and `weeklyWindowId` reference actual reported windows; one Codex seven-day limit can serve both roles without being duplicated.

It is the one read that also answers a plain GET, so it opens as a URL: `http://127.0.0.1:8427/claudeobs.v1.ObsService/GetUsageSnapshot?encoding=json&message={}`. `message` is the request as JSON, so a filter goes there, for example `message={"provider":"claude"}`. Both parameters are required. POST with a JSON body works as before. With `curl`, pass `-g` so it does not treat `{}` as a pattern.

`GetAccountHistory` returns allowance observations segmented by reset period/gaps and cumulative provider-reported overage. Claude observations use verifiable existing metrics. Codex history begins prospectively in quota polling, independently of UI visits and session indexing, with 90-day retention and a bounded query. Failed polls never add a fresh point for an old reading. Claude billing-period metadata is unavailable, so its period remains unknown and decreases/gaps are not converted into daily spend.

Activity and reports for an account cover its currently linked profiles. Older session records do not establish historical account ownership; the interface explicitly describes this limitation. Invalid scopes fail instead of widening to other accounts. Additional providers can supply different capabilities; Fireworks AI and Baseten adapters are not included yet.

### Codex monitoring

Codex collection combines read-only App Server metadata with supported local session records, including archived history. Collection never starts inference or resumes a thread. Account limits come from `account/rateLimits/read`; they are subscription utilization, not billed spend. The adapter detects available methods and reports partial coverage when an API or historical field is unavailable.

Codex session totals in SQLite are the authority for token activity. Overlapping local records are deduplicated; cached-input and reasoning-output counts retain their subset semantics. Current local records do not identify the billing route of individual requests, so historical outside-cap spending is unavailable. The account's current credit overage is read from its limits instead; see [Reported overage and estimated overage are separate](#reported-overage-and-estimated-overage-are-separate). A token count, API price, or exhausted-window snapshot is insufficient to classify a request as paid. Claude Insights and billed-overage attribution do not apply to Codex.

Plan limits refresh every minute independently of history indexing. The app preserves nullable included-use permission, credit balances, spending controls and reset-credit counts, without assuming credit units are dollars. Failed polls retain the last successful observation and mark it stale; snapshots older than five minutes are unconfirmed. Alerts warn at 75%, 90%, 95% and 100%, deduplicated by account (otherwise profile), window and reset period. Monitoring does not enforce provider spending limits or stop running agents.

Codex `config.toml` wiring disables prompt logging and sends supported OTel logs and metrics through the local server, which adds provider/profile attribution before forwarding to the stores. Existing exporter conflicts remain untouched. Unsupported configuration layouts are reported in Settings for manual resolution.

### Health

Health reports:

- reachability, freshness and coverage for every store this profile depends on: VictoriaMetrics, VictoriaLogs and the analysis runner
- poller freshness per source
- the session indexer's own freshness
- whether each expected metric series has a recent sample
- 24-hour log volume by event name
- each store's configured retention

### Sessions

The Sessions page lists every session `claude-obs index` has indexed, built from its `sessions` table in `state/claude-obs.db`, parsed from Claude transcripts and Codex, OMP and OpenCode history. Claude indexing is incremental: a transcript unchanged in mtime and size is not re-parsed. Rows are permanent, so a session whose transcript later ages out keeps its historical numbers with `has_transcript` flipped to false. Each session's subagent usage is merged in from its nested `<project>/<session-uuid>/subagents/agent-*.jsonl` files.

Selecting a session opens `/sessions?id=<id>`, with analysis at `&tab=analysis`. Filters and scope survive navigating back. `StartAnalysis`/`GetAnalysis` run in-process and preserve configured profile hooks and the session workspace. Claude, Codex, OMP and OpenCode sessions can be evaluated by any configured supported CLI, account and model.

Recent Claude live records are merged server-side with indexed sessions. Missing token, tool, duration or delivery records remain unknown; a partial positive total carries `+`. Period filters select sessions by their last recorded activity, while displayed totals cover the whole selected sessions. A cap-exhaustion snapshot alone never identifies paid requests.

### Findings

Findings use scoped session, log and metric evidence for tool failures, permission friction, hook latency, unused MCP connections, cache discipline and model mix. Each finding states its window and evidence; insufficient history is distinct from a clean result. Aggregate findings cover retained history, including disconnected profiles. Claude uses its scoped telemetry checks; Codex, OMP and OpenCode use indexed coverage, recorded failures and session-size observations. Large sessions are review candidates, not proof of waste.

## Advice and session analysis

Choose the evaluator **Provider**, **Account / profile** and **Model** independently from the evidence shown in the current view. Claude, Codex, OMP and OpenCode can evaluate any supported session source. The Model dropdown lists models reported by the selected CLI/account; **Profile default** uses its configured default. Catalogs load on demand without generating a response or loading local models. See the [setup guide](docs/analysis-setup.md) for authentication, directory conventions and a verification walkthrough.

| Depth | Evidence | Delegation |
|---|---|---|
| `shallow` | Scoped findings and up to 200 recent SQLite session summaries | None requested |
| `standard` | Summaries plus relevant source configuration | None requested |
| `evidence` | Standard evidence plus at most ten recent transcripts | At most four readers when the CLI supports delegation; otherwise sequential |

Shallow advice includes its summaries directly in the prompt (up to 200 sessions and 128 KiB), so it needs no file-reading tool calls. Session analysis and deeper advice require a model with working tool support. If a local model prints tool-call markup as ordinary text, use shallow advice or select a model/provider configuration that supports native tool calls.

The server removes its temporary evidence bundle after the run. It never recreates a global session JSON index. Reports state sample coverage and missing evidence, distinguish confidence from measured facts, and pair recommended changes with verification steps. Included or unclassified usage receives no dollar valuation.

Aggregate advice retains `dashboard/data/advice/`; scoped reports live under `advice/scopes/<scope-hash>/`. Source scope and evaluator metadata are persisted in SQLite. A saved report keeps its original evidence roster even when connections change. OMP/OpenCode inference-provider scopes include only sessions that used that provider, with whole-session totals for mixed-provider sessions.

The runner preserves CLI configuration and hooks. Claude denies file-edit tools, and session analysis also denies Bash; advice retains its existing Bash permissions. Codex runs with its read-only sandbox. OMP enables read/grep/glob, while OpenCode grants read/search/list operations and denies other tools. These provider-specific controls do not replace the selected CLI's own authentication and permission configuration. One shared gate serializes runs across all providers and scopes.

### Recorded report metadata

Reports retain source scope, evaluator provider/profile, run session ID where available, model, depth, timestamps and observed subagent counts. Included or unclassified execution receives no monetary valuation. Legacy reports that do not satisfy the usage-only report contract require regeneration.

### Restart the server after replacing the binary by hand

`just upgrade` and `just install` both restart every agent for you: each one is booted out and bootstrapped again, so the new binary takes effect immediately. Nothing extra is needed after either.

Copying a new `claude-obs` into the install dir yourself is different. The server agent runs under `KeepAlive`, so it keeps serving the old code until restarted:

```bash
launchctl kickstart -k gui/$UID/<LABEL_PREFIX>.server
```

Skip it and the advice button may fail against stale code even while other endpoints still answer. Check `runner_busy` before restarting, so a run in flight is not killed mid-way.

## Reading the dollar figures

The top-bar **Billed MTD** figure totals known usage charges across all accounts, including hidden accounts. It combines provider-reported overage, Oh My Pi and opencode pay-as-you-go estimates (such as Baseten and Fireworks), and Codex credit overage priced at `CODEX_CREDIT_USD`; a total with any estimate in it is marked **≈**. Hover for the account breakdown. A **+** marks missing amounts, unpriced requests, or Codex credits with no price set. This is recorded usage spend, not a consolidated provider invoice or subscription-fee total.

### Included usage has no dollar valuation

The dashboard and terminal status line never display an API-equivalent price for included or unclassified activity. Historical token-cost counters remain internal compatibility data; they are not bills. Usage and efficiency views use tokens, requests, context and elapsed time. Earlier generated reports that valued all usage need regeneration under the usage-focused report contract.

### Reported overage and estimated overage are separate

**Provider-reported overage** comes from an explicit provider financial field. Claude's `claude_extra_usage_usd` is sourced from `extra_usage.used_credits` in cents, divided by 100. This is the provider's month-to-date amount at the source observation time, not a rolling 7/30/90-day total or a final invoice. The history selector changes the visible observation range. Readings are never summed; month boundaries, decreases and collection gaps remain separate. Account totals deduplicate profiles sharing one account. See [Claude's usage-credit documentation](https://support.claude.com/en/articles/12429409-manage-usage-credits-for-paid-claude-plans).

Older single-account collectors stored these billing readings without account or profile labels. They remain in VictoriaMetrics but are excluded from account history by default. After confirming the historical owner, set `LEGACY_CLAUDE_BILLING_ACCOUNT="claude:ACCOUNT_ID"` in the installed `config.env` to that exact dashboard/API account key and restart the server (or run the normal upgrade). The setting is retained on upgrade. It makes earlier unlabelled readings visible only for the explicitly assigned account, with legacy-source provenance; account-labelled readings take precedence for the same source observation timestamp. It does not rewrite or delete stored metrics, change the current billing value, or infer ownership from the current primary profile. Leave it empty if the old collector changed accounts or ownership is uncertain.

**Codex credit overage** comes from `account/rateLimits/read`, whose `individualLimit.used` is the credits spent under the member's individual credit limit in its current period. Codex spends credits only once the included allowance is used up ([OpenAI's Codex pricing](https://learn.chatgpt.com/docs/pricing)), so this is the account's overage, reported in credits. Codex reports no dollars, and OpenAI's price per credit depends on the plan, so the dollar figure is an estimate: the credits times `CODEX_CREDIT_USD`, labelled as an estimate wherever it appears. With `CODEX_CREDIT_USD` empty, the overage shows in credits only. An account whose limit Codex does not report shows its overage as **Not provided**. Codex reports only the current period's total, so no Codex overage history is kept.

**Estimated overage** may use public API token prices only after the source establishes which requests fell outside included allowance. Such an amount must state that it is an estimate and identify its pricing source. No supported Codex record supplies that classification, so no recorded Codex request is priced. Current account limits are not applied retrospectively to historical requests.

### Exhaustion is a warning, not proof of a charge

Reaching 100% means a reported allowance is exhausted. Whether further work stops, completes within the allowance, consumes credits, or incurs a charge depends on provider rules and account controls. A reset time passing is not confirmation that included usage has resumed. Use the provider's own spending controls for enforcement; this app supplies monitoring and warnings.

## Configuration

`profiles.json` stores the profile registry, stable IDs, and owned wiring. Manage it through Settings. `config.env` holds the generated shell projection and machine-level options; it is generated from `config.env.example` by `claude-obs install`, gitignored and machine-local. A re-install merges rather than overwrites it, so a value you tune here survives an upgrade; see [Upgrading](#upgrading).

| Key | Default | Meaning |
|---|---|---|
| `OBS_DIR` | auto-detected | Absolute path to the install dir (not the checkout); do not edit. |
| `PROFILE_DIR` | empty | Generated primary enabled Claude directory; manage profiles in Settings. |
| `PROFILE_DIRS` | empty | Generated enabled Claude directories, colon separated and primary first. Empty means none. |
| `PROFILE_IDS` | empty | Stable Claude IDs aligned with `PROFILE_DIRS`; generated from the registry. |
| `CLAUDE_BIN` | detected, or empty | Absolute path to the `claude` binary; the usage probe and headless analysis runs use it. Owned by `claude-obs install`, which rewrites it on every run: it reuses the recorded value while that still works, else prefers `$HOME/.local/bin/claude`, else takes the first `claude` on `PATH`. `--claude-bin` overrides. |
| `LABEL_PREFIX` | `com.agent-observability` | launchd label prefix for the six agents. Owned by `claude-obs install`, which rewrites this key on every run whatever the file says: the ports are hardcoded, so a second install cannot coexist under a different prefix anyway. |
| `VM_URL` | `http://127.0.0.1:8428` | VictoriaMetrics endpoint. |
| `DASH_PORT` | `8427` | Dashboard port. Read by `bin/usage-alert.sh` as the base for its notification deep links. |
| `VM_VERSION` | `v1.152.0` | VictoriaMetrics release fetched into `state/bin`. |
| `VL_VERSION` | `v1.53.0` | VictoriaLogs release fetched into `state/bin`. |
| `STALE_AFTER_S` | `600` | Retained for configuration compatibility. Claude dashboard/API freshness uses a fixed 10-minute confirmation-age limit. |
| `WARN_PCTS` | `75 90 100` | Claude plan-window alert thresholds. Codex warns at 75, 90, 95 and 100%. |
| `OVERAGE_WARN_PER_HR` | `100` | Real overage burn (USD/hr) that triggers a warning. |
| `OVERAGE_CRIT_PER_HR` | `250` | Real overage burn (USD/hr) that triggers a critical alert. |
| `DAILY_BUDGET_USD` | `750` | Retained for configuration compatibility; the notional budget alert is retired. |
| `USAGE_STALE_ALERT_S` | `21600` | Alert when in overage and the billed figure is staler than this. |
| `DRY_RUN` | `0` | `1` makes alerts print instead of firing a notification. |
| `NOTIFIER_APP` | `$HOME/Applications/Agent Observability.app` | Click-to-open alert bundle built from `notifier/`. Must sit outside this checkout. Empty disables it, and alerts fall back to plain `osascript` banners. |
| `COST_METRIC` | `claude_code.cost.usage` | OTel cost counter name. |
| `TOKEN_METRIC` | `claude_code.token.usage` | OTel token counter name. |
| `CODEX_CREDIT_USD` | empty | Dollar price of one Codex credit, such as `0.04`, from the workspace's billing. Prices Codex credit overage as a labelled estimate; empty leaves it in credits. Read when the server starts. |

### Watching several profiles at once

Add each Claude or Codex directory in Settings, give it a display name, and choose the primary profile for its provider. Directories are canonicalized so aliases cannot import the same home twice. Disable or disconnect a profile to stop collecting it while retaining its history. Repair uses the same merge and backup logic as installation.

The existing explicit installer flags remain supported, for example `--profile-dir "$HOME/.claude-work" --profile-dirs "$HOME/.claude-personal:$HOME/.claude"`. These are declarations, not discovery; the installer does not suggest custom directories.

**A plan window belongs to an account, not a profile.** Two profile directories signed in to the same Claude account share one allowance, so every poller series carries both a `profile=` and an `account=` label, and the dashboard groups its meters by account. Drawing one meter per profile would present one allowance as two and read as twice the headroom. `claude_profile_account_info{profile,account,domain}` is the mapping.

The Overview page shows one meter card per account, highest first, so no account is hidden behind a control. An account with overage billing disabled says so rather than showing `$0.00`, because it cannot be billed at all: it throttles at 100% instead.

**One profile selector, in the sidebar, applies to every page.** Its state lives in `?profile=`, so it survives a refresh and can be pasted to someone, and each option carries that profile's account and its percentage: two profiles on one account show the same number under the same domain, which reads as one allowance rather than two.

What a selection narrows depends on what the figure belongs to. Cost, tokens and sessions are per profile and filter on `profile`. Plan windows, the billed figure and the overage curve belong to the account, so they resolve to the selected profile's account and say which one; splitting a bill between two profiles on one account would invent a number nothing charged. The Insights and Health views cover the whole install and say so in their headers rather than ignoring the selection quietly.

### The default profile keeps its config somewhere else

Claude Code stores the default profile's config at `~/.claude.json`, in your home directory, and only moves it inside a profile directory when `CLAUDE_CONFIG_DIR` names one. Everything here accounts for that: the poller reads `~/.claude.json` when `PROFILE_DIR` is `$HOME/.claude`, and it leaves `CLAUDE_CONFIG_DIR` unset rather than setting it to `$HOME/.claude`, which would point Claude Code at a different config file from the one being read.

`claude_poller_cache_ok` reports whether the poller found usable data: `1` when the cache exists and carries `cachedUsageUtilization`, `0` when it is missing or has never been populated. It is written on every run, so a poller that emits nothing else still says why.

Allowance freshness uses the time of the last successful confirmation: **10 minutes for Claude** and **5 minutes for Codex**. Claude's five-minute probe cadence needs room for scheduling and network delays; Codex polls every minute. The dashboard, API, and collection health use the same limits. Failed polls and missing confirmations remain explicit, and a passed reset still requires confirmation before allowance is considered restored. Any stale window, including a model-specific one, can make the account stale. Pay-as-you-go collection stays stale after 15 minutes, and Claude's billed figure after 6 hours.

`claude_plan_data_age_seconds` and `claude_plan_data_confirmed_age_seconds` answer two different questions, and only the second one is freshness. The first is how long since the figures last **changed**, which is what Claude Code's own `fetchedAtMs` records. It does not rewrite that cache when a fetch comes back identical, so a profile sitting at a steady percentage ages indefinitely while its numbers are correct to the second: one profile was measured reading 44 minutes old against a live probe and a 6 second old statusline tap that both agreed with it to the point. The second is how long since a live fetch last **confirmed** them, stamped by the poller and only by a probe that came back with usable rate limits. The Health row and the Overview badge judge on the confirmation and report the change age beside it, and fall back to the change age when nothing has ever confirmed.

`claude_profile_usage_available` answers the question that one cannot: whether the profile can fetch usage **at all**. A profile whose sign-in has lapsed leaves `cachedUsageUtilization` absent forever, which looks exactly like a profile nothing has probed yet, and a permanent zero reads as a broken poller rather than a profile nobody is signed in to. Claude Code reports it directly (`rate_limits_available`), so the poller reads that one field out of the probe response and nothing else: every number still comes from the cache afterwards. It is emitted only when a response parsed, so a probe that could not be read is not recorded as a profile that cannot report.

### The ports are fixed, and editing config.env does not move them

The dashboard and API share 8427, VictoriaMetrics listens on 8428, VictoriaLogs on 9428. All three are Go consts in `internal/server/server.go`, not read from `config.env` by `claude-obs serve`: `VM_URL` backs one flag default on the separate `claude-obs backfill` command, and `DASH_PORT` is read only by `bin/usage-alert.sh`, for the base of its notification deep links. Editing either does not move the running server's ports.

The embedded frontend needs no port config of its own, since it and the API share one origin. Two of the launchd templates, under `internal/installer/templates/`, still carry a port literal in their `ProgramArguments` (`victoriametrics.plist.tmpl`, `victorialogs.plist.tmpl`); a port change means updating the Go consts and those two templates in lockstep.

## Operations

**Stop or restart the whole stack:**
```bash
for a in victoriametrics victorialogs server limits-poller usage-alerts \
         session-indexer; do
  launchctl bootout "gui/$UID/<LABEL_PREFIX>.$a"
done
```
Bootstrap each plist from `~/Library/LaunchAgents/` to bring them back. Stopping an agent does not delete its data: that lives in `state/vmdata` and `state/vldata`.

**Pause the poller or alerts** without removing them:
```bash
launchctl bootout gui/$UID/<LABEL_PREFIX>.limits-poller
launchctl bootout gui/$UID/<LABEL_PREFIX>.usage-alerts
```
Reload with `launchctl bootstrap gui/$UID <plist path>`.

**Manual indexer pass** (launchd already runs this every 300s):
```bash
./dist/claude-obs index --once
```

**Test suites:**
```bash
just ci                             # the whole gate; what CI runs
just test                           # go vet, go test, vitest, tsc --noEmit
go test ./...                       # the internal/... packages alone
(cd frontend && npx vitest run)     # the frontend suite alone
```
`just ci` is the umbrella gate and the one to run before opening a pull request: it adds shellcheck over `bin/` and `notifier/build.sh`, a full `just build`, and an installer dry run on top of `just test`. Run it inside `nix develop` so the pinned toolchain is on PATH.

Analysis runs are ordinary Claude Code sessions (`CLAUDE_BIN` pointed at the instrumented profile): they bill like any other session and show up in this stack's own telemetry.

**Changing the cost series' labels** (`claude-obs migrate-history-labels --value <profile>`): adds a label to every existing historical cost sample by moving it through VictoriaMetrics' own export and import, so values round-trip exactly and no transcript is consulted. `--dry-run` rehearses it, `--metrics` points it at a throwaway metric name for a real rehearsal, and it writes the rewritten dump to `<install-dir>/backups` before deleting anything.

Use this rather than `backfill --reset-cost-series` whenever the history is older than about 30 days. A reset rebuilds from transcripts, Claude Code ages transcripts out, and a series that has outlived its own source is deleted and not replaced. `claude-obs backfill` now refuses `--reset-cost-series` in that case and says by how much, overridable with `--i-know-this-deletes-history`.

**Backfill** (queued automatically for fresh installs unless `--no-backfill`, or requested later in Settings): `claude-obs backfill` walks `PROFILE_DIR/projects`, including each session's nested `<project>/<session-uuid>/subagents/agent-*.jsonl` files, and imports per-request history into VictoriaMetrics as `claude_hist_*` series. It is idempotent (VictoriaMetrics dedup on identical series+timestamp means re-imports overwrite rather than double-count) and reconciles the imported total against its own parse sums rather than a hardcoded number. `--dry-run` computes and prints without importing.

**Logs:** `logs/poller.{log,err}`, `logs/alerts.{log,err}`, `logs/session-indexer.log`, `logs/victoriametrics.log`, `logs/victorialogs.log`, `logs/server.log` (the server agent's log now covers the embedded frontend, the API, and in-process analysis runs together; there is no separate analysis-runner log any more).

## Known limitations

- macOS only (launchd).
- Codex historical coverage depends on available App Server methods and surviving local records. Missing token detail and unknown billing classification remain explicit; historical outside-cap spending cannot currently be reconstructed.
- Existing incompatible telemetry exporters and inline/dotted Codex `otel` layouts require manual resolution in the agent configuration before repair can connect them.
- One analysis runs at a time: the runner holds a single lock, so a second `StartAnalysis` while it is busy gets an error and the pane's analyse button is disabled until the first run finishes. `StartAdvice` shares the same lock, and its evidence depth can hold it for many minutes.
- The evidence depth's transcript fan-out is capped at ten sessions by the prompt itself, and the run gets its own longer timeout separate from the other depths'. Both caps mean it can cover less than the full session history on a heavily used profile, so its "about $20 to $40" is an estimate, not a guarantee of completeness.
- Claude Code's own transcripts age out after roughly 30 days locally, but indexed session rows are permanent: once a transcript ages out, `has_transcript` flips to false and the analyse button disappears, but the historical numbers stay.
- `used_credits` (the real-dollar figure) is backed by a cache that refreshes rarely; if the dashboard's overage number looks stale, run `/usage` to force a refresh.
- VictoriaLogs and the poller series only start at install, so most Findings page rules report "not enough history yet" on a fresh install and fill in over the following days. The session index covers all history on disk and its findings work immediately.
- Session-level real money attribution is estimated from the trajectory, which the indexer downsamples to 300 points, against a `claude_in_overage` series sampled every minute. It is accurate to roughly a ten minute band, and shows nothing at all for sessions that ended before the poller was installed.
- Each install is its own stack: nothing is centralized across machines, and there is no multi-host view.
- The notifier bundle lives in `~/Applications`, outside the checkout, because macOS notification authorization requires an installed application. Notification permission is requested interactively when the bundle is first registered.

## Privacy

Claude Code's OTel export can include account emails and, with `OTEL_LOG_TOOL_DETAILS=1`, tool inputs in the local logs. Codex wiring sets `log_user_prompt = false`; imported session summaries do not export message bodies. The dashboard and stores bind to loopback. Live limit reads use each agent's existing authenticated connection, and optional analysis sends its requested evidence to the selected evaluator’s configured endpoint, so these agent operations can contact the provider.

## Full uninstall

```bash
claude-obs uninstall            # remove the agents; keep data
claude-obs uninstall --purge    # also delete store data and downloaded store binaries
brew uninstall agent-observability   # then, for a Homebrew install, the command itself
```

From a source checkout, run `./dist/claude-obs` in place of `claude-obs`.

Uninstall uses the registry ownership records to remove only matching Claude JSON and Codex TOML wiring, with private `.agent-observability.bak.<unixnano>` backups before changes. User-edited values and unrelated configuration are preserved; an edited managed Codex block reports a conflict instead of discarding it. Profiles remain disabled in the registry, and backups and `state/` remain in place. Without `--purge`, this includes vmdata, vldata and the store binaries; the repository is never removed.

`claude-obs uninstall` also removes `~/Applications/Agent Observability.app` (or whatever path `NOTIFIER_APP` names). Its entry under System Settings > Notifications survives, because macOS offers no way to remove one programmatically; delete it there by hand if you want it gone.

## Releasing

Push a tag such as `v1.2.3` on `main`. The release workflow (`.github/workflows/release.yml`) runs the same gate as CI, builds one universal `claude-obs` for Apple Silicon and Intel Macs with `just release`, publishes its tarball and checksum as a GitHub release, and updates the formula in [heapsgoodstudio/homebrew-tap](https://github.com/heapsgoodstudio/homebrew-tap) from `packaging/homebrew/agent-observability.rb.tmpl`. The `TAP_DEPLOY_KEY` repository secret is the private half of an SSH deploy key that can push to the tap and nothing else.

## Licence

MIT. See [LICENSE](LICENSE).
