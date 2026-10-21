package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/history"
	"github.com/heapsgoodstudio/agent-observability/internal/overage"
	"github.com/heapsgoodstudio/agent-observability/internal/rules"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
)

// liveWindowS is the live cutoff: a session with an api_request event this
// recently counts as "live" and is read straight from
// VictoriaLogs instead of (ListSessions) overlaid onto, or (GetSession) used
// in place of, the static index/db row.
const liveWindowS = 600

// sessionsGeneratedAtKey is the meta table key internal/indexer writes the
// sessions index's own generation time under, epoch seconds as a string.
const sessionsGeneratedAtKey = "sessions_generated_at"

// notYetIndexedProject and liveSessionProject are placeholder project names
// for a session VictoriaLogs knows about that the indexer has not (yet)
// written a row for.
const (
	notYetIndexedProject = "(not yet indexed)"
	liveSessionProject   = "(live session)"
)

// sessionsListQuery collects the live overlay for the sessions list: one row
// per session with recent api_request activity.
const sessionsListQuery = `_time:24h event.name:api_request | stats by (session.id)` +
	` sum(cost_usd) cost, count() reqs, min(_time) first_t, max(_time) last_t`

// sessionsListQueryFor narrows the live overlay to one profile. The overlay
// has to be filtered alongside the db rows, not after them: a live session
// belonging to another profile has no db row to be filtered out by, so it
// would be synthesized as a "(not yet indexed)" row and appear under a filter
// that excludes it. The profile label is the same one OTEL_RESOURCE_ATTRIBUTES
// stamps, so it is present on every log line the collector wrote.
func sessionsListQueryFor(profile string) string {
	if profile == "" {
		return sessionsListQuery
	}
	return fmt.Sprintf(`_time:24h event.name:api_request profile:"%s" | stats by (session.id)`+
		` sum(cost_usd) cost, count() reqs, min(_time) first_t, max(_time) last_t`, profile)
}

const sessionsListLimit = 20000

// sessionProbeQuery, sessionEventsQuery, sessionToolsQuery and
// sessionPromptsQuery are the four VictoriaLogs queries behind one session's
// live detail.
func sessionProbeQuery(id string) string {
	return fmt.Sprintf(`session.id:"%s" event.name:api_request _time:%ds | stats count() n`, id, liveWindowS)
}

func sessionEventsQuery(id string) string {
	return fmt.Sprintf(`session.id:"%s" event.name:api_request | fields _time, cost_usd,`+
		` input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,`+
		` model, query_source, skill.name, profile`, id)
}

func sessionToolsQuery(id string) string {
	return fmt.Sprintf(`session.id:"%s" event.name:tool_result | stats by (tool_name) count() n`, id)
}

func sessionPromptsQuery(id string) string {
	return fmt.Sprintf(`session.id:"%s" event.name:user_prompt | stats count() n`, id)
}

const (
	sessionProbeLimit  = 10
	sessionEventsLimit = 20000
	sessionAuxLimit    = 10000 // vlQuery's own default limit, for the tools/prompts queries
)

// errSessionNotFound is GetSession's sentinel for "genuinely absent": no
// live activity and no static row, with no error clouding either check.
// server.go's obsService.GetSession maps this (via isSessionNotFound) to
// Connect's CodeNotFound; every other buildSession failure maps to
// CodeUnavailable, matching every other RPC's "all sources failed" handling.
var errSessionNotFound = errors.New("session not found")

func isSessionNotFound(err error) bool { return errors.Is(err, errSessionNotFound) }

/* Shared row conversion ----------------------------------------------------- */

func tokenTotalsToProto(t transcript.Tokens) *claudeobsv1.TokenTotals {
	return &claudeobsv1.TokenTotals{
		Input:         float64(t.Input),
		Output:        float64(t.Output),
		CacheRead:     float64(t.CacheRead),
		CacheCreation: float64(t.CacheCreation),
	}
}

func modelCostsToProto(models []transcript.ModelCost) []*claudeobsv1.ModelCost {
	if len(models) == 0 {
		return nil
	}
	out := make([]*claudeobsv1.ModelCost, len(models))
	for i, m := range models {
		out[i] = &claudeobsv1.ModelCost{Model: m.Model, CostUsd: m.CostUSD}
	}
	return out
}

func trajectoryToProto(t [][3]float64) []*claudeobsv1.TrajectoryPoint {
	if len(t) == 0 {
		return nil
	}
	out := make([]*claudeobsv1.TrajectoryPoint, len(t))
	for i, p := range t {
		out[i] = &claudeobsv1.TrajectoryPoint{Ts: p[0], CumCostUsd: p[1], ContextTokens: p[2]}
	}
	return out
}

