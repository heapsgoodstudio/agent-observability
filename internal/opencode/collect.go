package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/payg"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
	"github.com/heapsgoodstudio/agent-observability/internal/workspace"
)

// toolNameRe accepts a bare tool name outright; toolNameLeadingRe recovers
// its leading run of allowed characters when it does not. Mirrors
// internal/omp's rule so a tool name is sanitized the same way regardless of
// which harness recorded it.
var (
	toolNameRe        = regexp.MustCompile(`^[\w.:-]+$`)
	toolNameLeadingRe = regexp.MustCompile(`^[\w.:-]+`)
)

// fileStat is the identity a cached Collect result is validated against:
// unchanged size and mtime means opencode.db (and its -wal file, if any)
// have not been written to since.
type fileStat struct {
	size  int64
	mtime time.Time
}

// cachedEntry is the last successful Collect for one Options.Dir, valid only
// while opencode.db and opencode.db-wal (when present) have not changed
// size or mtime since, and only for the same profile identity.
type cachedEntry struct {
	profileID string
	dbStat    fileStat
	walOK     bool
	walStat   fileStat
	result    Result
}

// Collector caches the last successful Result per data directory, keyed on
// opencode.db and opencode.db-wal's size and mtime, so a Collect call that
// finds neither file changed can skip reopening and rereading the database.
type Collector struct {
	mu    sync.Mutex
	cache map[string]cachedEntry
}

func NewCollector() *Collector {
	return &Collector{cache: map[string]cachedEntry{}}
}

func (c *Collector) Collect(ctx context.Context, opts Options) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	result := Result{Complete: true}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if opts.Dir == "" || opts.ProfileID == "" {
		return result, fmt.Errorf("opencode: dir and profile ID are required")
	}

	dbPath := filepath.Join(opts.Dir, "opencode.db")
	dbInfo, err := os.Stat(dbPath)
	if err != nil {
		return result, fmt.Errorf("opencode: database unavailable: %w", err)
	}
	dbStat := fileStat{dbInfo.Size(), dbInfo.ModTime()}

	walInfo, walErr := os.Stat(dbPath + "-wal")
	walOK := walErr == nil
	var walStat fileStat
	if walOK {
		walStat = fileStat{walInfo.Size(), walInfo.ModTime()}
	}

	if c.cache == nil {
		c.cache = map[string]cachedEntry{}
	}
	if cached, ok := c.cache[opts.Dir]; ok && cached.profileID == opts.ProfileID &&
		cached.dbStat == dbStat && cached.walOK == walOK && (!walOK || cached.walStat == walStat) {
		return cached.result, nil
	}

	res, err := collect(ctx, opts, dbPath)
	if err != nil {
		return res, err
	}
	c.cache[opts.Dir] = cachedEntry{profileID: opts.ProfileID, dbStat: dbStat, walOK: walOK, walStat: walStat, result: res}
	return res, nil
}

// sessionState is one session's working accumulator while requests are
// being folded in; row is the value that eventually lands in Result.Sessions.
type sessionState struct {
	row        db.SessionRow
	modelCost  map[string]float64
	modelOrder []string // first-appearance order, per model key
	trajectory [][3]float64
}

// msgRow is one valid (json_valid) message row, with every field this
// package needs already pulled out of its JSON body by SQLite's own
// json_extract, so the JSON text itself is never decoded in Go.
type msgRow struct {
	sessionID                                          string
	timeCreated                                        int64 // epoch ms
	role, providerID, modelID                          string
	costValid                                          bool
	costUSD                                            float64
	tokIn, tokOut, tokReasoning, cacheRead, cacheWrite int64
	errored                                            bool
}

const (
	invalidMessageQuery = `SELECT COUNT(*) FROM message WHERE NOT json_valid(data)`
	invalidPartQuery    = `SELECT COUNT(*) FROM part WHERE NOT json_valid(data)`

	maxMessageTimeQuery = `SELECT session_id, MAX(time_created) FROM message GROUP BY session_id`

	sessionQuery = `SELECT id, parent_id, directory, time_created, time_updated FROM session`

	messageQuery = `
SELECT session_id, time_created,
       json_extract(data,'$.role'),
       json_extract(data,'$.providerID'),
       json_extract(data,'$.modelID'),
       json_extract(data,'$.cost'),
       json_extract(data,'$.tokens.input'),
       json_extract(data,'$.tokens.output'),
       json_extract(data,'$.tokens.reasoning'),
       json_extract(data,'$.tokens.cache.read'),
       json_extract(data,'$.tokens.cache.write'),
       json_extract(data,'$.error') IS NOT NULL
FROM message
WHERE json_valid(data)
ORDER BY session_id, time_created, id
`

	toolPartQuery = `
SELECT session_id, json_extract(data,'$.tool')
FROM part
WHERE json_valid(data) AND json_extract(data,'$.type') = 'tool'
ORDER BY session_id, time_created, id
`
)

