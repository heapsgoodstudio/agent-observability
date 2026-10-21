ALTER TABLE analysis_runs ADD COLUMN scope_json TEXT NOT NULL DEFAULT '';
ALTER TABLE analysis_runs ADD COLUMN execution_profile TEXT NOT NULL DEFAULT '';
