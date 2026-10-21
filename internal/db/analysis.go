package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// AnalysisRun is one row in analysis_runs: the runner's on-disk status.json
// mirrored into sqlite. Kind is
// "analysis" (one row per analysed session, Key = that session's id) or
// "advice" (Key = "advice" for the legacy aggregate or a stable scope hash).
// ScopeJSON preserves the evidence roster independently of ExecutionProfile.
// Subagents is nil when unknown, mirroring the
// on-disk field being omitted entirely rather than written as null while a
// run is still in progress.
type AnalysisRun struct {
	Provider, Profile, SourceProvider, SourceProfile, SourceAccountKey string
	Kind                                                               string
	Key                                                                string
	State                                                              string
	Model                                                              string
	Depth                                                              string
	StartedAt                                                          int64
	FinishedAt                                                         int64
	Error                                                              string
	RunSessionID                                                       string
	Subagents                                                          *int64
	ScopeJSON                                                          string
	ExecutionProfile                                                   string
}

// PutAnalysisRun inserts r, or replaces every column of an existing row with
// the same (kind, key).
func PutAnalysisRun(d *sql.DB, r AnalysisRun) error {
	_, err := d.Exec(`
		INSERT INTO analysis_runs(kind, key, state, model, depth, started_at, finished_at, error, run_session_id, subagents, scope_json, execution_profile, provider, profile, source_provider, source_profile, source_account_key)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(kind, key) DO UPDATE SET
			state = excluded.state,
			model = excluded.model,
			depth = excluded.depth,
			started_at = excluded.started_at,
			finished_at = excluded.finished_at,
			error = excluded.error,
			run_session_id = excluded.run_session_id,
			subagents = excluded.subagents,
			scope_json = excluded.scope_json,
			execution_profile = excluded.execution_profile,
			provider = excluded.provider, profile = excluded.profile, source_provider = excluded.source_provider, source_profile = excluded.source_profile, source_account_key = excluded.source_account_key
	`,
		r.Kind, r.Key, r.State, r.Model, r.Depth, r.StartedAt, r.FinishedAt, r.Error, r.RunSessionID, r.Subagents, r.ScopeJSON, r.ExecutionProfile, r.Provider, r.Profile, r.SourceProvider, r.SourceProfile, r.SourceAccountKey,
	)
	if err != nil {
		return fmt.Errorf("db: put analysis run %s/%s: %w", r.Kind, r.Key, err)
	}
	return nil
}

// RunningAnalysisRuns returns every row still recorded as in flight, in a
// stable order. A run only exists as a goroutine inside one `claude-obs serve`
// process, so a fresh process finding rows here has found runs that died with
// a previous one; see internal/analysis's ReapOrphanedRuns, the only caller.
func RunningAnalysisRuns(d *sql.DB) ([]AnalysisRun, error) {
	rows, err := d.Query(`
		SELECT kind, key, state, model, depth, started_at, finished_at, error, run_session_id, subagents, scope_json, execution_profile, provider, profile, source_provider, source_profile, source_account_key
		FROM analysis_runs WHERE state = 'running' ORDER BY kind, key
	`)
	if err != nil {
		return nil, fmt.Errorf("db: list running analysis runs: %w", err)
	}
	defer rows.Close()
	var out []AnalysisRun
	for rows.Next() {
		var r AnalysisRun
		var subagents sql.NullInt64
		if err := rows.Scan(
			&r.Kind, &r.Key, &r.State, &r.Model, &r.Depth, &r.StartedAt, &r.FinishedAt, &r.Error, &r.RunSessionID, &subagents, &r.ScopeJSON, &r.ExecutionProfile, &r.Provider, &r.Profile, &r.SourceProvider, &r.SourceProfile, &r.SourceAccountKey,
		); err != nil {
			return nil, fmt.Errorf("db: scan running analysis run: %w", err)
		}
		if subagents.Valid {
			r.Subagents = &subagents.Int64
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list running analysis runs: %w", err)
	}
	return out, nil
}

// GetAnalysisRun looks up one row by (kind, key). ok is false with a nil
// error when no such row exists.
func GetAnalysisRun(d *sql.DB, kind, key string) (*AnalysisRun, bool, error) {
	var r AnalysisRun
	var subagents sql.NullInt64
	err := d.QueryRow(`
		SELECT kind, key, state, model, depth, started_at, finished_at, error, run_session_id, subagents, scope_json, execution_profile, provider, profile, source_provider, source_profile, source_account_key
		FROM analysis_runs WHERE kind = ? AND key = ?
	`, kind, key).Scan(
		&r.Kind, &r.Key, &r.State, &r.Model, &r.Depth, &r.StartedAt, &r.FinishedAt, &r.Error, &r.RunSessionID, &subagents, &r.ScopeJSON, &r.ExecutionProfile, &r.Provider, &r.Profile, &r.SourceProvider, &r.SourceProfile, &r.SourceAccountKey,
	)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("db: get analysis run %s/%s: %w", kind, key, err)
	}
	if subagents.Valid {
		r.Subagents = &subagents.Int64
	}
	return &r, true, nil
}

// ListAdviceSessions queries the authoritative index, never an exported index.
// A non-nil profile list is an exact allowlist: empty means no sessions.
// The empty historical provider spelling belongs to Claude only.
func ListAdviceSessions(d *sql.DB, provider string, profiles []string) ([]SessionRow, error) {
	if d == nil {
		return nil, fmt.Errorf("advice: session database unavailable")
	}
	clauses := []string{"provider = ?"}
	args := []any{provider}
	if provider == "" {
		clauses[0] = "1=1"
		args = nil
	}
	if provider == "claude" {
		clauses[0] = "provider IN ('claude', '')"
		args = nil
	}
	if profiles != nil {
		if len(profiles) == 0 {
			return []SessionRow{}, nil
		}
		marks := make([]string, len(profiles))
		for i, p := range profiles {
			marks[i] = "?"
			args = append(args, p)
		}
		clauses = append(clauses, "profile IN ("+strings.Join(marks, ",")+")")
	}
	rows, err := d.Query("SELECT "+sessionColumns+" FROM sessions WHERE "+strings.Join(clauses, " AND ")+" ORDER BY start DESC, id", args...)
	if err != nil {
		return nil, fmt.Errorf("advice: list scoped sessions: %w", err)
	}
	defer rows.Close()
	out := []SessionRow{}
	for rows.Next() {
		row, err := scanSessionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// FilterAdviceSessions matches the session list's detected inference-provider scope.
// Totals remain whole-session totals when a session used several providers.
func FilterAdviceSessions(rows []SessionRow, accountKey string) []SessionRow {
	harness, inference, ok := strings.Cut(accountKey, ":")
	if !ok || (harness != "omp" && harness != "opencode") {
		return rows
	}
	selected := []SessionRow{}
	for _, row := range rows {
		for _, model := range row.Models {
			if strings.HasPrefix(model.Model, inference+"/") {
				selected = append(selected, row)
				break
			}
		}
	}
	return selected
}
