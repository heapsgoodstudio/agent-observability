-- Empty provider denotes pre-existing Claude rows and preserves their IDs.
ALTER TABLE sessions ADD COLUMN provider TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN native_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN parent_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN cost_known INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN usage_known INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN error_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0;
CREATE INDEX sessions_provider_profile ON sessions(provider, profile);

ALTER TABLE sessions ADD COLUMN tools_known INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN errors_known INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN duration_known INTEGER NOT NULL DEFAULT 0;
