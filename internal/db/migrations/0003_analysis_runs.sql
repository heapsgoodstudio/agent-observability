-- analysis_runs is one row per analysis or advice run: the runner's on-disk
-- status.json (bin/analysis-runner.py's set_status) mirrored into sqlite.
-- key is the analysed session id for kind='analysis' and the fixed string
-- 'advice' for kind='advice' (one global advice slot, mirroring the single
-- advice/status.json).
CREATE TABLE analysis_runs (
    kind           TEXT NOT NULL CHECK(kind IN ('analysis', 'advice')),
    key            TEXT NOT NULL,
    state          TEXT,
    model          TEXT,
    depth          TEXT,
    started_at     INTEGER,
    finished_at    INTEGER,
    error          TEXT,
    run_session_id TEXT,
    subagents      INTEGER,
    PRIMARY KEY (kind, key)
);