// collect performs one fresh, read-only pass over dbPath, ignoring the
// Collector's cache. The connection is opened read-only and query-only
// (mode=ro, PRAGMA query_only) and capped at one open connection, so this
// never blocks or competes with opencode's own writer; it is closed before
// returning.
func collect(ctx context.Context, opts Options, dbPath string) (Result, error) {
	result := Result{Complete: true}

	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return result, fmt.Errorf("opencode: database unavailable: %w", err)
	}
	dsn := "file:" + absPath + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return result, fmt.Errorf("opencode: database unavailable: %w", err)
	}
	defer sqldb.Close()
	sqldb.SetMaxOpenConns(1)
	if err := sqldb.PingContext(ctx); err != nil {
		return result, fmt.Errorf("opencode: database unavailable: %w", err)
	}

	warn := func(s string) { result.Complete = false; result.Warnings = append(result.Warnings, s) }

	if n, err := countRows(ctx, sqldb, invalidMessageQuery); err != nil {
		return result, fmt.Errorf("opencode: query message validity: %w", err)
	} else if n > 0 {
		warn(fmt.Sprintf("%d message row(s) had malformed JSON and were skipped", n))
	}
	if n, err := countRows(ctx, sqldb, invalidPartQuery); err != nil {
		return result, fmt.Errorf("opencode: query part validity: %w", err)
	} else if n > 0 {
		warn(fmt.Sprintf("%d part row(s) had malformed JSON and were skipped", n))
	}

	maxMsgTime, err := loadMaxMessageTime(ctx, sqldb)
	if err != nil {
		return result, fmt.Errorf("opencode: query message times: %w", err)
	}

	states, err := loadSessions(ctx, sqldb, opts.ProfileID, maxMsgTime)
	if err != nil {
		return result, fmt.Errorf("opencode: query sessions: %w", err)
	}

	msgs, err := loadMessages(ctx, sqldb)
	if err != nil {
		return result, fmt.Errorf("opencode: query messages: %w", err)
	}

	priced := pricedProviders(msgs)
	result.Usage = accumulate(msgs, states, priced)

	if err := accumulateTools(ctx, sqldb, states); err != nil {
		return result, fmt.Errorf("opencode: query tool parts: %w", err)
	}

	result.Sessions = finalizeSessions(states)
	result.Sources = map[string]db.SessionSource{}
	for _, row := range result.Sessions {
		result.Sources[row.ID] = db.SessionSource{ID: row.ID, Path: dbPath, Cwd: row.Project, Format: "opencode"}
	}

	if err := ctx.Err(); err != nil {
		result.Complete = false
		result.Warnings = append(result.Warnings, "Collection canceled; completed local summaries retained")
		return result, err
	}
	return result, nil
}

func countRows(ctx context.Context, sqldb *sql.DB, query string) (int64, error) {
	var n int64
	err := sqldb.QueryRowContext(ctx, query).Scan(&n)
	return n, err
}

// loadMaxMessageTime reads every session's latest message time_created,
// unguarded by json_valid: the column is set regardless of whether that
// row's data body parses, so a malformed message still marks a session as
// having been touched at that time.
func loadMaxMessageTime(ctx context.Context, sqldb *sql.DB) (map[string]int64, error) {
	rows, err := sqldb.QueryContext(ctx, maxMessageTimeQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var sessionID string
		var maxTime int64
		if err := rows.Scan(&sessionID, &maxTime); err != nil {
			return nil, err
		}
		out[sessionID] = maxTime
	}
	return out, rows.Err()
}

