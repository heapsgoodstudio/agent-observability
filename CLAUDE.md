# Agent Observability

Local session and cost telemetry for Claude Code, Codex, Oh My Pi and opencode. macOS only (launchd). README.md covers architecture, data semantics, and configuration.

This repository is self-contained: its own Go module (`github.com/heapsgoodstudio/agent-observability`), nix devshell, justfile and gate. Run every command from the repository root.

## Verify before claiming done

`just ci` is the umbrella gate; run it inside `nix develop` (or let CI do
it). It runs, in order:

    shellcheck bin/statusline.sh bin/statusline-tap.sh bin/usage-alert.sh \
              bin/fetch-stores.sh bin/limits-poller.sh notifier/build.sh
    go vet ./...
    go test ./...
    (cd frontend && npx vitest run)
    (cd frontend && npx tsc --noEmit)                # typechecks the whole
                                                      # frontend (app, src, lib);
                                                      # Next's own build-time
                                                      # check only covers files
                                                      # its page graph reaches
    just build                                       # buf generate, frontend
                                                      # static export, go build
    ./dist/claude-obs install --dry-run --yes          # prints actions, changes nothing

The notifier's click path has no automated test and cannot have one. Verify it
by hand: `./notifier/build.sh`, then post a banner and click it.

    "$HOME/Applications/Agent Observability.app/Contents/MacOS/AgentObsNotifier" \
        "Test" "click me" "http://localhost:8427/#overage"

Changing the icon needs one more step, and it is not optional. macOS caches the
rendered icon in an IconServices store that a rebuild does not invalidate, so
banners keep showing the previous mark. Rebuilding, `lsregister -f -R`, bumping
`CFBundleVersion`, restarting `usernoted`, and even deleting the bundle and
re-requesting authorization all leave it stale; only removing the store does.
Finder and `NSWorkspace` read a different path and show the new icon throughout,
so they cannot be used to confirm the change.

    rm -rf "$(getconf DARWIN_USER_CACHE_DIR)com.apple.iconservices" \
           "$(getconf DARWIN_USER_CACHE_DIR)com.apple.iconservicesagent"
    killall iconservicesagent usernoted NotificationCenter

Then post a banner and look at it. `screencapture` works for this: post, capture
a few frames, and crop the top right corner.

## Hard constraints

- **Included or unclassified usage has no displayed monetary value.** Dashboard, statusline, notifications, findings and generated reports must use tokens/requests/allowance percentages for that usage. Show dollars only for provider-reported overage, separately labelled API-price estimates of explicitly identified outside-cap requests, or Codex credit overage priced at `CODEX_CREDIT_USD`. Existing cost counters and SQLite costs are compatibility data, not bills. Codex history has no request-level billing-route evidence, so no recorded Codex request is priced. Codex overage is the account's credit use instead: `account/rateLimits/read` reports `individualLimit.used`, the credits spent under the member's individual credit limit in its current period, and Codex spends credits only once the included allowance is used up (OpenAI's Codex pricing page). That figure is provider-reported and in credits, never dollars. `CODEX_CREDIT_USD` (empty by default, because OpenAI's price per credit depends on the plan) turns it into a dollar estimate that is always labelled as one. Checked 2026-10-20: the Credits view of Codex's `/usage` showed credits on exactly the days the weekly allowance sat at 100%, and its current-month total matched `individualLimit.used`. Never use a current cap, a 100% snapshot, or a passed reset time to infer paid requests or restored permission. Schema-2 reports omit dollar figures; legacy reports require regeneration.
- **Pay-as-you-go harness dollars are shown, and always as that harness's estimate.** Oh My Pi (`omp`, `~/.omp/agent`) and opencode (`~/.local/share/opencode`) send requests to pay-as-you-go inference providers such as Baseten and Fireworks, so no plan includes that usage and the rule above does not hide it. Each request's cost is the harness's own: provider-reported tokens times its catalog rates, never a bill. `internal/omp` reads Oh My Pi's session logs, where a missing `cost` stays unknown rather than $0 and a request copied into a fork is counted once by `responseId`. `internal/opencode` reads opencode's sqlite read-only; opencode records $0 for a model it cannot price, so `$0` is read per provider (free if the provider was never priced, unknown if it was), and its output tokens are output plus reasoning. Both produce `internal/payg` buckets, stored at `<provider>.snapshot.<profile>` (`monitor.SpendSnapshotKey`; the omp form is the key the first release wrote), which `payg.Merge` only ever grows, because deleted data cannot un-spend money. `docs/omp-provider.md` and `docs/opencode-provider.md` have the formats.
- **Codex quota polling is independent of history indexing.** One-minute reads persist under `codex.quota.<profile>`; history stays under `codex.snapshot.<profile>`. Readers use `monitor.ReadSnapshot`. Failed polls retain observation time, snapshots older than five minutes are stale, and explicit false/zero fields remain distinct from missing fields. Alerts deduplicate shared accounts and never claim that cap exhaustion proves billing.

- **Nothing runs from this checkout, but on a machine with the stack installed it IS live, elsewhere.** Building (`just build`) and testing (`just ci`) run real code, but that is verification, not a live install. Six `com.agent-observability.*` launchd agents run this tool's Go binary and embedded frontend out of the install dir (`~/.agent-observability` by default), scoped to the enabled profiles in `profiles.json`; `config.env` contains the derived Claude shell projection. Never conclude from "pure source" that nothing is running: a bug committed here reaches that install the moment the dir is synced, and on 2026-08-17 that reasoning briefly made a live outage (the poller had shipped no metrics for a day) look impossible. Check `launchctl list | grep claude` and the install dir, not this file's framing.
  - **The install dir's store data can predate it.** `state/vmdata` and `state/vldata` may both predate the install dir's own mtime, so VictoriaMetrics can hold series older than anything this repo wrote. That bit once: rollups stamped an hour off `noonMs`'s true local noon meant a backfill added a second sample per day instead of overwriting, and 26 overlapping days were counted twice until the series were deleted and re-imported.
  - **Replacing `<install-dir>/bin/claude-obs` must land on a new inode.** Truncating it in place reuses the inode, invalidates the code signature of the image the running server agent still has mapped, and macOS SIGKILLs the *next* exec (exit 137, no output). The running server keeps its own mapped image and is unaffected either way, so the damage only shows when something next launches the binary. Reproduced 2026-08-17: with a server running from the file, an in-place overwrite succeeds and the next exec returns 137, while both `rm` then `cp` and a rename return 0.
    - `installer.writeFileAtomic` is what makes `claude-obs install` safe to re-run: it writes a temp file beside the target and renames it over. Rename rather than remove-then-write, because it is atomic and never leaves launchd a window in which `bin/claude-obs` is missing to restart an agent into. `stepCopy` and `CopyTree` both go through it, so the copied shell scripts get the same treatment: bash reads a script incrementally, and the poller and alert agents may be mid-run when an install replaces one. `TestStepCopyReplacesTheBinaryOnANewInode` pins it, and fails against the old `os.WriteFile`.
    - Doing it **by hand still needs `rm` then `cp`**, since a bare `cp` truncates in place.
- **An upgrade must retain configured profiles, never rediscover them.** `profiles.json` is the profile authority. `internal/profiles.Store.Initialize` migrates legacy `PROFILE_DIR`/`PROFILE_DIRS` once, retaining the exact Claude roster, stable labels, and primary, and adding an existing default `~/.codex`. This detection only runs before `profiles.json` exists; registry upgrades never rediscover removed or disabled profiles. Fresh detection checks exactly `~/.claude` and `~/.codex`; it must not scan custom directories or use environment overrides for discovery. Custom paths are manually declared in Settings. `--upgrade` implies `--yes`, requires an existing readable install, and retains disabled/custom profiles. Explicit existing CLI overrides still win. Installation with no profile directory or executable is valid and asks no profile/backfill questions.
- **Registry updates must preserve identity and owned configuration.** Display names are separate from IDs. Migration retains Claude telemetry IDs; the default Codex ID is `codex-default`. Canonical paths deduplicate symlink aliases, and disconnected identity reservations prevent an unrelated new directory inheriting old history. Settings and installation share `internal/profiles` JSON/TOML validation, ownership tracking, private backups, atomic writes, locking, and transaction recovery. Unrelated configuration and external telemetry destinations must survive repair/disconnect/uninstall. Disable and disconnect never delete historical sessions or store samples. Do not bypass the registry lock when rewriting the shell projection.
- **The registry roster is independent of telemetry availability.** Load it on each request/collection. Empty is explicit; unavailable directories and wiring conflicts remain visible in Settings. Settings mutations require local-origin/Host protection. Provider identity persists in sessions and must participate in filtering; unknown pricing or unsupported Codex fields must not be presented as measured zero.
- **History import is background work.** The installer queues `state/import-request.json` before agents start, except with `--no-backfill`; `--backfill` explicitly queues imports on upgrades. `internal/monitor` owns collection scheduling, durable queue/status, and Codex snapshots. A daemon lease prevents the timed CLI fallback from repeating the server's collection; a separate indexing lock serializes actual passes. Queued work must survive interruption and removed/disabled profiles must not block the queue. Never block installation on a transcript/index pass.