// dbRowToProto converts one indexed db row into its wire shape. Trajectory
// and Tools carry no field on SessionRow itself (they only exist at
// GetSessionResponse's top level), so callers that need them read r
// directly.
func dbRowToProto(r db.SessionRow) *claudeobsv1.SessionRow {
	return &claudeobsv1.SessionRow{
		Id:       r.ID,
		Provider: providerOf(r), NativeId: r.NativeID, ParentId: r.ParentID, CostKnown: r.CostKnown || providerOf(r) == "claude", UsageKnown: r.UsageKnown || providerOf(r) == "claude", ReasoningTokens: float64(r.ReasoningTokens), ErrorCount: float64(r.ErrorCount), DurationMs: float64(r.DurationMS), ToolsKnown: r.ToolsKnown || providerOf(r) == "claude", ErrorsKnown: r.ErrorsKnown, DurationKnown: r.DurationKnown,
		Project:          r.Project,
		Workspace:        r.Workspace,
		Profile:          r.Profile,
		Start:            float64(r.Start),
		End:              float64(r.End),
		CostUsd:          r.CostUSD,
		Requests:         float64(r.Requests),
		Tokens:           tokenTotalsToProto(r.Tokens),
		Models:           modelCostsToProto(r.Models),
		SidechainCostUsd: r.SidechainCostUSD,
		ToolCalls:        float64(r.ToolCalls),
		UserTurns:        float64(r.UserTurns),
		Skills:           append([]string(nil), r.Skills...),
		McpServers:       append([]string(nil), r.MCPServers...),
		HasTranscript:    r.HasTranscript,
		Analysed:         r.Analysed,
	}
}

/* ListSessions --------------------------------------------------------------

Rows come straight from db.ListSessions plus the live overlay. Sorting,
filtering and any row cap are the frontend's own UI state, deliberately not
part of what this RPC contracts to return. */

// liveSummary is one row of sessionsListQuery's result.
type liveSummary struct {
	CostUSD, Requests, Start, End float64
}

// loadLiveSessions runs sessionsListQuery: session.id-less rows are dropped,
// and numeric fields that fail to parse read as 0.
func loadLiveSessions(ctx context.Context, mvl memoVL, profile string) (map[string]liveSummary, error) {
	rows, err := mvl.Query(ctx, sessionsListQueryFor(profile), sessionsListLimit)
	if err != nil {
		return nil, err
	}
	out := make(map[string]liveSummary, len(rows))
	for _, r := range rows {
		id := r["session.id"]
		if id == "" {
			continue
		}
		cost, _ := strconv.ParseFloat(r["cost"], 64)
		reqs, _ := strconv.ParseFloat(r["reqs"], 64)
		start, _ := parseVLTime(r["first_t"])
		end, _ := parseVLTime(r["last_t"])
		out[id] = liveSummary{CostUSD: cost, Requests: reqs, Start: start, End: end}
	}
	return out, nil
}

// mergeSessionRows overlays the live rows onto the indexed ones. Db rows keep
// their own order (already cost_usd descending from db.ListSessions); a live row
// overlays end/cost/requests (each maxed) onto a matching db row only when
// it is live, and never touches that row's start. A live-only id (not yet
// indexed) synthesizes a "(not yet indexed)" row regardless of liveness.
// Synthesized rows are appended after the indexed ones, sorted by id for a
// deterministic response (mergeRows' own order for these follows the VL
// response, which carries no ordering guarantee either).
func mergeSessionRows(dbRows []db.SessionRow, live map[string]liveSummary, now int64, profile string) []*claudeobsv1.SessionRow {
	byID := make(map[string]*claudeobsv1.SessionRow, len(dbRows)+len(live))
	order := make([]string, 0, len(dbRows))
	for _, r := range dbRows {
		byID[r.ID] = dbRowToProto(r)
		order = append(order, r.ID)
	}

	var extra []string
	for id, v := range live {
		isLive := float64(now)-v.End <= liveWindowS
		if row, ok := byID[id]; ok {
			if isLive {
				row.Live = true
				row.End = math.Max(row.End, v.End)
				row.CostUsd = math.Max(row.CostUsd, v.CostUSD)
				row.Requests = math.Max(row.Requests, v.Requests)
			}
			continue
		}
		byID[id] = &claudeobsv1.SessionRow{
			Id: id, Project: notYetIndexedProject, Workspace: notYetIndexedProject,
			// The live query has already matched this profile exactly. Carry
			// that known attribution until the indexer writes the static row.
			Profile: profile, Provider: "claude", CostKnown: true, UsageKnown: true,
			Start: v.Start, End: v.End, CostUsd: v.CostUSD, Requests: v.Requests,
			HasTranscript: true, Live: isLive,
		}
		extra = append(extra, id)
	}
	sort.Strings(extra)

	out := make([]*claudeobsv1.SessionRow, 0, len(order)+len(extra))
	for _, id := range order {
		out = append(out, byID[id])
	}
	for _, id := range extra {
		out = append(out, byID[id])
	}
	return out
}

