# Set up history, insights and analysis

This guide covers a fresh macOS installation and upgrading an existing dashboard. It enables Claude, Codex, Oh My Pi (OMP), and OpenCode as both session sources and analysis runners. The source being reviewed and the CLI/account/model doing the review are independent.

## 1. Install and authenticate the agent CLIs

Install the CLIs you want to use. History collection does not require running model sessions, but generating a report requires a working CLI and credentials or a reachable local model. The observability installer does not install these CLIs or sign into provider accounts.

| Agent | Example installation | Sign in / check |
|---|---|---|
| Claude Code | `brew install --cask claude-code` | `claude auth login`, then `claude auth status` |
| Codex | `npm install -g @openai/codex` | `codex login`, then `codex login status` |
| OMP | Install Bun, then `bun install -g @oh-my-pi/pi-coding-agent` | Start `omp`, use `/login` and `/model` to configure the intended provider and model |
| OpenCode | `npm install -g opencode-ai` | `opencode auth login`, then `opencode auth list`; use `opencode models` for model IDs |

Use one installation method per CLI. See the upstream [Claude quickstart](https://code.claude.com/docs/en/quickstart), [Codex CLI guide](https://learn.chatgpt.com/docs/codex/cli), [OMP installation and provider guide](https://github.com/can1357/oh-my-pi), and [OpenCode CLI guide](https://opencode.ai/docs/cli/) for alternative installation and authentication methods. CLI contracts were checked with Claude Code 2.1.280, Codex 0.156.1, OMP 18.2.8 and OpenCode 1.18.25; these are tested versions, not enforced minimum versions.

**Use credentials saved by the CLI.** A launchd service does not inherit API keys exported only in `.zshrc`, `.zprofile`, or the terminal that ran the installer. For OMP, `/login` can save API keys in its credential store; current releases use the profile's `agent.db`. OpenCode stores login credentials in its data directory. Never put credentials in this repository or send them through the dashboard API. A directory marked connected means history wiring is configured; it does not prove authentication or model access.

For multiple accounts, initialize and sign into separate profile directories. Use the same directory when launching the CLI and when adding it in Settings:

```bash
# Custom Claude profile
CLAUDE_CONFIG_DIR="$HOME/Claude/work" claude auth login

# Custom Codex profile
mkdir -p "$HOME/.codex-work"
CODEX_HOME="$HOME/.codex-work" codex login

# Custom OMP profile; use /login and /model in this session
PI_CODING_AGENT_DIR="$HOME/.omp/work" omp

# Custom OpenCode account; Settings will use .../work/opencode
XDG_DATA_HOME="$HOME/.opencode-accounts/work" opencode auth login
```

An OpenCode profile is its **app data directory ending in `opencode`**. The runner derives `XDG_DATA_HOME` from its parent. OpenCode's usual config discovery remains active; separate data roots isolate credential/history stores, not every global or project setting. Likewise, multiple credentials inside one OMP profile still follow OMP's routing rules. Separate credential profiles are needed for deterministic account selection.

## 2. Build and install the dashboard

With Homebrew, run `brew install heapsgoodstudio/tap/agent-observability` and then `claude-obs install`, and skip to opening Settings below. To build from source instead:

Requirements: macOS, Git, and Nix with flakes enabled. The Nix development shell supplies the build tools. Xcode Command Line Tools are optional for the notification app.

```bash
git clone https://github.com/heapsgoodstudio/agent-observability.git
cd agent-observability
nix develop --command just ci
nix develop --command just install
```

The installer builds the embedded UI, installs to `~/.agent-observability`, migrates SQLite, and starts six per-user launchd jobs. Keep the installation separate from the source checkout. Open [Settings](http://localhost:8427/settings). Fresh installs discover only the default Claude and Codex homes; add OMP, OpenCode and custom homes explicitly.

For an **existing installation**, first verify that your checkout includes the installed release:

```bash
git status --short
git fetch origin
"$HOME/.agent-observability/bin/claude-obs" version
git log -1 --oneline
```

Move your work onto the installed revision or a newer descendant if needed, preserving local changes. Check Insights/Analysis for an active run and let it finish before restarting services. Then run:

```bash
nix develop --command just ci
nix develop --command just upgrade
```

`just upgrade` refuses an older or unrelated checkout, and refuses an installed revision missing from local Git history. Fetch that history and reconcile the checkout; do not bypass the check to force an old UI onto a newer installation. Directly invoking the lower-level installer does not run this Git ancestry check. The check cannot reconstruct uncommitted source changes in an installed `-dirty` build; preserve that source and its patch when handing an installation over.

Upgrades preserve configured profile IDs, credentials, indexed history, metrics and logs. They restart the six jobs. A build alone does not update the running app. Before changes involving migrations, keep an SQLite backup made with SQLite's backup command/API, plus `profiles.json`, `config.env`, and the previous executable. An ordinary copy of a live WAL database is not a complete database backup. Replace executables atomically through the installer; never overwrite a running binary with `cp`.

## 3. Connect session history in Settings

Use **Add profile**, choose the provider, enter an absolute directory and a display name where offered, leave it enabled, then save. OMP/OpenCode names are derived from the harness and directory. Edit an existing matching entry instead of adding it again.

| Provider | Directory to enter | History read |
|---|---|---|
| Claude | `/Users/you/.claude` or custom Claude home | `projects/**/*.jsonl` |
| Codex | `/Users/you/.codex` or custom `CODEX_HOME` | Local rollouts, archived sessions, App Server metadata |
| OMP | `/Users/you/.omp/agent` or its custom agent home | `sessions/**/*.jsonl` |
| OpenCode | `/Users/you/.local/share/opencode` or `<custom XDG_DATA_HOME>/opencode` | `opencode.db`, opened read-only |

OpenCode's directory must contain `opencode.db`; do not enter `.config/opencode` or the parent `.local/share`. Existing OMP/OpenCode profiles need no path migration for this feature. Their existing collectors and inferred provider accounts remain in use.

Saving a new or re-enabled connection queues collection. Settings displays import progress; use **Import history** to request another pass. Wait for completion, check **Health**, then open **Activity** (`/sessions`) under each provider/account. Empty history can be legitimate for a newly authenticated profile. Collection errors remain visible. Disabling a connection stops collection and makes it unavailable as an evaluator while preserving indexed history.

## 4. Choose an evaluator and generate a report

For one session: open Activity, choose a session, then its **Analysis** tab. For a broader report: open **Insights → Advice report** in the source provider/account/profile view you want to review.

In **Run analysis with**, select:

1. **Provider**: Claude, Codex, OMP or OpenCode.
2. **Account / profile**: an enabled credential profile for that CLI.
3. **Model**: select from the models reported by that profile, or choose **Profile default**. OMP and OpenCode models are grouped by inference provider. Changing provider or account clears the previous model selection.

Model discovery uses metadata only: Claude SDK initialization, Codex App Server `model/list`, `omp models --json`, and `opencode models`. It does not send a prompt, start inference, load local models or start a local model server. The selected profile supplies the credential/configuration home; discovery runs outside project directories. Catalogs are cached for five minutes per provider/home/executable, concurrent requests share a lookup, and each lookup has a 15-second deadline. Failures are cached for 30 seconds. The picker polls once per minute, hides the previous account's list immediately on a switch, and retains **Profile default** if discovery fails or returns no models. Listing a model does not verify endpoint reachability, entitlement or native tool support.

For example, select a Claude account's activity in the sidebar, then choose Codex and a Codex account as the evaluator. Changing the evaluator does not widen the evidence scope. The selected CLI sends that evidence to its configured inference endpoint and consumes its allowance or API billing. With a local model, inference stays at that configured local endpoint.

Start with **shallow** for a small setup check. **Standard** also considers relevant source configuration; **evidence** adds transcripts. Advice samples at most 200 recent sessions and supplies at most ten transcript paths, reporting coverage and recency bias. Findings distinguish observed problems from missing evidence; reports include supporting evidence, confidence and verification steps. A high token count alone does not establish waste.

Shallow advice includes its summaries directly in the prompt (up to 200 sessions and 128 KiB), so it needs no file-reading tool calls. Session analysis and deeper advice require a model with working tool support. If a local model prints tool-call markup as ordinary text, use shallow advice or select a model/provider configuration that supports native tool calls.

Only one analysis/advice run can execute at a time. Default timeouts are 25 minutes for sessions and shallow/standard advice, and 45 minutes for evidence advice. The run continues if you leave the page. Saved advice remains separate for each scope; source scope, evaluator, model, depth and timestamps are retained. Older reports retain their previous format and may require regeneration.

OMP/OpenCode account views identify inference providers observed in history, not verified login identities. A session that used multiple inference providers can appear under multiple accounts, with whole-session totals; the evidence states that limitation. Missing transcripts prevent new session analysis, but do not erase recorded activity.

## 5. Verify the installation

```bash
"$HOME/.agent-observability/bin/claude-obs" doctor
curl -fsS http://localhost:8427/claudeobs.v1.ObsService/GetMeta \
  -H 'Content-Type: application/json' -d '{}'
curl -fsS http://localhost:8427/claudeobs.v1.ObsService/GetSettings \
  -H 'Content-Type: application/json' -d '{}'
```

`doctor` should find all six agents, the dashboard, both stores and the profile wiring. `GetMeta` reports the installed revision and `analysisTargets`; enabled profiles with executable CLIs should have `available: true`. This availability check does not spend tokens or validate credentials. `runnerBusy` is omitted by protobuf JSON when false. `GetSettings.historyImport` should settle to complete; investigate a failed state before trusting coverage.

Check both a session report and shallow advice for the desired evaluator. A successful small prompt from a terminal proves less than a dashboard run: the terminal can have credentials and runtime paths that launchd lacks. See `~/.agent-observability/logs/server.log` and the report's displayed error when a background run fails.

## Binary paths and local models

The runner searches its PATH and common installation directories: `~/.local/bin`, `~/.bun/bin`, `~/.opencode/bin`, `/opt/homebrew/bin`, and `/usr/local/bin`. Bun/npm launchers also need their runtime. For other locations, add absolute paths to the **installed** `~/.agent-observability/config.env`:

```bash
# Examples only: use executable paths that exist on your machine.
CODEX_BIN="/absolute/path/to/codex"
OMP_BIN="/absolute/path/to/omp"
OPENCODE_BIN="/absolute/path/to/opencode"
```

`CLAUDE_BIN` is recorded by the installer; use its `--claude-bin` override for a deliberate change. Do not add empty overrides, and do not put these values in the checkout. Custom keys survive upgrades. After changing binary paths, wait for the runner to become idle and run `just upgrade` or restart the server job:

```bash
launchctl kickstart -k "gui/$(id -u)/com.agent-observability.server"
```

A local provider still needs its inference server running and the configured model identifier available. For LM Studio, inspect the existing configuration and models with `lms server status`, `lms ls`, and `lms ps`. Start the server with `lms server start --port 1234`; load the chosen installed model with `lms load <model-key> --identifier <configured-api-model-id> --context-length <configured-context> --yes`. Keep that context limit aligned with OpenCode's model configuration and your machine's memory. The observability installer does not download models or arrange LM Studio startup at login. See the [LM Studio CLI documentation](https://lmstudio.ai/docs/cli).

For a new local setup, install [LM Studio and its CLI](https://lmstudio.ai/docs/cli), download a model through LM Studio, and register it with OpenCode. Merge a provider/model entry into `~/.config/opencode/opencode.json` without replacing existing entries or changing defaults unintentionally:

```json
{
  "provider": {
    "lms": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "LM Studio",
      "options": { "baseURL": "http://127.0.0.1:1234/v1" },
      "models": {
        "your-local-model": {
          "name": "Local analysis model",
          "limit": { "context": 32768, "output": 4096 },
          "tool_call": true
        }
      }
    }
  }
}
```

Load that model with `lms load <installed-model-key> --identifier your-local-model --context-length 32768 --yes`, then select `lms/your-local-model` in the analysis model dropdown after its catalog refreshes. These limits are an example; match the actual model and machine. Declaring `tool_call` does not make an incompatible model/template support tools. Verify a small file-reading task before deeper analysis, and check that a completed dashboard run displays a structured report. Merely receiving text from a model is not sufficient. Back up the OpenCode config before editing; account-specific data roots still share its normal config discovery.

## Troubleshooting

| Symptom | Check / action |
|---|---|
| UI looks older after an upgrade | Compare the installed revision with your checkout. Preserve your patch and use a descendant of the installed release. The upgrade ancestry check prevents this case on the normal upgrade path. |
| Provider missing from evaluator accounts | Add/enable its profile in Settings and install its CLI; check `analysisTargets[].unavailableReason`. |
| OMP works in terminal but says “No models available” in the dashboard | Store the intended API key with OMP's `/login`, or authenticate its configured credential broker. A terminal-only environment variable is not available to launchd. Bun must also be discoverable. |
| OpenCode cannot connect to `127.0.0.1:1234` | Start the configured local model server and load the model using the same API identifier, or explicitly choose a configured remote model. |
| OpenCode has no saved credentials | Local endpoints can work without them. For a remote provider, run `opencode auth login` with the same XDG data root used by that profile. |
| Authentication, model-access or permission error | Run the selected CLI with that exact profile directory, correct its login/model/permissions, then retry. The app retains profile hooks and does not bypass them. |
| Model dropdown is empty or outdated | Check the selected credential profile, CLI version and model configuration. Allow up to five minutes for the cached catalog to refresh. Use **Profile default** while a lookup is unavailable. |
| “Runner busy” | Let the current run finish. Check the report and server log; do not restart the service mid-run. |
| Empty/missing history | Verify the provider-specific directory and supported format, then import history and inspect Health. Old JSON-only OpenCode storage is not the SQLite format this collector reads. |
| Saved report appears outdated | Confirm its evidence scope and generation time. Generate a new report; changing evaluator does not rewrite old reports. |

For implementation details and evaluation limitations, see [analysis review](analysis-review.md), [OMP history](omp-provider.md), and [OpenCode history](opencode-provider.md).