- **`config.env` may repeat keys**, from the example and an appended machine-local block, so the value that counts is the **last** assignment, including an explicitly empty one; `config.Load` matches bash on this and `internal/config/config_test.go`'s `TestLoadTakesTheLastAssignment` pins it. Reading an earlier assignment or discarding an empty value can silently restore an unwanted profile. Profile assignments are regenerated from the registry.
- **`config.env` is merged on re-install, not rebuilt.** `MergeConfigEnv` (`internal/installer/configenv.go`) keeps the value an install already has for any key the example ships, adds keys the example has gained, and carries keys the operator added that the example never lists (`bin/limits-poller.sh` reads several, such as `POLLER_TAP_FILE`). Rebuilding from the example, which is what it used to do, silently reverted tuned budgets and alert thresholds on every upgrade. Three things make it behave: `machineOwned` (`OBS_DIR`, `PROFILE_DIR`, `PROFILE_DIRS`, `PROFILE_IDS`, `PROFILES_REGISTRY`, `CLAUDE_BIN`, `LABEL_PREFIX`) is never carried over, because the appended block owns those and `OBS_DIR` must stay a `$(...)` expression; comparison is by **whole line**, not parsed value, so an untouched `$HOME/...` line is not rewritten to an expanded path on first upgrade; and since nothing records whether a difference is the operator's edit or a default we changed, the operator wins and the key is returned in `kept` so the installer says so out loud. Adding a key to `config.env.example` needs no merge work, but **renaming or removing one drops the operator's value silently**, so treat a rename as a migration.
- **Scripts refuse to run from the checkout, and new writers must too.** Every script in `bin/` resolves its own root as the parent of its directory (`OBS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"`). That is right for the copy `claude-obs install` places in the install dir and wrong for the copy that stays here, where it would write `state/`, `logs/` and `config.env` into the repository and keep doing so. That is not hypothetical: 89 MB of a dead v1 install (store binaries, vmdata, vldata) sat in this tree until 2026-08-17.
  - **`go.mod` is the marker**, because it is tracked here and the installer never copies it: `stepCopy` takes `bin/`, `notifier/` and `config.env.example`, and `stepLayout` makes only empty directories. A guard keyed on anything the installer *does* place in the install dir would fire on real installs.
  - **Those three are embedded in the binary, not read from a checkout.** `assets.go` (the module's root package) embeds them as `agentobservability.Assets`, and `stepCopy` places them from there, so a Homebrew install, which has only the binary, installs the same files as a source build. A file added under `bin/` or `notifier/` ships with no other change. Embedding drops file modes, so `CopyTree` installs every `.sh` file executable and everything else 0644; a new executable must end in `.sh`. `TestStepCopyInstallsTheEmbeddedFiles` pins it.
  - **Four scripts guard, and any new script that writes must join them**: `fetch-stores.sh`, `usage-alert.sh` and `limits-poller.sh` refuse loudly on stderr and `exit 1`; `statusline-tap.sh` refuses **silently** with `exit 0`, because it is forked on every statusline render and stderr from a statusline command lands in the user's terminal. `statusline.sh` is deliberately unguarded: it only reads, and the tap it forks guards itself.
  - **The Go side guards too**, through `config.CheckInstallDir`, called by `install` (before the dry run, since installing here is what writes the `config.env` that arms every script), `serve` (it opens the sqlite db and hands `dashboard/data` to the analysis runner) and `index` (only when `--install-dir` is what fills the blanks; passing all of `--projects-root`, `--out` and `--db` stays an escape hatch). `internal/config/config_test.go`'s `TestCheckInstallDirRefusesTheSourceCheckout` pins it against a synthetic install root, a scratch dir, and this repository's real root.
- **Dependencies and build steps are both fine.** Go dependencies
  (`go.mod`) are allowed, and the frontend is a real npm build:
  `frontend/package.json` (Next.js App Router, TypeScript, Tailwind,
  vitest), static-exported and embedded into the binary via `go:embed`.
- **`/usr/bin/jq` is hardcoded**, in `bin/statusline.sh` and
  `bin/statusline-tap.sh` (wired into Claude Code via `settings.json`'s
  `statusLine` key, not launchd; every other script that calls `jq` resolves
  it off `PATH`). Do not swap in a Homebrew or venv `jq` there.
  `.github/workflows/ci.yml`'s CI preflight asserts that exact path for
  the same reason.

## Coupled edits

- **A release is a pushed `v1.2.3` tag, and its names are spelled in three places.** `.github/workflows/release.yml` runs `just ci`, then `just release` (`packaging/release.sh`), publishes the tarballs, and runs `packaging/update-tap.sh`, which renders `packaging/homebrew/agent-observability.rb.tmpl` into heapsgoodstudio/homebrew-tap. The tarball name `agent-observability-<version>-macos.tar.gz` appears in `release.sh`, in `update-tap.sh`'s checksum lookup and in the formula's URL; renaming it means changing all three. It holds one universal binary (arm64 and amd64 joined by `lipo`, then ad-hoc signed, because Go signs only arm64) because Homebrew's audit refuses a `url` inside `on_arm`/`on_intel`. The release binary's version is the tag, so `bin/check-upgrade.sh` resolves an installed `v1.2.3` as that tag, and source builds use `git describe --tags`. Both architectures build on one Mac because the sqlite driver is pure Go; a cgo dependency would end that. The formula's caveats and the README's Homebrew steps say the same thing and must agree.
- **Launchd templates live in the Go binary.** They live in
  `internal/installer/templates/` and are embedded via `go:embed`, not read
  from a `launchd/` directory at install time (that directory is gone). The
  label prefix is `com.agent-observability`. An agent suffix is spelled in
  five places that must agree: the `Agents` slice in
  `internal/installer/launchd.go`, the template filename, the `Label` key
  inside that template, and `UninstallSteps` / `Doctor`, which both iterate
  the same `Agents` slice; renaming or removing an agent means updating
  every one. There are six.
- **Sessions live in SQLite, and nowhere else.** `internal/indexer` (run
  through `claude-obs index --once`, on the `session-indexer` launchd agent)
  writes only the `sessions` and `transcript_state` SQLite tables;
  `internal/indexer/index_test.go`'s `TestNoJSONWritten` pins that no session
  JSON gets written at all. `internal/server`'s RPCs (`ListSessions`,
  `GetSession`, and the session facts `GetSpend`/`GetFindings` build from)
  all read those tables directly (`db.ListSessions`/`db.GetSession`), never a
  JSON projection; `GetOverview` never touches the sessions table at all, so
  it is not part of this group. `internal/pricing/prices.go` is the only
  pricing table in the repo, so a price change lands in one place.
- **Session analysis uses the same SQLite index.** `analysis.StartAnalysis`
  checks the database row and embeds its activity facts in the prompt. It must
  not require `dashboard/data/sessions/<id>.json`: the indexer no longer writes
  those files. Legacy JSON is supported only when a runner explicitly has no
  database, never as a fallback for a missing row or database error. Prompt
  facts omit compatibility cost fields, including trajectory valuations.
- **Analysis keeps profile hooks active and uses the session's workspace.**
  Pick the most-mentioned surviving absolute `cwd` from its transcript; use
  the configured working directory only when no recorded directory survives.
  Never disable a hook to make a run succeed. Claude can exit zero after a hook
  blocks the prompt, so recognise that diagnostic before treating stdout as a
  report. Do not publish the echoed prompt as report prose or claim it is an
  old report. Existing saved hook failures are exposed as errors on API reads.
