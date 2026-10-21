# Analysis implementation review

Setup and operator instructions are in [analysis setup](analysis-setup.md).

The implementation extends the current redesigned dashboard and its existing Claude, Codex, OMP and OpenCode collectors. It preserves the sidebar, account visibility, provider spend views and separately saved advice scopes.

- Evaluator provider/profile/model are independent of source scope. The API retains its existing advice scope fields and adds `executionProvider`; `executionProfile` continues to select credentials. Session analysis records both source and evaluator.
- Model selection uses a profile-scoped catalog RPC with bounded metadata-only discovery and a five-minute cache. It lists the CLI's configured/available models, groups harness models by inference provider, and keeps the profile default usable if discovery fails. No model is loaded or invoked by catalog discovery.
- The existing collectors retain their usage attribution and publish transcript source pointers. OpenCode exports only the selected session from its read-only SQLite source into a temporary JSONL file.
- Advice takes up to 200 recent sessions from SQLite and exposes at most ten transcripts for evidence depth. Provider/account filtering happens before sampling. Explicitly empty profile scopes stay empty. Detected inference-provider accounts filter sessions by their recorded model providers and retain whole-session totals.
- Evaluation considers task outcome, verification, instruction following, context selection, tool use, recovery and delegation. Missing evidence may produce an unknown verdict. Definition counts and high token counts alone do not establish avoidable work. Confidence is a qualitative evidence assessment, not a calibrated probability.
- Findings for other providers use indexed observations. Claude's existing scoped live telemetry checks remain attributed to Claude. Checked and insufficient-evidence results can be expanded; report actions lead and include verification steps.
- The shared flight gate and scope-specific status/report storage preserve single-run behavior, timeout handling and restart recovery. CLI failures, including structured OpenCode error events, are not accepted as reports.
- `just upgrade` checks Git ancestry against the installed revision before building. This prevents an older source branch from silently replacing a newer UI.

## Runner contracts

| CLI | Execution | Account selection |
|---|---|---|
| Claude | Print mode, stdin prompt, configured hook/tool permissions | Selected `CLAUDE_CONFIG_DIR`; default-home special case retained |
| Codex | `exec --sandbox read-only`, capture `--output-last-message` | Selected `CODEX_HOME` |
| OMP | `--print --mode text --tools read,grep,glob --no-title` | Selected `PI_CODING_AGENT_DIR`; inherited named-profile overrides removed |
| OpenCode | `run --format json`, extract text events, reject error events | Parent of the registered `opencode` directory as `XDG_DATA_HOME` |

Binaries and their runtimes are resolved for launchd's small environment. Credential storage and model availability remain the CLI's responsibility. The dashboard's availability indicator checks executable and directory availability, not live authentication.

Shallow advice includes its summaries directly in the prompt (up to 200 sessions and 128 KiB), so it needs no file-reading tool calls. Session analysis and deeper advice require a model with working tool support. If a local model prints tool-call markup as ordinary text, use shallow advice or select a model/provider configuration that supports native tool calls.

## Historical Claude delegation measurements

The existing standard-depth tool restrictions came from measurements before this multi-provider change. These are recorded execution costs from those trials, not prices or predicted savings for current reports:

| Earlier Claude standard-depth configuration | Observed result |
|---|---|
| Agent permitted, Bash permitted | 537 seconds, $1.67 over 22 requests |
| Agent permitted, Bash denied | Timed out at 900 seconds after $3.66, including $3.07 (84%) in subagents |
| Agent denied, Bash permitted | 261 seconds, $1.20 over 14 requests, no subagent spend |

This is why Claude standard advice still denies Agent and retains Bash. The bounded evidence bundles and additional providers have not been benchmarked against those historical trials; these measurements do not establish their performance.

## Validation and remaining opportunities

Tests cover evaluator/source separation, account filtering, report scopes, source export isolation, final-output parsing, prompt bounds, report rendering and preservation of the redesigned session experience. The full gate includes shell lint, Go vet/tests, frontend tests/types, static build and installer dry run. Live setup should additionally verify a short run through each selected evaluator, since mocked CLI tests cannot validate credentials or local model servers.

Future improvements include a curated set of human-reviewed evaluation sessions, repeat-run stability checks, and calibrated confidence. The current deterministic recent-session sample is not representative of all historical work; every report must disclose that limitation. OMP/OpenCode allowance windows are not available. Existing provider-reported usage and explicitly labelled spend estimates are retained.

The CLI contracts were checked against installed CLI help and the [Codex non-interactive guide](https://learn.chatgpt.com/docs/non-interactive-mode), [OpenCode CLI](https://opencode.ai/docs/cli/), [OpenCode permissions](https://opencode.ai/docs/permissions/), and [OMP documentation](https://github.com/can1357/oh-my-pi).
