-- sessions is one row per Claude Code session: the indexer's index.json row
-- and per-session detail file folded into a single table. "end" is a
-- reserved word in SQLite and must stay quoted everywhere it is read or
-- written.
CREATE TABLE sessions (
    id                 TEXT PRIMARY KEY,
    project            TEXT NOT NULL,
    start              INTEGER NOT NULL,
    "end"              INTEGER NOT NULL,
    cost_usd           REAL NOT NULL,
    requests           INTEGER NOT NULL,
    tok_input          INTEGER NOT NULL,
    tok_output         INTEGER NOT NULL,
    tok_cache_read     INTEGER NOT NULL,
    tok_cache_creation INTEGER NOT NULL,
    sidechain_cost_usd REAL NOT NULL,
    tool_calls         INTEGER NOT NULL,
    user_turns         INTEGER NOT NULL,
    models             TEXT NOT NULL,
    skills             TEXT NOT NULL,
    mcp_servers        TEXT NOT NULL,
    tools              TEXT NOT NULL,
    trajectory         TEXT NOT NULL,
    has_transcript     INTEGER NOT NULL,
    analysed           INTEGER NOT NULL
);

-- transcript_state is the indexer's incremental-scan bookkeeping: one row
-- per primary transcript path, mirroring session-indexer.py's
-- [mtime, size, file_count, status] state entries.
CREATE TABLE transcript_state (
    path       TEXT PRIMARY KEY,
    mtime      REAL NOT NULL,
    size       INTEGER NOT NULL,
    file_count INTEGER NOT NULL,
    status     TEXT NOT NULL
);