/* Delivery counters ---------------------------------------------------------

Claude Code's own commit and pull request counters, read per session rather
than per project (internal/server/spend.go's buildDelivery reads the same two
series grouped the other way). Neither has a historical counterpart: a
transcript cannot say whether the command it ran opened a pull request, so
these begin wherever the collector did and are 0 for everything older. */

// deliveryWindow spans [earliest row start, now] on the daily grid, so a
// session's figure is its whole lifetime total rather than a recent slice.
// It floors the start to the day and adds a day of slack at each end, since
// increase() attributes a counter's rise to the bucket it lands in and a
// session straddling a bucket edge would otherwise lose the outside half.
func deliveryWindow(rows []*claudeobsv1.SessionRow, now int64) (history.Window, bool) {
	earliest := int64(0)
	for _, r := range rows {
		s := int64(r.Start)
		if s <= 0 {
			continue
		}
		if earliest == 0 || s < earliest {
			earliest = s
		}
	}
	if earliest == 0 {
		return history.Window{}, false
	}
	start := (earliest/history.DAY)*history.DAY - history.DAY
	end := now + history.DAY
	return history.Window{Start: start, End: end, Step: history.DAY}, true
}

// loadSessionDelivery returns (session id -> total) for both delivery
// counters over win. The two are fetched separately so one failing kind
// cannot blank the other.
func loadSessionDelivery(ctx context.Context, qr history.RangeQuerier, win history.Window) (commits, prs map[string]float64, err error) {
	commits, err = history.MergedTotals(ctx, qr, history.MergedParams{
		Kind: "commits", By: []string{"session.id"},
		Start: win.Start, End: win.End, Step: win.Step,
	})
	if err != nil {
		return nil, nil, err
	}
	prs, err = history.MergedTotals(ctx, qr, history.MergedParams{
		Kind: "pullRequests", By: []string{"session.id"},
		Start: win.Start, End: win.End, Step: win.Step,
	})
	if err != nil {
		return nil, nil, err
	}
	return commits, prs, nil
}

// applyDelivery writes each row's counters, rounding because increase() over
// a monotonic integer counter comes back with floating point dust that would
// otherwise render as "3" for 2.9999999999999996 only by luck of formatting.
func applyDelivery(rows []*claudeobsv1.SessionRow, commits, prs map[string]float64) {
	for _, r := range rows {
		r.Commits = math.Round(commits[r.Id])
		r.PullRequests = math.Round(prs[r.Id])
	}
}

// applyBilledOverlap sets each row's billed_overlap, which gates the "real $"
// badge: only a row whose own end falls on/after the overage series' observed
// start, and whose span overlaps a band, is marked. A row that predates the
// series cannot be judged, so it is left unmarked rather than marked false.
func applyBilledOverlap(rows []*claudeobsv1.SessionRow, bands [][2]float64, seriesStart float64, hasStart bool) {
	for _, row := range rows {
		observed := hasStart && row.End >= seriesStart
		row.BilledOverlap = observed && overage.SpanOverlapsBands(row.Start, row.End, bands)
	}
}

