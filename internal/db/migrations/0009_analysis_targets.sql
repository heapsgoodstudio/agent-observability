ALTER TABLE analysis_runs ADD COLUMN provider TEXT NOT NULL DEFAULT 'claude';
ALTER TABLE analysis_runs ADD COLUMN profile TEXT NOT NULL DEFAULT '';
ALTER TABLE analysis_runs ADD COLUMN source_provider TEXT NOT NULL DEFAULT 'claude';
ALTER TABLE analysis_runs ADD COLUMN source_profile TEXT NOT NULL DEFAULT '';
ALTER TABLE analysis_runs ADD COLUMN source_account_key TEXT NOT NULL DEFAULT '';
CREATE TABLE session_sources (id TEXT PRIMARY KEY, path TEXT NOT NULL, cwd TEXT NOT NULL DEFAULT '', format TEXT NOT NULL);