- **Ports** 8427 dash+API / 8428 VM / 9428 VL. Analysis has no port of its
  own; it runs in-process inside `claude-obs serve` on 8427. All three are
  Go consts in one place, `internal/server/server.go`'s
  `DashPort`/`VMPort`/`VLPort`; changing one means editing that block and
  the matching launchd template's hardcoded port literal
  (`internal/installer/templates/victoriametrics.plist.tmpl`'s
  `-httpListenAddr`, `victorialogs.plist.tmpl`'s equivalent;
  `server.plist.tmpl` carries no port literal at all, since `DashPort` only
  backs the `--addr` flag's default). The frontend needs no port config:
  `frontend/src/lib/client.ts` builds its Connect transport from
  `window.location.origin`, since the embedded frontend and the API share
  one origin. `config.env`'s `VM_URL` and `DASH_PORT` are not read by
  `claude-obs serve` at all: `VM_URL` backs one flag default on the separate
  `claude-obs backfill` command (`cmd/claude-obs/backfill.go`'s
  `cfg.Get("VM_URL", ...)`), and `DASH_PORT` is read only by
  `bin/usage-alert.sh`, as the base for its notification deep links. Editing
  either does not move the running server's ports.
- **The default profile is the one profile whose config is not inside it, and that rule is implemented twice.** Claude Code keeps the default profile's config at `~/.claude.json`, in `$HOME`, and only moves it to `<dir>/.claude.json` when `CLAUDE_CONFIG_DIR` names `<dir>`. So `$PROFILE_DIR/.claude.json` is wrong for `$HOME/.claude`, which is the default directory checked during fresh installation. Before this was handled, an install against the documented default emitted **zero** plan-window metrics and said nothing: measured 2026-09-21 as 0 metrics against 11 for a named profile, same config, same run. The two copies are `is_default_profile` in `bin/limits-poller.sh` and `IsDefaultProfile` in `internal/analysis/runner.go`, and they must agree. Each governs two things: which file to read as the cache, and whether to set `CLAUDE_CONFIG_DIR` on a spawned `claude` at all. **Setting it to `$HOME/.claude` is not the same as leaving it unset**: it points Claude Code at a second config file beside the real one, so a probe would refresh a cache nothing reads. `internal/analysis/runner_test.go`'s `TestSpawnEnvUnsetsConfigDirForTheDefaultProfile` pins the Go half; the shell half has no harness and is verified by hand in a scratch install dir.
  - **`claude_poller_cache_ok` is what makes that class of failure visible**, since a poller with nothing to read and a poller with nothing to report both emit nothing. It is `0` when the cache file is missing or holds no `cachedUsageUtilization` yet, `1` otherwise, and unlike `claude_usage_probe_ok` it is emitted on **every** run, including runs that emit nothing else. A missing file also warns on stderr, because that one does not resolve itself; an absent `cachedUsageUtilization` does not, because the probe populates it.
  - **Two ages, and only one of them is freshness.** `claude_plan_data_age_seconds` is how long since the figures last **changed**, because that is what `fetchedAtMs` records and Claude Code does not rewrite `cachedUsageUtilization` when a fetch comes back identical. `claude_plan_data_confirmed_age_seconds` is how long since a live fetch last **confirmed** them, stamped into `state/usage-confirmed.<profile>` by the poller and only by a probe that came back with usable rate limits. Measured 2026-09-21: one profile read 44 minutes old while a live probe and a 6 second old statusline tap both agreed with its figures to the point, so the 300s stale rule was calling a working poller stale, and the quieter the profile the worse it got. `GetHealth`'s cache row and `freshnessPill` both resolve their verdict on the confirmation and report the change age beside it; both fall back to the change age when nothing has ever confirmed, which is what a signed-out profile and a `USAGE_PROBE=0` install report. **`PickFreshest` must keep using the change age**: between two sources, the one whose value moved more recently is the one to believe, and that is a different question. `internal/plan/accounts_test.go`'s `TestLoadAccountWindowsKeepsConfirmationApartFromChange` pins the pair apart.
  - **`claude_profile_usage_available` is what distinguishes a permanent zero from a pending one**, and it is the only thing that reads the probe's response. `claude_poller_cache_ok` is 0 both for a profile nothing has probed yet and for a profile whose sign-in has lapsed, and those resolve differently: the first clears itself on the next probe, the second never does. Claude Code answers directly, `rate_limits_available: false` alongside `subscription_type: null`, so the poller reads **that field and nothing else** out of the response and every number still comes from `cachedUsageUtilization` afterwards. Measured 2026-09-21 on the default profile: probe exits 0, answers false, and the login keychain's unsuffixed `Claude Code-credentials` entry holds an empty access token and a refresh token expired around 2026-08-16. The same probe against two signed-in named profiles answers `max`/`team` and true, which is the control that makes the false meaningful. The metric is emitted only when a response parsed: a probe that could not be read must not be recorded as a profile that cannot report.
- **`CLAUDE_BIN` is detected, not carried over, so detection must not trust `PATH` first.** The key is `machineOwned`, so every install rewrites it. `detectClaude` (`cmd/claude-obs/install.go`) therefore tries `$HOME/.local/bin/claude` **before** `lookPath`, and `install` prefers the value the existing install already records whenever it still validates, falling through to detection when it does not so a moved binary self-heals. `--claude-bin` overrides both. This is not hypothetical tidying: on 2026-09-21 an upgrade recorded a PATH wrapper that refuses out-of-scope working directories, and every usage probe exited 3 with the billed figure frozen and nothing logged. **`ValidateClaudeBin` cannot be the defence** and should not be made to try: such a wrapper is absolute, executable, and answers `--version`, because passing `--version` through is exactly what a wrapper does. Rejecting shell scripts is also wrong, since `claude` is a native binary in some installs and a script in others.
  - **The poller's plist pins `WorkingDirectory`** to the install dir for the same incident: launchd gives a job without it `/`, which was what put the probe out of scope. `internal/installer/launchd_test.go`'s `TestPollerPlistPinsAWorkingDirectory` pins it.
- **A profile is not an account, and the allowance belongs to the account.** Several profile directories can be signed in to one Claude account and share one set of plan windows: on the machine this was built against, a named profile directory and `~/.claude` are both signed in to the same account. So every poller series carries **both** `profile=` and `account=`, and anything about an allowance (windows, reset times, billed dollars, overage) groups by `account`. Grouping those by profile draws one meter per profile and reads as that many separate allowances. `claude_profile_account_info{profile,account,domain}` is the join: a standard `_info` gauge, value 1, facts in the labels, so nothing downstream has to parse a uuid or guess an account from a domain.
  - **The per-account queries must keep `account` in the grouping.** `internal/plan`'s flat `pctQuery` reduces with `max by (window,source)`, which across several accounts answers "which account is highest" rather than "what is this account at": one account at 95% would be reported as every account's figure. `pctByAccountQuery` and its two siblings exist for that reason, and `internal/plan/accounts_test.go`'s `TestLoadAccountWindowsKeepsAccountsApart` pins it with a 95/5 pair. The same applies to the billing queries in `internal/server/accounts.go`.
  - **A series with no `account` label is not an account.** `LoadAccountWindows` and `loadAccounts` both drop them. Every upgrade leaves the previous poller's unlabelled samples inside the next query's lookbehind, and grouping those produced a nameless fifth "account" on a four account machine, carrying a copy of the primary account's percentage and its billed figure: the same dollars twice, one row unable to say whose. Observed live on 2026-09-21. It ages out within the lookbehind, which is exactly why it is dropped in code rather than waited out: a bug that disappears before it can be investigated is one that gets rediscovered on every upgrade. `internal/plan/accounts_test.go`'s `TestLoadAccountWindowsDropsUnlabelledSeries` pins it.
  - **The flat fields still describe one account.** `GetOverviewResponse`'s `windows`, `in_overage` and `source_ages` describe `primary_account`, which is the account of the current registry primary Claude ID (`Options.PrimaryProfile`, refreshed by the server options reader). That is what keeps a single-profile install reading exactly as it did.
  - **`$0.00` is the wrong answer for an account that cannot be billed.** Three of the four accounts measured had `extra_usage.is_enabled` false: they throttle at 100% rather than charging. `AccountPlan.overage_enabled` carries that, and `frontend/src/lib/accounts.ts`'s `billedCaption` says it in words, because a zero reads as "spent nothing" rather than "cannot spend". Same rule as the delivery counters: blank is not zero.
- **Stable profile IDs are the join key.** Claude `OTEL_RESOURCE_ATTRIBUTES`, tap filenames, poller series, statusline segment filenames, and indexed session attribution must use the same registry ID. `PROFILE_IDS` aligns with the generated `PROFILE_DIRS` list. `statusline-tap.sh` and `statusline.sh` canonicalize the current `CLAUDE_CONFIG_DIR` (or `~/.claude`) and resolve its ID from `profiles.json` using `/usr/bin/jq`. Basename derivation remains only the legacy fallback; editing the name or directory must not relabel history.
- **Empty profile projections must stay empty.** `PROFILE_DIR` is the primary enabled Claude directory; `PROFILE_DIRS` is the full enabled Claude list. The shell uses `${PROFILE_DIRS-${PROFILE_DIR-}}` and `ConfiguredProfiles` uses `Config.Lookup`, so fallback happens only when the list key is absent, never when it is explicitly empty. `doctor` and `uninstall` read the registry for both providers.
- **Telemetry keys have one authority.** `profiles.ClaudeTelemetry` supplies the historical ten-key set, with the installer helper delegating to it. Ownership metadata records the values this installation inserted or adopted during legacy migration; uninstall/disconnect removes only owned values that still match. Codex merges an isolated owned TOML block, preserves unrelated tables/comments and custom exporters, disables prompt logging, and routes logs/metrics through `/ingest/codex/<id>/v1/{logs,metrics}` for stable attribution. The normalized session index remains the authority for Codex cost/token totals, so live OTel and imports must not be added together.
- **`VM_VERSION` and `VL_VERSION` in `config.env.example`** are consumed only
  by `bin/fetch-stores.sh`. A version bump means the pinned tag must already
  exist as a `darwin-arm64` and `darwin-amd64` release asset, each with a
  matching `_checksums.txt`, or the fetch fails closed.
- **The icon geometry lives in one place.** `notifier/icon.swift`'s
  CoreGraphics drawing is the only copy. The frontend has no favicon or app
  icon of its own, a real gap; whoever gives it one should match this
  geometry rather than re-deriving it, remembering SVG y grows downward while
  CoreGraphics y grows upward. The notifier
  bundle still cannot live in this checkout: macOS refuses notification
  authorization to bundles inside it, so `NOTIFIER_APP` must stay an
  absolute path outside the repo.
- **Alert deep links preserve account scope.** `bin/usage-alert.sh` posts
  `/?provider=claude&accountKey=...#now` and the same account with `#overage`.
  Older `/#now`, `/#overage` and `/#history` links still resolve through the `ALIASES` map in
  `frontend/src/lib/aliases.ts`: `#now` and `#overage` to Overview,
  `#history` to Spend, each with a scroll target. Removing an alias sends an
  alert click to a blank page, and no automated test catches it because the
  link lives in shell; `frontend/src/lib/aliases.test.ts` (13 tests) covers
  the map itself. The root page
  (`frontend/src/app/(pages)/page.tsx`) resolves the hash via
  `resolveAlias` and redirects to the aliased route with `#<scrollTo>`
  in the URL; `frontend/src/lib/scrollTarget.ts`'s `useScrollTarget` (called
  on mount by every page that owns a scroll target) then reads that hash
  and scrolls to the matching element id. Each alias's `scrollTo` value must
  therefore name a section id that actually exists in the relevant page
  component (today: `Overview.tsx`'s `Section id="meters"` and
  `id="curve"` for `#now`/`#overage`, `Spend.tsx`'s `Section id="daily"`
  for `#history`); an id that does not exist just makes the scroll a no-op,
  silently. `frontend/src/lib/aliases.scroll-targets.test.ts` covers this,
  checking each non-null `scrollTo` against the owning page component's
  source text. Since the Linear redesign merged Findings and Advice into
  one Insights view, `#insights`/`#findings` resolve to `/insights` and
  `#advice` to `/insights?tab=advice` (`PAGE_ROUTE` keeps `findings` and
  `advice` as internal keys); `/findings` and `/advice` remain as
  client-side redirects, `Findings.tsx` and `Advice.tsx` render as the
  Insights tabs, and the sidebar findings badge lives on the Insights nav
  item while Health sits at the sidebar bottom as a status row rather than
  a nav group.