// buildListSessions is ListSessions' real body. The db, VictoriaLogs and
// VictoriaMetrics (overage) sources each degrade independently to a
// SectionError; only when neither the db nor VictoriaLogs answered is there
// nothing left to return, which is the one case that fails the whole call.
// profile, when non-empty, narrows both halves: db.ListSessions filters in SQL
// and the VictoriaLogs overlay filters on the same label. Both have to narrow
// or the two disagree, and the row cap the frontend applies would then be
// spent on rows the reader excluded.
func buildListSessions(ctx context.Context, o Options, profile string, now int64) (*claudeobsv1.ListSessionsResponse, error) {
	resp := &claudeobsv1.ListSessionsResponse{Identity: identity()}
	var errs []*claudeobsv1.SectionError

	var dbRows []db.SessionRow
	dbOK := true
	if o.DB == nil {
		dbOK = false
		errs = append(errs, &claudeobsv1.SectionError{Section: "db", Error: "no database configured"})
	} else {
		var err error
		dbRows, err = db.ListSessions(o.DB, profile)
		if err != nil {
			dbOK = false
			errs = append(errs, &claudeobsv1.SectionError{Section: "db", Error: err.Error()})
		}
	}

	mvl := memoVL{client: o.VL, memo: o.Memo}
	live, liveErr := loadLiveSessions(ctx, mvl, profile)
	if liveErr != nil {
		errs = append(errs, &claudeobsv1.SectionError{Section: "live", Error: liveErr.Error()})
	}

	if !dbOK && liveErr != nil {
		return nil, fmt.Errorf("sessions: neither the database nor VictoriaLogs is reachable")
	}

	claudeRows := dbRows[:0]
	for _, r := range dbRows {
		if providerOf(r) == "claude" {
			claudeRows = append(claudeRows, r)
		}
	}
	rows := mergeSessionRows(claudeRows, live, now, profile)

	mvm := memoVM{client: o.VM, memo: o.Memo}
	bands, seriesStart, hasStart, overageErr := overage.Load(ctx, mvm, mvm, now)
	if overageErr != nil {
		errs = append(errs, &claudeobsv1.SectionError{Section: "overage", Error: overageErr.Error()})
	}
	applyBilledOverlap(rows, bands, seriesStart, hasStart)

	if win, ok := deliveryWindow(rows, now); ok {
		commits, prs, derr := loadSessionDelivery(ctx, mvm, win)
		if derr != nil {
			errs = append(errs, &claudeobsv1.SectionError{Section: "delivery", Error: derr.Error()})
		} else {
			applyDelivery(rows, commits, prs)
		}
	}

	resp.Sessions = rows
	resp.OverageObserved = hasStart

	var generatedAt int64
	var hasGeneratedAt bool
	if o.DB != nil {
		if v, ok, err := db.MetaGet(o.DB, sessionsGeneratedAtKey); err != nil {
			errs = append(errs, &claudeobsv1.SectionError{Section: "meta", Error: err.Error()})
		} else if ok {
			if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
				generatedAt, hasGeneratedAt = n, true
			}
		}
	}
	if hasGeneratedAt {
		resp.GeneratedAt = time.Unix(generatedAt, 0).UTC().Format(time.RFC3339)
	}
	resp.IndexStale = !hasGeneratedAt || now-generatedAt > indexStaleS

	resp.Errors = errs
	return resp, nil
}

/* GetSession -----------------------------------------------------------------

One session's detail: the static row from the db, recomputed live when the
session has recent VictoriaLogs activity, and the two merged. */

// parseVLTimeMillis parses a VictoriaLogs `_time` value to millisecond
// precision (UnixMilli), unlike parseVLTime's whole-second floor.
// loadLiveSession's event sort needs this: two events landing in the same
// second floor to an identical key under parseVLTime, so a comparison keyed
// on that floor cannot tell them apart, and raw string comparison of `_time`
// fares no better (lexicographic order breaks once one fraction is a prefix
// of the other, e.g. ".5" vs ".51"). Parsing once to milliseconds and
// sorting on that avoids both.
func parseVLTimeMillis(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return 0, false
		}
	}
	return t.UnixMilli(), true
}

