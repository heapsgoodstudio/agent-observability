package db

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
)

// SessionRow is one session's stored summary: the sessions table's Go shape,
// with every TEXT json column already decoded. Collection fields (Models,
// Skills, MCPServers, Tools, Trajectory) round-trip through UpsertSession and
// GetSession/ListSessions as non-nil empty collections rather than nil, so
// callers never have to nil-check before ranging.
type SessionRow struct {
	ToolsKnown      bool
	ErrorsKnown     bool
	DurationKnown   bool
	Provider        string
	NativeID        string
	ParentID        string
	CostKnown       bool
	UsageKnown      bool
	ReasoningTokens int64
	ErrorCount      int
	DurationMS      int64
	ID              string
	// Project is Claude Code's own project directory key (the encoded launch
	// path); Workspace is the descriptive name derived from where the session
	// actually worked. Project stays the join key spend attribution runs
	// through, so the two are kept separately rather than one overwriting the
	// other. Workspace is empty for a row indexed before it existed.
	Project   string
	Workspace string
	// Profile is which Claude Code profile the session ran under, the same
	// name OTEL_RESOURCE_ATTRIBUTES stamps on that session's telemetry.
	// Empty for a row indexed before the column existed.
	Profile          string
	Start            int64
	End              int64
	CostUSD          float64
	Requests         int
	Tokens           transcript.Tokens
	SidechainCostUSD float64
	ToolCalls        int
	UserTurns        int
	Models           []transcript.ModelCost
	Skills           []string
	MCPServers       []string
	Tools            map[string]int
	Trajectory       [][3]float64
	HasTranscript    bool
	Analysed         bool
}

// TranscriptState is one transcript path's incremental-scan bookkeeping,
// mirroring session-indexer.py's state[path] = [mtime, size, file_count,
// status] entries.
type TranscriptState struct {
	Path      string
	Mtime     float64
	Size      int64
	FileCount int64
	Status    string
}

const sessionColumns = `id, project, workspace, profile, start, "end", cost_usd, requests,
		tok_input, tok_output, tok_cache_read, tok_cache_creation,
		sidechain_cost_usd, tool_calls, user_turns,
		models, skills, mcp_servers, tools, trajectory,
		has_transcript, analysed, provider, native_id, parent_id, cost_known, usage_known, reasoning_tokens, error_count, duration_ms, tools_known, errors_known, duration_known`

// rowScanner is satisfied by both *sql.Row and *sql.Rows, letting
// scanSessionRow back GetSession and ListSessions with one implementation.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanSessionRow reads one row in sessionColumns order and decodes its JSON
// columns, matching UpsertSession's encoding.
func scanSessionRow(sc rowScanner) (SessionRow, error) {
	var row SessionRow
	var modelsJSON, skillsJSON, mcpJSON, toolsJSON, trajJSON string
	var hasTranscript, analysed int

	err := sc.Scan(
		&row.ID, &row.Project, &row.Workspace, &row.Profile, &row.Start, &row.End, &row.CostUSD, &row.Requests,
		&row.Tokens.Input, &row.Tokens.Output, &row.Tokens.CacheRead, &row.Tokens.CacheCreation,
		&row.SidechainCostUSD, &row.ToolCalls, &row.UserTurns,
		&modelsJSON, &skillsJSON, &mcpJSON, &toolsJSON, &trajJSON,
		&hasTranscript, &analysed, &row.Provider, &row.NativeID, &row.ParentID, &row.CostKnown, &row.UsageKnown, &row.ReasoningTokens, &row.ErrorCount, &row.DurationMS, &row.ToolsKnown, &row.ErrorsKnown, &row.DurationKnown,
	)
	if err != nil {
		return SessionRow{}, err
	}

	if err := json.Unmarshal([]byte(modelsJSON), &row.Models); err != nil {
		return SessionRow{}, fmt.Errorf("db: decode models for %s: %w", row.ID, err)
	}
	if err := json.Unmarshal([]byte(skillsJSON), &row.Skills); err != nil {
		return SessionRow{}, fmt.Errorf("db: decode skills for %s: %w", row.ID, err)
	}
	if err := json.Unmarshal([]byte(mcpJSON), &row.MCPServers); err != nil {
		return SessionRow{}, fmt.Errorf("db: decode mcp_servers for %s: %w", row.ID, err)
	}
	if err := json.Unmarshal([]byte(toolsJSON), &row.Tools); err != nil {
		return SessionRow{}, fmt.Errorf("db: decode tools for %s: %w", row.ID, err)
	}
	if err := json.Unmarshal([]byte(trajJSON), &row.Trajectory); err != nil {
		return SessionRow{}, fmt.Errorf("db: decode trajectory for %s: %w", row.ID, err)
	}
	row.HasTranscript = hasTranscript != 0
	row.Analysed = analysed != 0
	return row, nil
}