- **The prompt frames live in one place: `internal/analysis/prompts/`.**
  They are embedded into the binary, so a run cannot pick up a half-edited
  frame. Both frames pull in one shared fragment, `report-contract.md`, which
  holds the reader rule, the report schema and the output discipline; that is
  what keeps the analyse and advise reports in one voice rather than two.
  The substitution mechanics matter, and prompt assembly is now two pure
  functions so they can be tested: `analysisPrompt` and `advicePrompt`
  (`internal/analysis/runner.go`) fill `{REPORT_CONTRACT}` (and, for advice,
  the chosen depth's `{DEPTH_BRIEF}`) before resolving the frame's own
  placeholders (`{SESSION_ID}`, `{TRANSCRIPT_PATH}`, `{DETAIL_JSON_PATH}`,
  `{HEADLINE}`; `{FINDINGS_JSON}`, `{INDEX_PATH}`, `{PROFILE_DIR}`,
  `{SESSIONS_DIR}`), so a fragment may itself contain a `{PROFILE_DIR}`-style
  placeholder and still resolve; a new placeholder added to a frame needs
  substituting in both functions, and for every depth.
  `internal/analysis/prompt_test.go` is the guard: it greps every built prompt
  for a surviving `{PLACEHOLDER}` at all three depths, since one that ships
  reaches the model as literal braces and the run still costs money.
- **The report schema is spelled in three places that must agree.**
  `internal/analysis/prompts/report-contract.md` (what the model is asked
  for), `internal/report`'s structs (what is parsed and normalized), and the
  proto `Report` message (what the frontend reads). Adding or renaming a field
  means all three plus `buf generate`. One rename is deliberate and permanent:
  the contract's `do` field travels as `do_this` on the wire, because `do` is
  a keyword in the generated Go. `internal/report` is where a model's
  disobedience is absorbed: a band outside its set is blanked rather than
  rejected, lists are truncated to their caps, shares are clamped, and only
  three things are fatal (no JSON object, wrong schema number, missing
  headline or summary).
- **The analyse path permits `Agent`, and only the prompt bounds it.** `AnalysisDisallowedTools` is `Write,Edit,NotebookEdit,Bash`, and `--allowedTools` permits rather than restricts, so an analysis run can fan out to readers even though `Read,Grep,Glob` is all the allow list names. That is deliberate and it earns its keep: a measured sonnet run on 2026-08-17 dispatched 4 readers (one `Explore`, three `general-purpose`) over an 18 MB transcript, read all of it in thirds, and cost **$5.02** (5.57M cache-read tokens, 509k cache creation, 92k output), which is 0.2% of the cost of the session it explained. The findings that only full coverage produced are the reason not to force single-pass. The cap therefore lives in `prompts/analysis-prompt.md` as text (at most four readers, no nesting, state real coverage), pinned by `internal/analysis/prompt_test.go`'s `TestAnalysisPromptBoundsItsFanOut`, because nothing in Go counts subagents on this path.
  - **Measuring one run's cost needs the `session.id` label, not a time
    window.** Several Claude sessions share this checkout, so a
    `sum(increase(...[25m]))` over `claude_code.cost.usage` mixes them. Doing
    exactly that on 2026-08-17 attributed a concurrent session's opus work to
    the analysis run and reported $46.28 for a run that cost $5.02, a 9x error
    in the direction that would have driven a wrong prompt change. The analyse
    path does not pass `--session-id` (only advice does), so the run's own id has
    to be recovered from the metric:
    `sum by ("session.id") (increase({__name__="claude_code.cost.usage"}[30m]))`,
    quoted that way because the label name contains a dot.
- **The advice run's allow list, deny list and timeout are all per depth.**
  The two lists are `AdviceAllowedTools` and
  `AdviceDisallowedTools` in `internal/analysis/runner.go`; the timeout is
  `DefaultEvidenceTimeout` for
  `evidence` and `DefaultAnalysisTimeout` for the rest. `--allowedTools`
  permits rather than restricts, so the deny list is the only real
  constraint, and it is where the tier boundary lives: `shallow` and
  `standard` are denied `Agent` because they are single-pass by definition,
  and `evidence` is not because fanning out is why it exists. `Bash` is
  allowed at every depth and the mutating tools are denied at every depth. A
  run is therefore trusted, not sandboxed: it holds the instrumented
  profile's own permissions and has a shell. The measurements behind those
  choices are in the comment above the constants; read it before tightening
  them.
- **The analysis runner dual-writes file and db.** All four analysis RPCs
  (`StartAnalysis`, `GetAnalysis`, `StartAdvice`, `GetAdvice`) run in-process
  inside `claude-obs serve` on 8427 (`internal/analysis` +
  `internal/server`), behind one shared `analysis.FlightGate`. The initial status reader and every
  dynamic profile runner share that gate, including after primary/directory changes.
  `Busy` must report any profile's active analysis or advice. Aggregate advice keeps
  its legacy slot; provider/account/profile reports use distinct scope hashes,
  while the shared gate prevents concurrent writers across all report scopes. A runner's `Wait` remains local to its jobs. Every status transition writes both
  the `analyses/<sid>/{status.json,report.json}` file layout under
  `<install-dir>/dashboard/data` and a full-row upsert into sqlite
  (`db.PutAnalysisRun`); `AnalysisStatus`/`AdviceStatus` read the db first
  and fall back to the file, which is also what makes a file-only Runner
  (`DB` nil, used in tests) work. `internal/indexer`'s `analysedIDs` reads
  the file side: a session counts as analysed once either report file exists
  there.
  - **A run lands exactly one report file, and removes the other.**
    `writeReport` (`internal/analysis/runner.go`) writes `report.json` when
    the run's output parses as a structured report, and `report.md` holding
    that output verbatim when it does not. The run ends `done` either way: an
    unparseable report is still a report, and there is no retry, since the
    money is already spent. Removing the file this run did not write is not
    tidiness: the server prefers `report.json`, so a stale one left beside a
    fresh `report.md` would quietly serve the previous run's report as though
    it were this one's.
- **A restart during a run leaves a status nothing else will ever finish, so
  the server reaps them at boot.** A run is a goroutine inside one
  `claude-obs serve` process and only that goroutine writes the terminal
  status, so killing the process (a sync's `launchctl kickstart -k`, a crash, a
  kill) leaves `status.json` and the sqlite row at `running` with nothing alive
  to close them. `cmdServe` therefore calls
  `runner.ReapOrphanedRuns()` before serving: a fresh process owns no runs, so
  every `running` status it finds is dead by definition, and it repairs each
  through the same `dualStatus` path so the file and the row cannot disagree.
  It finds them from the file layout (authoritative, since `dualStatus` writes
  the file first) unioned with `db.RunningAnalysisRuns`, so a row whose file was
  removed is repaired too. This is not theoretical: on 2026-08-17 a sync
  restarted the server 6m34s into a real opus run, spending $3.74 for nothing,
  and before this existed the page went on claiming "analysing" for the full
  30 minutes that `isStuck` takes to time out (1500s timeout + 300s grace) with
  the runner idle the whole time. **Check `runner_busy` before restarting the
  server**, and prefer `launchctl kickstart` when nothing is in flight.
- **Stuck thresholds and runner timeouts now have one source of truth:
  `GetMeta`.** `internal/analysis/runner.go`'s `DefaultAnalysisTimeout` /
  `DefaultEvidenceTimeout` (1500s/2700s) are the only numeric defaults left,
  overridable by `ANALYSIS_TIMEOUT_S` / `ADVICE_EVIDENCE_TIMEOUT_S` (read in
  `cmd/claude-obs/main.go`'s `cmdServe`). Nothing hardcodes a stuck
  threshold: a hardcoded copy drifted below the real timeout once, at 1200s
  against a 1500s default, and reported healthy runs as stuck. `GetMeta`
  serves the Go values as `analysis_timeout_s` / `advice_evidence_timeout_s`,
  and the frontend reads them:
  `frontend/src/lib/sessionDetail.ts`'s `stuckAfter`/`isStuck` compute the
  stuck rule from `analysis_timeout_s` plus a 300s grace, and
  `Advice.tsx` imports the same `isStuck` rather than carrying its own
  copy. One number, one function, two call sites.
- **Retention strings are read back from the rendered plist, not just
  claimed.** `internal/server/health.go`'s `loadRetention` reads the actual
  rendered `com.agent-observability.{victoriametrics,victorialogs}.plist`
  under `LaunchAgentsDir` and parses the real `-retentionPeriod=` value
  (`parseRetention`/`retentionRe`), rather than restating a configured
  value as though it were observed.
  `GetHealthResponse.retention_observed` reports whether the
  read-back succeeded; `Health.tsx` just renders whichever string it got,
  with no retention knowledge of its own. The read can only succeed once a
  profile's plists actually exist under `LaunchAgentsDir`, which this repo's
  own dev/test runs never render; there both rows fall back to
  `defaultVMRetention`/`defaultVLRetention` ("24 months"/"52 weeks"). On the
  live install the read-back now genuinely succeeds (`retention_observed`
  true, verified 2026-08-17 against the rendered plists), so a wrong value
  there is a real disagreement rather than a fallback.
  Verified in this pass: those fallbacks still agree with the templates'
  `internal/installer/templates/victoriametrics.plist.tmpl`'s
  `-retentionPeriod=24` and `victorialogs.plist.tmpl`'s `-retentionPeriod=52w`.
  A retention change means editing the template's flag and `health.go`'s two
  fallback consts together.
- **Freshness limits must agree across the API and UI.** Claude uses
  `internal/server/health.go`'s `staleAfterS = 600`; Codex uses
  `internal/monitor/quota.go`'s `QuotaFreshFor = 5 * time.Minute`.
  `frontend/src/lib/allowance.ts` mirrors these as `CLAUDE_FRESH_FOR_S`
  and `CODEX_FRESH_FOR_S`, shared by the current account dashboard and
  legacy presentation helpers. Change each server/client pair together.
  The `STALE_AFTER_S` environment entry is retained for compatibility;
  changing it does not override these limits. Failed polls, missing
  confirmations, and unconfirmed resets retain their existing handling.
  Pay-as-you-go collection keeps its 900-second cutoff, and Claude billing
  keeps its separate 21600-second cutoff.
- **Findings rules are pure.** In `internal/rules/rules.go`, every rule is a
  pure function from an already-fetched `Data` bundle to one `Finding`, with
  no fetches and no I/O, so each is testable without a store. A new rule
  means a new function, an entry in the package's rule registry, and a case
  per severity in `internal/rules/rules_test.go`.
- **The indexer's row shape is versioned.** Adding or changing a field means
  bumping `internal/indexer/index.go`'s `SchemaVersion` (currently `"5"`),
  which forces one full re-parse on the next pass against its
  `sessions_schema` meta row. Session rows themselves are untouched by the
  bump; only `transcript_state` is cleared. A full re-parse of ~240
  transcripts measured 22s, git lookups included, so the bump is cheap enough
  to reach for whenever a field's meaning moves.
- **A session row records which profile it came from, and blank is not a guess.** `sessions.profile` (migration `0005`) holds the same name `OTEL_RESOURCE_ATTRIBUTES` stamps on that profile's telemetry, so a row joins to its own metrics and logs. It is attribution, not identity: session ids are uuids, so several profiles index into one table without colliding and would merge correctly with no column at all. A row indexed before it existed carries the empty string, which `frontend/src/lib/sessions.ts`'s `profileLabel` renders as a dash rather than naming a profile it does not know. `IndexOpts.Profiles` is the list and `ProjectsRoot` alone still works, attributed to nothing, which is what keeps a single-root caller identical. A fresh-signature row whose profile is empty is filled in without a re-parse, so the column populates on the first pass after an upgrade even for transcripts that have not changed.
- **One profile selector, and it narrows on two different labels.** `GetOverviewRequest`, `GetSpendRequest` and `ListSessionsRequest` each carry a `profile` field, empty meaning every profile, and `ListProfiles` backs the sidebar control. `profileScope` (`internal/server/spend.go`) is what keeps the two kinds of figure apart: `match()` narrows anything measured per profile (cost, tokens, sessions) and `accountMatch()` narrows anything about an allowance (plan windows, the billed figure, the overage bands), because an allowance belongs to an account that two profiles can share. Narrowing an allowance by profile draws one allowance as two; splitting a bill between two profiles on one account invents a number nothing charged. A selected profile whose account is unknown leaves the account half **unnarrowed** rather than guessing one: an unfiltered figure is wrong in a way the reader can see, where another account's figure is not.
  - **`ListSessions` must filter both halves.** The db rows filter in SQL and the VictoriaLogs overlay filters on the same `profile` label. A live session on another profile has no db row to be excluded by, so an unfiltered overlay synthesizes it as a "(not yet indexed)" row and shows it under a filter that excludes it. `internal/server/sessions_test.go`'s `TestBuildListSessionsFiltersTheLiveOverlayToo` pins it, with an unfiltered control first so an empty filtered result cannot be a query that never matched.
  - **`MergedParams.Match` goes through `CanMerge`, like `By`.** Filtering on a label the backfill never recorded matches no historical series, so the total silently becomes the live counter alone, which is the thirteenth-of-the-real-number failure recorded under History merge below. `internal/history/history_test.go`'s `TestMergedTotalsRefusesToMergeOnANonHistoricalFilter` pins it against a historical label and a non-historical one.
  - **`history.Selector` sorts its label keys.** VictoriaMetrics caches instant-query results by query string and Go's map iteration order is deliberately random, so an unsorted selector would miss its own cache on every call. `TestSelectorIsStableAndOptional` repeats the render twenty times.
  - **`ListProfiles` includes registry configuration even before telemetry arrives.** Configured names/IDs/providers remain visible for new and disabled profiles; Claude account metadata still comes from `claude_profile_account_info`. Never derive the whole roster from account windows or usage series, since an empty or unavailable provider must remain configurable.

  - **Overview's meters are one card per account, and Insights and Health say they ignore the selection.** A global control that some views silently honour and others silently ignore leaves the reader unable to tell "filtered to nothing" from "not filtered". `frontend/src/lib/profiles.ts` holds the pure rules and `profiles.test.ts` covers them, including that either profile of a shared account resolves to one card.
- **`GetUsageSnapshot` answers "which account has headroom", and `internal/usage` is the only Go probe.** The RPC serves the poller's store in under 100ms. It reports facts and no verdict, because a threshold baked in here would be one caller's policy sitting where every other caller has to work around it.
  - **It is the only RPC that answers GET**, through `idempotency_level = NO_SIDE_EFFECTS` in the proto, so it opens as a URL (`?encoding=json&message={}`; connect-go rejects a GET missing either parameter, and an empty `message`). Every other RPC stays POST only. `internal/server/server_test.go`'s `TestUsageSnapshotAnswersAPlainGet` pins both halves, with `GetSpend`'s 405 as the control.
  - **On-demand probing is OFF by default (`USAGE_ON_DEMAND_PROBE`), and `max_age_s` is ignored while it is.** A probe is a full Claude Code startup, hooks and all, and a server that spawns those on request is a bigger thing than an observability read should be. The poller already refreshes every profile every five minutes, which is inside what a plan-window ceiling can absorb. `on_demand_probe_enabled` says so in the response: a `max_age_s` that silently did nothing would let a caller read a stale number as a live one. Measured with it on, before it was gated: 0.09s from the store, 4.7s probing all five.
  - **A caller must not refuse a reading for being old.** Every row carries `confirmed_age_s` so staleness can be reported, and reporting it is the whole permitted use. Refusing on it turns a quiet poller, or a profile whose numbers simply have not moved, into a blocked decision, and there is no safe fallback from a refusal anyway.
  - **`rate_limits` is heterogeneous and must be decoded as `json.RawMessage`.** Measured on a live response: 25 keys, of which 5 objects, 17 null, 2 arrays and one bool. Decoding it as `map[string]struct{...}` fails on the first array and takes the whole response down, which is exactly what shipped first: every profile came back "no control_response with rate limits" while the response was sitting there intact. A tidy two-window fixture passed against that parser, so `internal/usage/probe_test.go`'s fixture carries all four value kinds on purpose.
  - **Windows come from `rate_limits.limits[]`, not the sibling `five_hour`/`seven_day` keys.** That array is byte-for-byte the shape of `cachedUsageUtilization`'s own `.utilization.limits[]`, so `windowOfKind` is the same `session` -> `five_hour`, `weekly_all` -> `seven_day` mapping `bin/limits-poller.sh` applies. Agreeing with the poller is the point: a probe's window has to land on the store's window of the same name or a snapshot grows a second set beside the one it meant to refresh. It is also the only field carrying `weekly_scoped`.
  - **A probe that returns no window does NOT stamp a confirmation.** Signed-in-but-no-windows is a payload shape change, not a reading, and stamping there would claim the stored figures had just been checked when nothing checked them. It records an error and leaves the store's numbers and its age alone. A probe answering `rate_limits_available: false` DOES confirm, because "there is nothing to fetch" is an answer.
  - **The prober is single flight per profile.** A launch wave asks about every profile at once while the poller probes on its own cadence, so without it one profile has several probes in the air, each spawning a process and each racing to rewrite the same config file. It is not a cache: a caller arriving after a flight finishes gets a fresh probe, since serving an old answer is the lag this endpoint exists to remove.
  - **`internal/analysis.IsDefaultProfile` delegates here**, so the Go side has one implementation of the default-profile rule rather than two. The shell half stays its own copy.
- **`notifier/build.sh` skips the rebuild when nothing in it changed, and that is a permissions fix rather than a speed one.** The bundle is ad-hoc signed (`codesign --sign -`), and an ad-hoc signature has no team identifier: its identity IS the code directory hash, which changes on every recompile. macOS keys TCC grants to that identity, so rebuilding hands the system a different app wearing the same name and every permission the notifier held is requested again. Four upgrades in one afternoon on 2026-09-21 meant four rounds of prompts for an app whose source had not changed once. The stamp covers both Swift sources, `Info.plist.tmpl` and `LABEL_PREFIX`, and is written before signing so it is sealed; a missing or unreadable stamp rebuilds, because not knowing always means build. `CLAUDE_OBS_FORCE_NOTIFIER=1` overrides, for a toolchain change rather than a source change.
- **A session has two names, and only one of them is a key.** `project` is
  Claude Code's own encoded launch directory (`-Users-you-Development-GitHub-acme`); `workspace` is the descriptive name `internal/workspace` derives from where the session actually worked (`acme/api`). Everything the user reads shows `workspace` (`frontend/src/lib/sessions.ts`'s `projectLabel`, which falls back to `shortProject(project)` for a row indexed before the column existed); `project` remains the sqlite column other joins key on. Three server-side paths render the name and all three must agree: `dbRowToProto` passes it through for the sessions pages, `buildDelivery` joins through `workspacesFromSessions`, and `buildProjectAttribution` groups its history half on the backfill's `workspace` label while joining its live half through the same map. That last one is the fragile one, and `internal/server/spend_test.go`'s `TestProjectAttributionMergesHistoryOnWorkspaceLabel` is what pins it: if the two halves ever key differently, one project splits into two rows with half its money in each, which reads as real.
- **`--reset-cost-series` is guarded, because history outlives its own transcripts.** The flag rebuilds the cost series by re-parsing transcripts, and Claude Code ages transcripts out after roughly 30 days, so a series older than that has no source left to rebuild from. Measured on a real install 2026-09-21: `claude_hist_daily_cost_usd` spanned 2026-07-16 to 2026-08-17 while the oldest surviving transcript was 2026-08-19. **The two did not overlap by a single day**, so a reset would have deleted that whole month of cost history and re-imported nothing for that period, reporting success. `indexer.CheckResetSafety` compares the two dates and `cmdBackfill` refuses when they do not cover; `--i-know-this-deletes-history` overrides. It fails **open** on a query error, because it guards one known hazard and is not an authority on whether the store is reachable.
  - **`queryTotal` passes `nocache=1`, and removing it breaks every reconciliation in this package.** VictoriaMetrics caches instant-query results by query string, and both `reconcile` and `settledTotal` poll one query string repeatedly while an import is still ingesting. Without it the first read lands mid-ingestion and every later read returns that same partial figure from cache, unchanged, for as long as you poll: indistinguishable from a settled total that is simply short. Measured 2026-09-21 on a 147 series import, the cached query held a total **4.6% short** indefinitely while the same query with `nocache=1` returned the dump's own sum to the cent. This is at least part of what the "settles within about twenty seconds" note below was attributing to ingestion lag. `internal/indexer/relabel_test.go`'s `TestQueryTotalDisablesTheQueryCache` pins it.
  - **`settledTotal` deliberately does NOT use `reconcile`'s stability heuristic.** A large import climbs through many intermediate totals, any of which can hold still across two 500ms polls, so "stable" is not evidence that ingestion finished. Its only acceptances are "the totals match" and "the deadline expired". The asymmetry is the point: a slow success costs 90 seconds, a false failure tells an operator their money is gone.
  - **Use `claude-obs migrate-history-labels` for a label change instead.** It moves the existing samples through VictoriaMetrics' own export and import (`internal/indexer/relabel.go`), so values round-trip exactly and no transcript is consulted. It writes the rewritten dump to `--backup-dir` (default `<install-dir>/backups`) **before deleting anything**, since the samples it is moving may be the only copy left, and names that file in any failure. `--dry-run` rehearses without deleting or importing, and `--metrics` restricts it to a throwaway metric name so a rehearsal never has to name the real series: without that flag a "rehearsal" runs against live history, which is how one did. Verified end to end on 2026-09-21 against a throwaway metric name: 80 series and 232 samples relabelled, `sum by (profile)` returning exactly the original total, to four decimal places.
  - **An absent profile label is not `profile=""`.** `toLines`' `profileLabel` emits nothing at all for an unlabelled run, because `profile=""` is a *different* series identity from the one an unlabelled install already holds: the old samples would not be overwritten and a query summing the metric would read both, counting every historical dollar twice. `internal/indexer/backfill_test.go`'s `TestToLinesOmitsTheProfileLabelEntirelyWhenThereIsNone` pins it.
- **The `workspace` label is why `--reset-cost-series` exists.** The
  backfill's idempotency rests on VM dedup: identical series+timestamp lines
  overwrite, so a re-run cannot double count. Adding a label changes the
  series identity, so samples written before `workspace` existed are *not*
  overwritten by a plain re-run, and a query summing the metric reads the old
  series plus the new one. `claude-obs backfill --reset-cost-series` deletes
  `claude_hist_cost_usd` and `claude_hist_daily_cost_usd` first
  (`deleteSeries` in `internal/indexer/backfill.go`), which is the only clean
  way to land a label change. The token series are deliberately untouched:
  they carry no project or workspace label, so no label change can orphan
  them. Any future label change to the cost series needs the same one-time
  reset run; a normal re-run must never pass the flag.
- **The name is derived by ranking, not by taking the launch directory.**
  `internal/workspace`'s `Name` walks a session's cwds most-mentioned first
  (`transcript.Summary.Cwds`) and stops at the first that still resolves to a
  git repo, naming it `owner/repo` from `origin`. Both halves earn their
  keep against real data: 184 of 255 sessions were launched from one workspace
  root, so the launch directory alone would have given them all that root's
  name, and their busiest directory is routinely a `.worktrees/` checkout that
  has since been deleted, which is why it walks on rather than stopping at the
  top entry. Measured over one profile, 149 sessions resolve to one repository.
  Git sits behind `workspace.Resolver` so tests need no repos, and an absent
  git binary degrades to the path fallback rather than failing a pass.
- **A delivery counter is a running total, not a flow, and the two are read differently.** `claude_code.commit.count` and `claude_code.pull_request.count` are cumulative per session: the series is that session's tally, so its **final value** is the answer and `history.seriesTable` marks both `Cumulative: true`. `MergedTotals` then reads them with `max_over_time` over a lookbehind spanning the whole request, and reduces across buckets with max rather than sum. Cost and tokens are flows and stay on `increase()` summed, which is what their buckets mean. Getting this wrong was not theoretical: measured 2026-08-17, `increase()` reported **0 commits for a session that made 50** (its commits landed before the window, so the series is flat and increase over flat is 0) and **48 for a session that made 30** (edge extrapolation, 60% high). The wide lookbehind is load-bearing too: a Claude Code restart mid-session starts a second series under the same `session.id`, and only a window holding both lets `sum by` add the two tallies. `internal/history/history_test.go`'s `TestMergedTotalsReadsDeliveryCountersAsRunningTotals` pins the flat and rising cases, and `TestMergedTotalsStillSumsCostBuckets` pins that cost was not dragged along.
  - **`internal/server/findings.go`'s `commitsQuery` keeps `increase()` on purpose.** It asks "how many in the last 7 days" and pairs that against 7 days of cost, so a session whose commits predate the window must contribute nothing. Same counter, different question. Do not "fix" it to match the others.
- **Commits and pull requests are live-only, and blank is not zero.**
  `claude_code.commit.count` and `claude_code.pull_request.count` carry a
  `session.id` label, which `internal/server/sessions.go`'s
  `loadSessionDelivery` reads per session for the list and detail pages, and
  `spend.go`'s `buildDelivery` reads per project. Neither has a backfilled
  counterpart, on purpose: a transcript cannot say whether the command it ran
  opened a pull request. So a 0 means "this session predates the collector"
  exactly as often as it means "this session shipped nothing", and
  `frontend/src/lib/sessions.ts`'s `deliveryText` returns null for it so the
  UI renders a dash with a tooltip rather than asserting the second. As of
  2026-08-17 the counters covered 20 of 255 indexed sessions, all within six
  days.
- **MCP server names are canonicalized in one place.** VictoriaLogs says
  `plugin:linear:linear`, transcripts say `plugin_linear_linear`, so the two
  raw spellings genuinely differ. `pricing.NormServer`
  (`internal/pricing/names.go`) is the only code that reconciles them
  (`internal/rules/rules.go`'s `orderedUniqueNorm` and
  `internal/transcript`'s `MCPServerOf` both call it).
- **The billed figure is refreshed by asking Claude Code for it.** Real
  dollars only change when Claude Code fetches `GET /api/oauth/usage`, which
  used to mean someone running `/usage` in a session; the figure was measured
  6.8 hours stale, past the hour after which Claude Code discards its own
  cache. `bin/limits-poller.sh` now sends a `get_usage` **control request** to
  a headless `claude` over the stream-json transport, which triggers that
  fetch: 2.7s, no model tokens, no transcript. Three things about it are
  load-bearing.
  - It ignores the response and re-reads `.claude.json` afterwards. The
    response's own `spend` field is not always populated (a rapid repeat comes
    back seeded from headers instead), and keeping the cache as the single
    source is what lets the "billed figure as of" caption stay honest when a
    probe fails.
  - `--settings '{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"0"}}'` is required,
    not decoration: without it every probe counts itself in
    `claude_code.session.count`, around 288 phantom sessions a day. The plain
    environment variable does not work, since the profile's settings.json wins
    over the environment. Verified by A/B: two suppressed probes left the
    counter unmoved, one unsuppressed probe raised it by 1.
  - `USAGE_PROBE_MIN_AGE_S` defaults to 300 because Claude Code throttles the
    cache rewrite to once every 5 minutes (`Ns_ = 300000` in the bundle);
    probing more often only spawns processes. `claude_usage_probe_ok` is
    written on probing runs only, so a run that skipped because the data was
    still fresh cannot be mistaken for a failure.
  This rides an internal protocol and can break on a Claude Code upgrade. It
  fails soft: the poller falls back to whatever the cache holds, and the age
  is already on the Health page.
- **Plan windows come from two sources of very different freshness.**
  `bin/limits-poller.sh` writes every window twice: `source="statusline"` from
  the tap, current within a minute, and `source="cache"` from
  `.claude.json`'s `cachedUsageUtilization`, which only changes when Claude
  Code refetches `/usage` and was measured 6.8 hours stale beside a 26 minute
  old tap. Both `claude_plan_window_pct` and
  `claude_plan_window_reset_timestamp_seconds` carry the label, and the poller
  must keep emitting both halves: when only the cache wrote reset times, a
  five hour window that had ended showed "resetting now" for hours. Never
  collapse the two with `max by (window)`, which answers "which number is
  larger" rather than "which is true"; go through `PickFreshest` in
  `internal/plan/plan.go`, which picks per window, since `weekly_scoped`
  exists only in the cache (`GetOverview`/`GetHealth` reach it through
  `LoadWindows`). `countdown` in `frontend/src/lib/format.ts` backs this up
  by calling a reset more than five minutes past "reset time stale" rather
  than imminent. The Go API and `pick` in `bin/usage-alert.sh` both choose by
  source freshness, never by largest percentage. Alerts join source timestamps
  on the exact account, profile and source, then choose per account/window.
  Their keys and click URLs also include the account. Shared profiles are one
  allowance, and unresolved `unknown`/unlabelled identities are excluded.
  The alert path used to use `max()` and so
  alerted off whichever source was larger: with the tap 9 hours cold it read
  five_hour at 14% against a reset 5.5 hours past while the freshly probed
  cache read 0%, and a tap that freezes above a threshold would have kept
  firing that threshold for a window that had since reset to empty.
- **The reset timestamp is an alert dedup key, so its last second matters.**
  `bin/usage-alert.sh` keys "once per threshold per reset period" on
  `claude_plan_window_reset_timestamp_seconds`, so any wobble in that number
  reads as a new period and re-fires every crossed threshold. The cache's
  `resets_at` carries sub-second precision that lands either side of the whole
  second between refetches (`02:59:59.574463` and `03:00:00.410182` were both
  observed for one boundary), and truncating it made the epoch alternate by
  one every few minutes: measured at 22 flips in four hours, each firing the
  75% and 90% banners again. `bin/limits-poller.sh` **rounds** that fraction
  rather than truncating, which usually lands on the whole second the
  statusline tap reports. **Rounding is a reduction, not a cure, and nothing
  should be built on it holding**: the observed spread runs from
  `02:59:59.527954` to past `03:00:00.6`, so it straddles the `.5` boundary as
  well and the metric still alternates by one, measured at four flips in
  ninety minutes rather than twenty two in four hours. The guarantee is
  `same_marker` in `bin/usage-alert.sh`, which treats numeric markers within
  `MARKER_SLACK_S` (120s) as the same period; the closest two real periods
  ever get is the five hour window, so it can only swallow a repeat, never a
  new window. Those four flips fired nothing, which is the check that matters. Non-numeric markers ("high", "stale", a date)
  still compare exactly. Missing reset metadata cannot establish a new period
  or overwrite an established marker. `internal/installer/usage_alert_test.go`
  runs the real script in scratch installs against fake metrics and verifies
  account/source isolation, shared accounts, reset wobble and missing-data
  recovery. Manual dry runs must likewise use a scratch install, never the
  source checkout. Native notification click delivery still needs a UI check.
- **Model names are still spelled twice.** The OTel counter reports the
  context variant (`claude-opus-5[1m]`), the transcripts the backfill parses
  report `claude-opus-5`. Server-side, `pricing.ShortModel`
  (`internal/pricing/names.go`) strips the suffix for every RPC that builds
  a breakdown itself (`GetOverview`, `GetSpend`). `GetSession`, though,
  deliberately returns raw model names (`internal/server/sessions.go`), so
  the session detail page canonicalizes client-side:
  `frontend/src/lib/format.ts`'s `shortModel` (and `shortProject`, the
  matching path-shortener) is a second, independent implementation of the
  same regexes. Without it one model becomes two rows with half its money in
  each; the two must agree.