// loadLiveSession recomputes one session's detail from VictoriaLogs. ok is
// false with a nil error when the session simply has no recent activity (use
// the static row instead); err is non-nil only for a genuine VictoriaLogs
// failure.
func loadLiveSession(ctx context.Context, mvl memoVL, id string, now int64) (row *claudeobsv1.SessionRow, tools map[string]int, trajectory [][3]float64, ok bool, err error) {
	probeRows, err := mvl.Query(ctx, sessionProbeQuery(id), sessionProbeLimit)
	if err != nil {
		return nil, nil, nil, false, err
	}
	var n float64
	if len(probeRows) > 0 {
		n, _ = strconv.ParseFloat(probeRows[0]["n"], 64)
	}
	if n == 0 {
		return nil, nil, nil, false, nil
	}

	evs, err := mvl.Query(ctx, sessionEventsQuery(id), sessionEventsLimit)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if len(evs) == 0 {
		return nil, nil, nil, false, nil
	}
	// Parse each event's timestamp once to millisecond precision, then sort
	// stably on that key: parseVLTime's whole-second floor (used below only
	// for `end`, which stays in seconds) loses sub-second order between
	// events that land in the same second, and a plain sort.Slice is not
	// guaranteed stable on ties either.
	type timedEvent struct {
		row map[string]string
		ms  int64
	}
	timed := make([]timedEvent, len(evs))
	for i, e := range evs {
		ms, _ := parseVLTimeMillis(e["_time"])
		timed[i] = timedEvent{row: e, ms: ms}
	}
	sort.SliceStable(timed, func(i, j int) bool { return timed[i].ms < timed[j].ms })
	for i, te := range timed {
		evs[i] = te.row
	}
	end, _ := parseVLTime(evs[len(evs)-1]["_time"])
	if float64(now)-end > liveWindowS {
		return nil, nil, nil, false, nil
	}

	toolRows, err := mvl.Query(ctx, sessionToolsQuery(id), sessionAuxLimit)
	if err != nil {
		return nil, nil, nil, false, err
	}
	promptRows, err := mvl.Query(ctx, sessionPromptsQuery(id), sessionAuxLimit)
	if err != nil {
		return nil, nil, nil, false, err
	}

	var tokIn, tokOut, tokCacheRead, tokCacheCreation float64
	var modelOrder []string
	modelCost := map[string]float64{}
	skillSet := map[string]bool{}
	profileIDs := map[string]bool{}
	trajectory = make([][3]float64, 0, len(evs))
	var cost, side float64
	for _, e := range evs {
		if profile := e["profile"]; profile != "" {
			profileIDs[profile] = true
		}
		c, _ := strconv.ParseFloat(e["cost_usd"], 64)
		cost += c
		// Sidechain is everything the main thread did not ask for directly.
		// isMainThreadSource comes from the shared rules classifier, so this
		// and the Findings rules cannot disagree about what the main thread
		// is.
		if src := e["query_source"]; src != "" && !rules.IsMainThreadSource(src) {
			side += c
		}
		if m := e["model"]; m != "" {
			if _, seen := modelCost[m]; !seen {
				modelOrder = append(modelOrder, m)
			}
			modelCost[m] += c
		}
		inTok, _ := strconv.ParseFloat(e["input_tokens"], 64)
		outTok, _ := strconv.ParseFloat(e["output_tokens"], 64)
		crTok, _ := strconv.ParseFloat(e["cache_read_tokens"], 64)
		ccTok, _ := strconv.ParseFloat(e["cache_creation_tokens"], 64)
		tokIn += inTok
		tokOut += outTok
		tokCacheRead += crTok
		tokCacheCreation += ccTok
		if sk := e["skill.name"]; sk != "" {
			skillSet[sk] = true
		}
		ts, _ := parseVLTime(e["_time"])
		contextTokens := inTok + crTok + ccTok
		// Title generation and background/subagent requests have their own
		// prompts. Missing source retains compatibility with older telemetry.
		if src := e["query_source"]; src == "generate_session_title" || (src != "" && !rules.IsMainThreadSource(src)) {
			contextTokens = 0
		}
		trajectory = append(trajectory, [3]float64{ts, cost, contextTokens})
	}

	tools = map[string]int{}
	var toolCalls float64
	for _, t := range toolRows {
		name := t["tool_name"]
		if name == "" {
			continue
		}
		nf, _ := strconv.ParseFloat(t["n"], 64)
		tools[name] = int(nf)
		toolCalls += nf
	}

	var userTurns float64
	if len(promptRows) > 0 {
		userTurns, _ = strconv.ParseFloat(promptRows[0]["n"], 64)
	}

	skills := make([]string, 0, len(skillSet))
	for s := range skillSet {
		skills = append(skills, s)
	}
	sort.Strings(skills)

	models := make([]*claudeobsv1.ModelCost, len(modelOrder))
	for i, m := range modelOrder {
		models[i] = &claudeobsv1.ModelCost{Model: m, CostUsd: modelCost[m]}
	}

	row = &claudeobsv1.SessionRow{
		Id:       id,
		Provider: "claude", CostKnown: true, UsageKnown: true, ToolsKnown: true,
		Project: liveSessionProject,
		Start:   trajectory[0][0],
		End:     end,
		CostUsd: cost, Requests: float64(len(evs)),
		Tokens: &claudeobsv1.TokenTotals{
			Input: tokIn, Output: tokOut, CacheRead: tokCacheRead, CacheCreation: tokCacheCreation,
		},
		Models:           models,
		SidechainCostUsd: side,
		ToolCalls:        toolCalls,
		UserTurns:        userTurns,
		Skills:           skills,
		HasTranscript:    true,
		Live:             true,
	}
	// Telemetry carries the stable registry ID, including for sessions that
	// have not been indexed yet. Conflicting labels cannot establish an owner.
	if len(profileIDs) == 1 {
		for profile := range profileIDs {
			row.Profile = profile
		}
	}
	return row, tools, trajectory, true, nil
}

