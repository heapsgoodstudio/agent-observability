Inputs you may read are restricted to this run's selected evidence scope:

1. {INDEX_PATH}: a temporary evidence bundle generated from SQLite for this run.
   Its `scope`, `coverage`, and `sessions` fields describe exactly what is included.
   Each session has request/token activity, model names, tools and context samples.
   This is not a global index. Never open another index or query the stores directly.
2. {PROFILE_DIR}: a JSON manifest of the source profile directories in this
   evidence scope, with their providers. The evaluator profile supplies credentials;
   it is not extra evidence. Never inspect credential files, auth.json, account
   caches, environment secrets, or directories outside this manifest.

State the scope and limitations from the bundle in `coverage`. The temporary
bundle directory is {SESSIONS_DIR}. There are no independent session JSON files.
Do not infer project paths by reversing an encoded project name, and do not
widen the scope if a configuration or transcript is missing.

Inspect relevant non-secret configuration and instruction files only from the
manifest: Claude settings.json/CLAUDE.md, Codex config.toml/AGENTS.md, OMP
config.yml/AGENTS.md, or OpenCode instructions/configuration when a path is explicitly
present. Do not assume that OpenCode's credential/data directory contains its
configuration. Inspect relevant hook scripts and skills only when there is
observed evidence of their use. Definition count alone does not establish waste.
Read project instructions only at an actual absolute workspace path established
by an included session; never guess it from a display name.

Read only sessions with a nonempty `transcript` path in the bundle, at most ten.
The bundle provides only a recent sample. Report its total and sampled counts,
which transcripts you read, and how recency or missing files limits conclusions.
Read in bounded chunks, skimming tool-result payloads. If helper agents are
available, use at most four readers, with no nested delegation; otherwise read
sequentially. Do not demand a tool or model unsupported by the evaluator.
Look for outcome verification, retries, repeated reads, user corrections,
context pressure and abandoned approaches. An isolated incident is an anecdote;
call it a repeated pattern only when multiple extracts support it.