- **History merge lives in `internal/history/history.go`.** The live OTel counters start the day the collector is installed; `internal/indexer/backfill.go` (run via `claude-obs backfill`) covers everything before that. `GetOverview` and `GetSpend` both call into this package; no page-level code merges anything itself. Every cost figure outside the real-time panels goes through `MergedTotals`, which buckets both and lets the live series fill only the buckets history left empty, so re-running the backfill (it re-parses transcripts the counters also counted) cannot double count. Reading the live counter alone once made "By model, 7 days" report a thirteenth of what the chart beside it read for the same week.
  - The backfill writes each figure twice, per request and rolled up to local
    noon: `claude_hist_cost_usd` / `claude_hist_daily_cost_usd` and
    `claude_hist_tokens` / `claude_hist_daily_tokens`. Daily-step windows read
    the rollups, hourly windows the per-request pair. **Cost and tokens must
    come from the same pair**, or a rate column divides two different windows;
    mixed, that was 6.4% on 7 days. A new `kind` with history means adding
    both series to the backfill and to `seriesTable` in `history.go`.
  - Not every kind has history. `pullRequests` and `commits` are counters
    Claude Code writes itself, and a transcript cannot say whether a command
    opened a PR, so they are live-only by nature and their panel says so.
  - `CanMerge(kind, by)` decides, from `seriesTable` and from `historical`
    (the labels the backfill records: `model`, `project`, `workspace`,
    `sidechain`, `type`, `profile`). Call sites do not repeat that judgement, and `history` can only
    turn merging off, never on: asking for a label the history series lack
    returns every historical dollar under an empty label, as one giant
    "unlabelled" row that reads as real. The same predicate words
    `LiveOnlyNote`, so the query and the sentence admitting what it missed
    cannot disagree.
  - **Two window grids, deliberately.** `DayWindow` anchors to 03:00 UTC for the daily chart, whose columns are calendar days. `RollingWindow` anchors at "now" for every table, so "30 days" means the 30 days ending now. Anchoring the tables to the chart's grid was tried and reverted: at 23:00 local it hid 17 hours of the heaviest day, most of a day's spend. The two therefore disagree by the edges each includes, which is expected.