// UpsertSession inserts row, or replaces every column of an existing row
// with the same id. Nil collection fields encode as their empty JSON form
// ("[]"/"{}"), never "null".
func UpsertSession(d *sql.DB, row SessionRow) error {
	models := row.Models
	if models == nil {
		models = []transcript.ModelCost{}
	}
	modelsJSON, err := json.Marshal(models)
	if err != nil {
		return fmt.Errorf("db: encode models for %s: %w", row.ID, err)
	}

	skills := row.Skills
	if skills == nil {
		skills = []string{}
	}
	skillsJSON, err := json.Marshal(skills)
	if err != nil {
		return fmt.Errorf("db: encode skills for %s: %w", row.ID, err)
	}

	mcpServers := row.MCPServers
	if mcpServers == nil {
		mcpServers = []string{}
	}
	mcpJSON, err := json.Marshal(mcpServers)
	if err != nil {
		return fmt.Errorf("db: encode mcp_servers for %s: %w", row.ID, err)
	}

	tools := row.Tools
	if tools == nil {
		tools = map[string]int{}
	}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		return fmt.Errorf("db: encode tools for %s: %w", row.ID, err)
	}

	trajectory := row.Trajectory
	if trajectory == nil {
		trajectory = [][3]float64{}
	}
	trajJSON, err := json.Marshal(trajectory)
	if err != nil {
		return fmt.Errorf("db: encode trajectory for %s: %w", row.ID, err)
	}

	hasTranscript := 0
	if row.HasTranscript {
		hasTranscript = 1
	}
	analysed := 0
	if row.Analysed {
		analysed = 1
	}

	_, err = d.Exec(`
		INSERT INTO sessions(`+sessionColumns+`)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project = excluded.project,
			workspace = excluded.workspace,
			profile = excluded.profile,
			start = excluded.start,
			"end" = excluded."end",
			cost_usd = excluded.cost_usd,
			requests = excluded.requests,
			tok_input = excluded.tok_input,
			tok_output = excluded.tok_output,
			tok_cache_read = excluded.tok_cache_read,
			tok_cache_creation = excluded.tok_cache_creation,
			sidechain_cost_usd = excluded.sidechain_cost_usd,
			tool_calls = excluded.tool_calls,
			user_turns = excluded.user_turns,
			models = excluded.models,
			skills = excluded.skills,
			mcp_servers = excluded.mcp_servers,
			tools = excluded.tools,
			trajectory = excluded.trajectory,
			has_transcript = excluded.has_transcript,
			analysed = excluded.analysed,
            provider = excluded.provider, native_id = excluded.native_id,
            parent_id = excluded.parent_id, cost_known = excluded.cost_known,
            usage_known = excluded.usage_known, reasoning_tokens = excluded.reasoning_tokens,
            error_count = excluded.error_count, duration_ms = excluded.duration_ms, tools_known = excluded.tools_known, errors_known = excluded.errors_known, duration_known = excluded.duration_known
	`,
		row.ID, row.Project, row.Workspace, row.Profile, row.Start, row.End, row.CostUSD, row.Requests,
		row.Tokens.Input, row.Tokens.Output, row.Tokens.CacheRead, row.Tokens.CacheCreation,
		row.SidechainCostUSD, row.ToolCalls, row.UserTurns,
		string(modelsJSON), string(skillsJSON), string(mcpJSON), string(toolsJSON), string(trajJSON),
		hasTranscript, analysed, row.Provider, row.NativeID, row.ParentID, row.CostKnown, row.UsageKnown, row.ReasoningTokens, row.ErrorCount, row.DurationMS, row.ToolsKnown, row.ErrorsKnown, row.DurationKnown,
	)
	if err != nil {
		return fmt.Errorf("db: upsert session %s: %w", row.ID, err)
	}
	return nil
}

// ListSessions returns every session, cost_usd descending.
//
// profile, when non-empty, narrows the result to that profile. The match is
// exact, so a row whose own profile is the empty string (indexed before the
// column existed) is excluded from every filter rather than folded into one:
// the column means "which profile this came from", and blank there means "not
// known", not "any". Folding blanks into whichever profile was asked for would
// attribute one profile's money to another silently.
func ListSessions(d *sql.DB, profile string) ([]SessionRow, error) {
	query := `SELECT ` + sessionColumns + ` FROM sessions ORDER BY cost_usd DESC`
	args := []any{}
	if profile != "" {
		query = `SELECT ` + sessionColumns + ` FROM sessions WHERE profile = ? ORDER BY cost_usd DESC`
		args = append(args, profile)
	}
	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: list sessions: %w", err)
	}
	defer rows.Close()

	var out []SessionRow
	for rows.Next() {
		row, err := scanSessionRow(rows)
		if err != nil {
			return nil, fmt.Errorf("db: list sessions scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list sessions rows: %w", err)
	}
	return out, nil
}