// loadSessions builds one sessionState per session table row, archived
// sessions included. Start/End/DurationMS are resolved here since they only
// need maxMsgTime, not the per-request accumulation pass.
func loadSessions(ctx context.Context, sqldb *sql.DB, profileID string, maxMsgTime map[string]int64) (map[string]*sessionState, error) {
	rows, err := sqldb.QueryContext(ctx, sessionQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	states := map[string]*sessionState{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var (
			id, directory                string
			parentID                     sql.NullString
			timeCreatedMS, timeUpdatedMS int64
		)
		if err := rows.Scan(&id, &parentID, &directory, &timeCreatedMS, &timeUpdatedMS); err != nil {
			return nil, err
		}

		endMS := timeUpdatedMS
		if t, ok := maxMsgTime[id]; ok && t > endMS {
			endMS = t
		}

		row := db.SessionRow{
			ID:            SessionID(profileID, id),
			NativeID:      id,
			Provider:      "opencode",
			Profile:       profileID,
			Project:       directory,
			Start:         timeCreatedMS / 1000,
			End:           endMS / 1000,
			CostKnown:     true,
			UsageKnown:    true,
			ToolsKnown:    true,
			ErrorsKnown:   true,
			DurationKnown: true,
			HasTranscript: true,
			Tools:         map[string]int{},
		}
		if directory != "" {
			row.Workspace = workspace.TailSegments(directory, 2)
		}
		if parentID.Valid && parentID.String != "" {
			row.ParentID = SessionID(profileID, parentID.String)
		}
		dur := row.End - row.Start
		if dur < 0 {
			dur = 0
		}
		row.DurationMS = dur * 1000

		states[id] = &sessionState{row: row, modelCost: map[string]float64{}}
	}
	return states, rows.Err()
}

// loadMessages reads every valid message row once, in session/time order,
// so both the priced-provider pass and the accumulation pass can walk the
// same in-memory slice rather than requerying.
func loadMessages(ctx context.Context, sqldb *sql.DB) ([]msgRow, error) {
	rows, err := sqldb.QueryContext(ctx, messageQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []msgRow
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var (
			sessionID                                                string
			timeCreated                                              int64
			role, providerID, modelID                                sql.NullString
			cost, tokIn, tokOut, tokReasoning, cacheRead, cacheWrite sql.NullFloat64
			errFlag                                                  int64
		)
		if err := rows.Scan(&sessionID, &timeCreated, &role, &providerID, &modelID,
			&cost, &tokIn, &tokOut, &tokReasoning, &cacheRead, &cacheWrite, &errFlag); err != nil {
			return nil, err
		}
		costUSD, costValid := finiteOrZero(cost)
		out = append(out, msgRow{
			sessionID:    sessionID,
			timeCreated:  timeCreated,
			role:         role.String,
			providerID:   providerID.String,
			modelID:      modelID.String,
			costValid:    costValid,
			costUSD:      costUSD,
			tokIn:        numOrZero(tokIn),
			tokOut:       numOrZero(tokOut),
			tokReasoning: numOrZero(tokReasoning),
			cacheRead:    numOrZero(cacheRead),
			cacheWrite:   numOrZero(cacheWrite),
			errored:      errFlag != 0,
		})
	}
	return out, rows.Err()
}

// pricedProviders is the first pass the pricing rule needs: every canonical
// provider that has priced at least one request anywhere in the database.
// A provider outside this set is one opencode never has a rate for, so its
// cost-0 requests are genuinely free rather than unpriced.
func pricedProviders(msgs []msgRow) map[string]bool {
	priced := map[string]bool{}
	for _, m := range msgs {
		if m.role != "assistant" || m.providerID == "" || m.modelID == "" {
			continue
		}
		if m.costValid && m.costUSD > 0 {
			priced[payg.CanonicalProvider(m.providerID)] = true
		}
	}
	return priced
}

// classifyCost applies opencode's pricing rule to one request. A recorded
// cost above zero is always known. A recorded zero is known-free when its
// provider has never priced anything in this database (opencode has no rate
// for it, so nothing here was ever billed), known-zero when the request
// used no tokens at all (e.g. an immediate error), and otherwise unknown,
// because opencode could have priced this request and simply didn't. A
// missing or non-finite cost is unknown regardless.
func classifyCost(valid bool, cost float64, provider string, tokensSum int64, priced map[string]bool) (known bool, usd float64) {
	if !valid {
		return false, 0
	}
	if cost > 0 {
		return true, cost
	}
	if !priced[provider] {
		return true, 0
	}
	if tokensSum > 0 {
		return false, 0
	}
	return true, 0
}

// accumulate folds every message into its session (when one exists) and
// into the per-day/provider/model buckets (regardless of whether one
// exists), and returns the buckets sorted by day, provider, model.
func accumulate(msgs []msgRow, states map[string]*sessionState, priced map[string]bool) []payg.Bucket {
	buckets := map[[3]string]*payg.Bucket{}
	for _, m := range msgs {
		if m.role == "user" {
			if st, ok := states[m.sessionID]; ok {
				st.row.UserTurns++
			}
			continue
		}
		if m.role != "assistant" || m.providerID == "" || m.modelID == "" {
			continue
		}

		provider := payg.CanonicalProvider(m.providerID)
		model := payg.CanonicalModel(m.modelID)
		output := m.tokOut + m.tokReasoning
		tokensSum := m.tokIn + m.tokOut + m.tokReasoning + m.cacheRead + m.cacheWrite
		known, usd := classifyCost(m.costValid, m.costUSD, provider, tokensSum, priced)

		day := time.UnixMilli(m.timeCreated).In(time.Local).Format("2006-01-02")
		bk := [3]string{day, provider, model}
		b := buckets[bk]
		if b == nil {
			b = &payg.Bucket{Day: day, Provider: provider, Model: model, CostKnown: true}
			buckets[bk] = b
		}
		b.Requests++
		b.Input += m.tokIn
		b.Output += output
		b.CacheRead += m.cacheRead
		b.CacheWrite += m.cacheWrite
		b.Reasoning += m.tokReasoning
		if known {
			b.CostUSD += usd
		} else {
			b.CostKnown = false
		}

		st, ok := states[m.sessionID]
		if !ok {
			continue
		}
		row := &st.row
		row.Requests++
		row.Tokens.Input += m.tokIn
		row.Tokens.Output += output
		row.Tokens.CacheRead += m.cacheRead
		row.Tokens.CacheCreation += m.cacheWrite
		row.ReasoningTokens += m.tokReasoning
		if m.errored {
			row.ErrorCount++
		}
		if known {
			row.CostUSD += usd
		} else {
			row.CostKnown = false
		}

		modelKey := provider + "/" + model
		if _, ok := st.modelCost[modelKey]; !ok {
			st.modelOrder = append(st.modelOrder, modelKey)
		}
		if known {
			st.modelCost[modelKey] += usd
		}
		st.trajectory = append(st.trajectory, [3]float64{float64(m.timeCreated / 1000), row.CostUSD, float64(m.tokIn + m.cacheRead + m.cacheWrite)})
	}

	keys := make([][3]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		if keys[i][1] != keys[j][1] {
			return keys[i][1] < keys[j][1]
		}
		return keys[i][2] < keys[j][2]
	})
	out := make([]payg.Bucket, 0, len(keys))
	for _, k := range keys {
		b := *buckets[k]
		b.CostUSD = round6(b.CostUSD)
		out = append(out, b)
	}
	return out
}