## Never commit

`state/`, `dashboard/data/`, `config.env`, and `*.bak` stay untracked. Tracked files must carry no personal paths, emails, or one person's real spend figures; grep for those before opening a pull request. The tree was scrubbed clean of all three on 2026-08-17, so a hit today is something this change introduced, not a pre-existing one.

Measured costs of the tool itself are the exception and are wanted: the dollar figures in this file and the README are the evidence behind real decisions (why `Agent` is denied at standard depth, why the analyse path keeps its fan-out, why the server reaps orphaned runs). Keep those, and keep them honest about what was measured versus estimated.

`dashboard/data/` is not a source directory: it is the installed OutRoot
(`<install-dir>/dashboard/data`), where the analysis and advice runners write
real files (`analyses/<id>/{status.json,report.md}`,
`advice/{status.json,report.md}`) and where `internal/indexer`'s
`analysedIDs` looks for them. It stays gitignored and real.

## Conventions

- Commits: `feat:` / `fix:` / `docs:` / `test:`, lowercase, imperative.
- No em dashes in code, comments, docs, or commit messages.
- Analysis spawns the selected real CLI and may consume its allowance or API billing. It can appear in this stack's own telemetry.

## Account dashboard contracts

- `GetUsageSnapshot` schema 2 is additive: `accounts` is provider-neutral, while the legacy `profiles` array remains Claude-only with original presence flags. Keep zero/false separate from absent values. Current and weekly roles may reference the same actual window.
- `GetAccountHistory` preserves source observations, gaps and reset periods. Codex quota collection persists history independently of the UI; failed refreshes do not mint observations. Claude billing has unknown reporting periods; never infer daily spend from cumulative decreases or missing intervals.
- Account-scoped activity/reports are explicitly current-profile history where historical account ownership is unverified. `resolveDashboardScope` rejects unknown/mismatched selections, never widening them. Frontend `usePoll` hides old-scope responses immediately and ignores cancelled promises.
- Scoped advice requires an enabled evaluator provider and execution profile, separate from the evidence scope. Evidence is a temporary SQLite-derived bundle; account report directories use scope hashes. Scope metadata and orphan recovery must agree between files and SQLite.
- Oh My Pi (key `omp`) and opencode are detected-provider harnesses (`profiles.Detected`): groups like Claude and Codex, whose accounts are the inference providers found in their data, local ones included, keyed `<harness>:<provider>` (`omp:baseten`, `opencode:baseten`). The same provider used from both harnesses is two accounts, one under each. Their names are fixed: `internal/profiles.ProviderName` and `frontend/src/lib/providers.ts`'s `providerName` spell them and must agree, and `normalizeDetected` derives every such profile's name on `Load` (the harness name, plus the directory when a harness has two) so Settings asks no name and offers no Repair (`detectedHarness` is the frontend's half). A harness records the provider per request, not the provider-side account, so all of a harness's Baseten usage is one account, and a provider with no recorded cost (a local model, or one the harness cannot price) is an account whose headline is token usage: it has no `estimated_spend` capability and carries `month_to_date_tokens`, which `sidebarFigure`, the Overview row and the account page show instead of money. The account has no windows and carries the month-to-date estimate in `BillingSnapshot.estimated_overage_usd`; `sidebarFigure` shows an estimate only for these accounts; `BillingValue` and `billingLabel` show one for these accounts and for a Codex account's reported credits priced at `CODEX_CREDIT_USD`, naming it an estimate, so a Claude estimate, or one for included Codex usage, can never surface. Every profile of a harness matches every key of that harness, which is why two further checks exist: `resolveDashboardScope` refuses a key the dashboard does not list (`spendAccountExists`), and `buildScopedSessions` keeps only sessions whose models name that provider (`sessionsUsingProvider`). `GetPayAsYouGo` takes the harness (empty means `omp`). A detected account can be hidden from Settings (`SetAccountHidden`, a local-only write): the key is stored in `profiles.json`'s `hidden_accounts`, the account still arrives with `hidden: true`, and only lists (sidebar, Overview) drop it, so its page, data and collection are untouched; `Store.SetAccountHidden` refuses any key that is not a detected harness's. The orchestrator's `profilegate.sh` asks for `provider: "claude"` and never sees these accounts.
- Dark is the default theme; saved light/system choices remain respected. Overview, Activity, Insights, direct account navigation, Settings and Collection health share the visual system. Notification alias targets remain tested.

## Multi-provider analysis and release safety

The setup guide is `docs/analysis-setup.md`. `just upgrade` must verify the installed Git revision is an ancestor of the checkout before building; a source checkout can be older than the live installation. Preserve the current redesigned shell, account visibility, spend collectors and report scopes when adding analysis functionality. Check runner busy state before restarting.

Session sources and evaluators are independent. Advice `provider/account_key/profile` select evidence; `execution_provider/execution_profile` select the runner. Scope filtering, including OMP/OpenCode inference-provider session membership, happens before bounded sampling. Existing OpenCode profile directories contain `opencode.db`; derive the runner's XDG data root from their parent, and do not migrate them to the parent directory. Source credentials must never enter evidence bundles.

Model discovery is metadata-only and on demand through `GetAnalysisModels`. Keep catalogs scoped to the selected registered provider/home/executable, cache concurrent reads, and never start a model turn or local inference server to populate the picker. Changing evaluator profiles clears the model choice. Preserve the profile-default fallback when discovery is unavailable.