// buildSession is GetSession's real body. errSessionNotFound is the one
// error server.go maps to Connect's CodeNotFound (an id neither live nor the
// db has ever heard of); any other failure (a genuine db error with no live
// data to fall back on) maps to CodeUnavailable like every other RPC.
func buildSession(ctx context.Context, o Options, id string, now int64) (*claudeobsv1.GetSessionResponse, error) {
	resp := &claudeobsv1.GetSessionResponse{Identity: identity()}
	var errs []*claudeobsv1.SectionError

	mvl := memoVL{client: o.VL, memo: o.Memo}
	row, tools, trajectory, live, liveErr := loadLiveSession(ctx, mvl, id, now)
	if liveErr != nil {
		errs = append(errs, &claudeobsv1.SectionError{Section: "live", Error: liveErr.Error()})
	}

	var staticRow *db.SessionRow
	var staticOK bool
	var staticErr error
	if o.DB == nil {
		staticErr = errors.New("no database configured")
	} else {
		staticRow, staticOK, staticErr = db.GetSession(o.DB, id)
	}
	if staticErr != nil {
		errs = append(errs, &claudeobsv1.SectionError{Section: "db", Error: staticErr.Error()})
	}

	switch {
	case live && staticOK:
		// Preserve indexed identity when refreshing live measurements. Profile
		// is a stable registry ID, never the editable display name. Older
		// unattributed rows may still gain an ID from labelled live events.
		row.Provider = providerOf(*staticRow)
		row.NativeId = staticRow.NativeID
		row.ParentId = staticRow.ParentID
		if staticRow.Profile != "" {
			row.Profile = staticRow.Profile
		}
		row.Project = staticRow.Project
		row.Workspace = staticRow.Workspace
		row.Start = math.Min(row.Start, float64(staticRow.Start))
		row.CostUsd = math.Max(row.CostUsd, staticRow.CostUSD)
		row.Requests = math.Max(row.Requests, float64(staticRow.Requests))
	case live:
		// Not indexed yet (or the db could not be read): the pure live row
		// stands on its own.
	case staticOK:
		row = dbRowToProto(*staticRow)
		tools = staticRow.Tools
		trajectory = staticRow.Trajectory
	default:
		if staticErr == nil {
			return nil, errSessionNotFound
		}
		return nil, fmt.Errorf("session %s: no source available: %w", id, staticErr)
	}

	resp.Row = row
	resp.Live = live
	toolsProto := make(map[string]int64, len(tools))
	for k, v := range tools {
		toolsProto[k] = int64(v)
	}
	resp.Tools = toolsProto
	resp.Trajectory = trajectoryToProto(trajectory)

	mvm := memoVM{client: o.VM, memo: o.Memo}
	bands, seriesStart, hasStart, overageErr := overage.Load(ctx, mvm, mvm, now)
	if overageErr != nil {
		errs = append(errs, &claudeobsv1.SectionError{Section: "overage", Error: overageErr.Error()})
	}
	// Unlike ListSessions' response-level OverageObserved (there is no
	// single session to gate on there), GetSession has exactly one row, so
	// OverageObserved is that row's own observed gate: hasStart alone would
	// let a consumer print real_cost_usd's zero-value as a genuine "$0.00"
	// for a session that ended before the series started. Anything that ended
	// before the series starts must show nothing, never a zero.
	observed := hasStart && row.End >= seriesStart
	resp.OverageObserved = observed
	if observed {
		resp.RealCostUsd = overage.CostInBands(trajectory, bands)
	}
	row.BilledOverlap = observed && overage.SpanOverlapsBands(row.Start, row.End, bands)

	if win, ok := deliveryWindow([]*claudeobsv1.SessionRow{row}, now); ok {
		commits, prs, derr := loadSessionDelivery(ctx, mvm, win)
		if derr != nil {
			errs = append(errs, &claudeobsv1.SectionError{Section: "delivery", Error: derr.Error()})
		} else {
			applyDelivery([]*claudeobsv1.SessionRow{row}, commits, prs)
		}
	}

	resp.Errors = errs
	return resp, nil
}