// GetSession looks up one session by id. ok is false with a nil error when
// no such row exists.
func GetSession(d *sql.DB, id string) (*SessionRow, bool, error) {
	row, err := scanSessionRow(d.QueryRow(`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("db: get session %s: %w", id, err)
	}
	return &row, true, nil
}

// MarkMissing implements the indexer's permanence rule: every row whose id
// is not in seenIDs gets has_transcript cleared and analysed refreshed from
// the analysed map (false when the id is absent from it); nothing else on
// those rows changes, and rows in seenIDs are not touched at all.
func MarkMissing(d *sql.DB, seenIDs map[string]bool, analysed map[string]bool) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("db: mark missing begin: %w", err)
	}

	rows, err := tx.Query(`SELECT id FROM sessions WHERE provider IN ('', 'claude')`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("db: mark missing select: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			tx.Rollback()
			return fmt.Errorf("db: mark missing scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		tx.Rollback()
		return fmt.Errorf("db: mark missing rows: %w", err)
	}
	rows.Close()

	stmt, err := tx.Prepare(`UPDATE sessions SET has_transcript = 0, analysed = ? WHERE id = ?`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("db: mark missing prepare: %w", err)
	}
	defer stmt.Close()

	for _, id := range ids {
		if seenIDs[id] {
			continue
		}
		a := 0
		if analysed[id] {
			a = 1
		}
		if _, err := stmt.Exec(a, id); err != nil {
			tx.Rollback()
			return fmt.Errorf("db: mark missing update %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// StateGet looks up one path's transcript_state row. ok is false with a nil
// error when no such row exists.
func StateGet(d *sql.DB, path string) (*TranscriptState, bool, error) {
	st := TranscriptState{Path: path}
	err := d.QueryRow(`SELECT mtime, size, file_count, status FROM transcript_state WHERE path = ?`, path).
		Scan(&st.Mtime, &st.Size, &st.FileCount, &st.Status)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("db: state get %s: %w", path, err)
	}
	return &st, true, nil
}

// StatePut inserts st, or replaces an existing row for the same path.
func StatePut(d *sql.DB, st TranscriptState) error {
	_, err := d.Exec(`
		INSERT INTO transcript_state(path, mtime, size, file_count, status)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			mtime = excluded.mtime,
			size = excluded.size,
			file_count = excluded.file_count,
			status = excluded.status
	`, st.Path, st.Mtime, st.Size, st.FileCount, st.Status)
	if err != nil {
		return fmt.Errorf("db: state put %s: %w", st.Path, err)
	}
	return nil
}

// StatePruneMissing deletes every transcript_state row whose path is not a
// key with a true value in keep, mirroring run_index's `state = {p: v for
// p, v in state.items() if os.path.exists(p)}` sweep.
func StatePruneMissing(d *sql.DB, keep map[string]bool) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("db: state prune begin: %w", err)
	}

	rows, err := tx.Query(`SELECT path FROM transcript_state`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("db: state prune select: %w", err)
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			tx.Rollback()
			return fmt.Errorf("db: state prune scan: %w", err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		tx.Rollback()
		return fmt.Errorf("db: state prune rows: %w", err)
	}
	rows.Close()

	stmt, err := tx.Prepare(`DELETE FROM transcript_state WHERE path = ?`)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("db: state prune prepare: %w", err)
	}
	defer stmt.Close()

	for _, p := range paths {
		if keep[p] {
			continue
		}
		if _, err := stmt.Exec(p); err != nil {
			tx.Rollback()
			return fmt.Errorf("db: state prune delete %s: %w", p, err)
		}
	}
	return tx.Commit()
}

// MetaGet reads one meta key. ok is false with a nil error when the key is
// unset.
func MetaGet(d *sql.DB, key string) (string, bool, error) {
	var v string
	err := d.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("db: meta get %s: %w", key, err)
	}
	return v, true, nil
}

// MetaSet sets one meta key, inserting it or replacing its existing value.
func MetaSet(d *sql.DB, key, value string) error {
	_, err := d.Exec(`
		INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	if err != nil {
		return fmt.Errorf("db: meta set %s: %w", key, err)
	}
	return nil
}