// accumulateTools folds every "tool" part into its session's Tools/ToolCalls,
// ignoring text, reasoning, step-start, step-finish and patch parts (they
// are simply never selected by toolPartQuery's type='tool' filter) and any
// part whose session_id names no row.
func accumulateTools(ctx context.Context, sqldb *sql.DB, states map[string]*sessionState) error {
	rows, err := sqldb.QueryContext(ctx, toolPartQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var sessionID string
		var tool sql.NullString
		if err := rows.Scan(&sessionID, &tool); err != nil {
			return err
		}
		name := sanitizeToolName(tool.String)
		if name == "" {
			continue
		}
		st, ok := states[sessionID]
		if !ok {
			continue
		}
		st.row.Tools[name]++
		st.row.ToolCalls++
	}
	return rows.Err()
}

// finalizeSessions rounds each session's cost, orders its Models by cost
// descending (ties keep first-appearance order, since modelOrder already
// holds it and the sort below is stable), downsamples its trajectory, and
// returns every row sorted by ID.
func finalizeSessions(states map[string]*sessionState) []db.SessionRow {
	rows := make([]db.SessionRow, 0, len(states))
	for _, st := range states {
		row := st.row
		row.CostUSD = round6(row.CostUSD)
		sort.SliceStable(st.modelOrder, func(i, j int) bool { return st.modelCost[st.modelOrder[i]] > st.modelCost[st.modelOrder[j]] })
		for _, name := range st.modelOrder {
			row.Models = append(row.Models, transcript.ModelCost{Model: name, CostUSD: round6(st.modelCost[name])})
		}
		row.Trajectory = transcript.Downsample(st.trajectory, transcript.TrajectoryMax)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

// sanitizeToolName mirrors internal/omp's rule: trim, and when a name holds
// characters outside [\w.:-], keep only its leading run of allowed
// characters; a name with no such run at all reduces to "".
func sanitizeToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || toolNameRe.MatchString(name) {
		return name
	}
	return toolNameLeadingRe.FindString(name)
}

// finiteOrZero reports n's value and whether it is present and finite; a
// NULL or non-finite column reports (0, false).
func finiteOrZero(n sql.NullFloat64) (float64, bool) {
	if !n.Valid || math.IsNaN(n.Float64) || math.IsInf(n.Float64, 0) {
		return 0, false
	}
	return n.Float64, true
}

// numOrZero reads a token column as a whole number, treating anything NULL
// or non-finite as 0 rather than skipping the request.
func numOrZero(n sql.NullFloat64) int64 {
	f, ok := finiteOrZero(n)
	if !ok {
		return 0
	}
	return int64(math.Round(f))
}

func round6(f float64) float64 { return math.Round(f*1e6) / 1e6 }
