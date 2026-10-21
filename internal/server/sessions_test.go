package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1/claudeobsv1connect"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vl"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// openTestDB opens a fresh, migrated temp sqlite db, mirroring internal/db's
// own openMigrated helper (that one lives in package db and isn't exported).
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "claude-obs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	return d
}

func sessionsOptions(vmSrv, vlSrv *httptest.Server, d *sql.DB) Options {
	o := Options{Memo: store.New(store.TTL), DB: d}
	if vmSrv != nil {
		o.VM = &vm.Client{BaseURL: vmSrv.URL, HTTP: vmSrv.Client()}
	}
	if vlSrv != nil {
		o.VL = &vl.Client{BaseURL: vlSrv.URL, HTTP: vlSrv.Client()}
	}
	return o
}

func rfc3339(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format(time.RFC3339)
}

// emptyOverageVM answers overage.Load's two queries with "nothing observed":
// no bands, no series start. Tests that don't care about billed_overlap use
// this so hasStart comes back false rather than an error.
func emptyOverageVM(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(fakeVMHandler{
		ranged: map[string]string{"max(claude_in_overage) == 1": successBody(t)},
		instant: map[string]string{
			"min_over_time(timestamp(claude_in_overage)[45d:10m])": successBody(t),
		},
	})
}

const testNow = int64(1755300000)

/* ListSessions -------------------------------------------------------------- */

// A live session already in the index: VL's fresher end/cost/requests must
// overlay onto the indexed row, maxed against the static figures, and start
// must never move.
func TestBuildListSessionsLiveOverlayMaxes(t *testing.T) {
	d := openTestDB(t)
	if err := db.UpsertSession(d, db.SessionRow{
		ID: "s1", Project: "proj-a", Start: 1755000000, End: 1755000500,
		CostUSD: 5, Requests: 3, HasTranscript: true,
	}); err != nil {
		t.Fatal(err)
	}

	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
		sessionsListQuery: ndjson(map[string]string{
			"session.id": "s1", "cost": "8", "reqs": "5",
			"first_t": rfc3339(1755000000 - 10000), // must be ignored: start never moves
			"last_t":  rfc3339(testNow - 100),      // recent: live
		}),
	}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "", testNow)
	if err != nil {
		t.Fatalf("buildListSessions: %v", err)
	}
	if len(resp.Sessions) != 1 {
		t.Fatalf("len(Sessions) = %d, want 1", len(resp.Sessions))
	}
	row := resp.Sessions[0]
	if !row.Live {
		t.Error("Live = false, want true")
	}
	if row.Start != 1755000000 {
		t.Errorf("Start = %v, want unchanged 1755000000", row.Start)
	}
	if row.End != float64(testNow-100) {
		t.Errorf("End = %v, want max()'d to %v", row.End, testNow-100)
	}
	if row.CostUsd != 8 {
		t.Errorf("CostUsd = %v, want max(5,8)=8", row.CostUsd)
	}
	if row.Requests != 5 {
		t.Errorf("Requests = %v, want max(3,5)=5", row.Requests)
	}
	if row.Project != "proj-a" {
		t.Errorf("Project = %q, want proj-a (untouched by live overlay)", row.Project)
	}
}

// A stale VL row (its last_t older than the 600s live window) must not
// overlay onto its matching indexed row at all: the row comes back exactly
// as the db has it, not merely "not marked live" while some figures still
// leak through. This is what the isLive guard in mergeSessionRows exists
// for; deleting that guard would let the max()'d live figures overlay
// regardless of staleness, which this test must catch.
func TestBuildListSessionsStaleLiveOverlaySkipped(t *testing.T) {
	d := openTestDB(t)
	if err := db.UpsertSession(d, db.SessionRow{
		ID: "s1", Project: "proj-a", Start: 1755000000, End: 1755000500,
		CostUSD: 5, Requests: 3, HasTranscript: true,
	}); err != nil {
		t.Fatal(err)
	}

	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
		sessionsListQuery: ndjson(map[string]string{
			"session.id": "s1", "cost": "8", "reqs": "5",
			"first_t": rfc3339(1755000000),
			"last_t":  rfc3339(testNow - liveWindowS - 100), // stale: past the 600s live window
		}),
	}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "", testNow)
	if err != nil {
		t.Fatalf("buildListSessions: %v", err)
	}
	if len(resp.Sessions) != 1 {
		t.Fatalf("len(Sessions) = %d, want 1", len(resp.Sessions))
	}
	row := resp.Sessions[0]
	if row.Live {
		t.Error("Live = true, want false (last_t is older than the live window)")
	}
	if row.End != 1755000500 {
		t.Errorf("End = %v, want the static 1755000500 untouched (no overlay when stale)", row.End)
	}
	if row.CostUsd != 5 {
		t.Errorf("CostUsd = %v, want the static 5 untouched (no overlay when stale)", row.CostUsd)
	}
	if row.Requests != 3 {
		t.Errorf("Requests = %v, want the static 3 untouched (no overlay when stale)", row.Requests)
	}
}

// A session with VL activity but not (yet) in the index synthesizes a
// "(not yet indexed)" row; whether it is live depends solely on how recently
// its own last event landed, independent of any other row.
func TestBuildListSessionsSynthesizesNotYetIndexedRow(t *testing.T) {
	d := openTestDB(t)

	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
		sessionsListQuery: ndjson(
			map[string]string{
				"session.id": "s-live", "cost": "2.5", "reqs": "4",
				"first_t": rfc3339(testNow - 300), "last_t": rfc3339(testNow - 50),
			},
			map[string]string{
				"session.id": "s-stale", "cost": "1", "reqs": "1",
				"first_t": rfc3339(testNow - 2000), "last_t": rfc3339(testNow - 1000),
			},
		),
	}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "", testNow)
	if err != nil {
		t.Fatalf("buildListSessions: %v", err)
	}
	if len(resp.Sessions) != 2 {
		t.Fatalf("len(Sessions) = %d, want 2", len(resp.Sessions))
	}

	byID := map[string]*claudeobsv1.SessionRow{}
	for _, r := range resp.Sessions {
		byID[r.Id] = r
	}

	live := byID["s-live"]
	if live == nil {
		t.Fatal("s-live missing from Sessions")
	}
	if live.Project != "(not yet indexed)" {
		t.Errorf("s-live Project = %q, want (not yet indexed)", live.Project)
	}
	if !live.Live {
		t.Error("s-live Live = false, want true")
	}
	if live.CostUsd != 2.5 || live.Requests != 4 {
		t.Errorf("s-live cost/reqs = %v/%v, want 2.5/4", live.CostUsd, live.Requests)
	}
	if live.Start != float64(testNow-300) || live.End != float64(testNow-50) {
		t.Errorf("s-live start/end = %v/%v", live.Start, live.End)
	}
	if !live.HasTranscript {
		t.Error("s-live HasTranscript = false, want true")
	}
	if live.Analysed {
		t.Error("s-live Analysed = true, want false")
	}

	stale := byID["s-stale"]
	if stale == nil {
		t.Fatal("s-stale missing from Sessions")
	}
	if stale.Project != "(not yet indexed)" {
		t.Errorf("s-stale Project = %q, want (not yet indexed)", stale.Project)
	}
	if stale.Live {
		t.Error("s-stale Live = true, want false (last event over LIVE_WINDOW_S ago)")
	}
}

// billed_overlap: gated on the session's own end falling on/after the
// overage series' observed start, then on the span actually overlapping a
// band.
func TestBuildListSessionsBilledOverlap(t *testing.T) {
	d := openTestDB(t)
	const seriesStart = int64(1755000000)
	bandStart, bandEnd := int64(1755100000), int64(1755100500)

	rows := []db.SessionRow{
		{ID: "overlap", Project: "p", Start: 1755099900, End: 1755100200, HasTranscript: true},
		{ID: "no-overlap", Project: "p", Start: 1755200000, End: 1755200500, HasTranscript: true},
		{ID: "before-series", Project: "p", Start: 1754900000, End: 1754950000, HasTranscript: true},
	}
	for _, r := range rows {
		if err := db.UpsertSession(d, r); err != nil {
			t.Fatal(err)
		}
	}

	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{}})
	defer vlSrv.Close()
	vmSrv := httptest.NewServer(fakeVMHandler{
		ranged: map[string]string{
			"max(claude_in_overage) == 1": successBody(t,
				vmResult{Metric: map[string]string{}, Values: [][2]any{{float64(bandStart), "1"}, {float64(bandEnd), "1"}}}),
		},
		instant: map[string]string{
			"min_over_time(timestamp(claude_in_overage)[45d:10m])": successBody(t,
				vmResult{Metric: map[string]string{}, Value: []any{0, strconv.FormatInt(seriesStart, 10)}}),
		},
	})
	defer vmSrv.Close()

	resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "", testNow)
	if err != nil {
		t.Fatalf("buildListSessions: %v", err)
	}
	if !resp.OverageObserved {
		t.Error("OverageObserved = false, want true (series start resolved)")
	}

	byID := map[string]*claudeobsv1.SessionRow{}
	for _, r := range resp.Sessions {
		byID[r.Id] = r
	}
	if !byID["overlap"].BilledOverlap {
		t.Error("overlap.BilledOverlap = false, want true")
	}
	if byID["no-overlap"].BilledOverlap {
		t.Error("no-overlap.BilledOverlap = true, want false (span misses every band)")
	}
	if byID["before-series"].BilledOverlap {
		t.Error("before-series.BilledOverlap = true, want false (predates the observed series)")
	}
}

/* Delivery counters --------------------------------------------------------- */

// Matched by substring, not exact text: these counters are read with
// max_over_time over a lookbehind spanning the whole request, so the query's
// duration differs with whatever window the caller built.
const (
	commitsLiveQuery = `claude_code.commit.count`
	prsLiveQuery     = `claude_code.pull_request.count`
)

// deliveryVM answers both delivery range queries with per-session totals, and
// everything else (including overage) with an empty success result.
func deliveryVM(t *testing.T, commits, prs map[string]string) *httptest.Server {
	t.Helper()
	build := func(m map[string]string) string {
		results := make([]vmResult, 0, len(m))
		for id, v := range m {
			results = append(results, vmResult{
				Metric: map[string]string{"session.id": id},
				Values: [][2]any{{float64(1755000000), v}},
			})
		}
		return successBody(t, results...)
	}
	return httptest.NewServer(fakeVMHandler{
		rangedContains: map[string]string{
			commitsLiveQuery: build(commits),
			prsLiveQuery:     build(prs),
		},
	})
}

func TestBuildListSessionsDeliveryCounters(t *testing.T) {
	d := openTestDB(t)
	for _, r := range []db.SessionRow{
		{ID: "shipped", Project: "p", Start: 1755000000, End: 1755000500, HasTranscript: true},
		{ID: "quiet", Project: "p", Start: 1755000000, End: 1755000500, HasTranscript: true},
	} {
		if err := db.UpsertSession(d, r); err != nil {
			t.Fatal(err)
		}
	}

	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{}})
	defer vlSrv.Close()
	// The counter value carries floating point dust, which must round to a
	// whole number rather than reaching the client as 2.9999999999999996.
	vmSrv := deliveryVM(t,
		map[string]string{"shipped": "2.9999999999999996"},
		map[string]string{"shipped": "1"},
	)
	defer vmSrv.Close()

	resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "", testNow)
	if err != nil {
		t.Fatalf("buildListSessions: %v", err)
	}
	byID := map[string]*claudeobsv1.SessionRow{}
	for _, r := range resp.Sessions {
		byID[r.Id] = r
	}
	if byID["shipped"].Commits != 3 {
		t.Errorf("shipped.Commits = %v, want 3", byID["shipped"].Commits)
	}
	if byID["shipped"].PullRequests != 1 {
		t.Errorf("shipped.PullRequests = %v, want 1", byID["shipped"].PullRequests)
	}
	// A session the counters never mentioned reads 0, which the client shows
	// as blank: these counters have no history, so 0 means "not recorded"
	// just as often as it means "made none".
	if byID["quiet"].Commits != 0 || byID["quiet"].PullRequests != 0 {
		t.Errorf("quiet = (%v, %v), want (0, 0)", byID["quiet"].Commits, byID["quiet"].PullRequests)
	}
}

// A failing delivery query degrades to its own SectionError and leaves every
// other figure on the row intact, the same way the overage source does.
func TestBuildListSessionsDeliveryDegrades(t *testing.T) {
	d := openTestDB(t)
	if err := db.UpsertSession(d, db.SessionRow{
		ID: "s1", Project: "p", Start: 1755000000, End: 1755000500,
		CostUSD: 5, Requests: 3, HasTranscript: true,
	}); err != nil {
		t.Fatal(err)
	}

	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{}})
	defer vlSrv.Close()
	vmSrv := httptest.NewServer(fakeVMHandler{failContains: commitsLiveQuery})
	defer vmSrv.Close()

	resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "", testNow)
	if err != nil {
		t.Fatalf("buildListSessions: %v", err)
	}
	var found bool
	for _, e := range resp.Errors {
		if e.Section == "delivery" {
			found = true
		}
	}
	if !found {
		t.Errorf("no delivery SectionError, got %+v", resp.Errors)
	}
	if len(resp.Sessions) != 1 || resp.Sessions[0].CostUsd != 5 {
		t.Errorf("rows damaged by the delivery failure: %+v", resp.Sessions)
	}
}

// index_stale flips once the session index's own generated_at is more than
// 900s (indexStaleS) old; a fresh generated_at reads as not stale.
func TestBuildListSessionsIndexStale(t *testing.T) {
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	t.Run("fresh", func(t *testing.T) {
		d := openTestDB(t)
		if err := db.MetaSet(d, "sessions_generated_at", strconv.FormatInt(testNow-100, 10)); err != nil {
			t.Fatal(err)
		}
		resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "", testNow)
		if err != nil {
			t.Fatal(err)
		}
		if resp.IndexStale {
			t.Error("IndexStale = true, want false")
		}
		want := rfc3339(testNow - 100)
		if resp.GeneratedAt != want {
			t.Errorf("GeneratedAt = %q, want %q", resp.GeneratedAt, want)
		}
	})

	t.Run("stale", func(t *testing.T) {
		d := openTestDB(t)
		if err := db.MetaSet(d, "sessions_generated_at", strconv.FormatInt(testNow-1000, 10)); err != nil {
			t.Fatal(err)
		}
		resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "", testNow)
		if err != nil {
			t.Fatal(err)
		}
		if !resp.IndexStale {
			t.Error("IndexStale = false, want true")
		}
	})

	t.Run("missing meta", func(t *testing.T) {
		d := openTestDB(t)
		resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "", testNow)
		if err != nil {
			t.Fatal(err)
		}
		if !resp.IndexStale {
			t.Error("IndexStale = false, want true when generated_at is unknown")
		}
		if resp.GeneratedAt != "" {
			t.Errorf("GeneratedAt = %q, want empty when generated_at is unknown", resp.GeneratedAt)
		}
	})
}

// Fail-soft degradation: a down VL, a nil DB, and both down together.
func TestBuildListSessionsDegradation(t *testing.T) {
	t.Run("VL down: db rows still returned, with a SectionError", func(t *testing.T) {
		d := openTestDB(t)
		if err := db.UpsertSession(d, db.SessionRow{ID: "s1", Project: "p", HasTranscript: true}); err != nil {
			t.Fatal(err)
		}
		vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{}, fail: sessionsListQuery})
		defer vlSrv.Close()
		vmSrv := emptyOverageVM(t)
		defer vmSrv.Close()

		resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "", testNow)
		if err != nil {
			t.Fatalf("buildListSessions: %v", err)
		}
		if len(resp.Sessions) != 1 {
			t.Fatalf("len(Sessions) = %d, want 1 (db row survives a VL outage)", len(resp.Sessions))
		}
		if !hasSectionError(resp.Errors, "live") {
			t.Errorf("Errors = %+v, want a live SectionError", resp.Errors)
		}
	})

	t.Run("nil DB: live rows still returned, with a SectionError", func(t *testing.T) {
		vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
			sessionsListQuery: ndjson(map[string]string{
				"session.id": "s1", "cost": "1", "reqs": "1",
				"first_t": rfc3339(testNow - 10), "last_t": rfc3339(testNow - 5),
			}),
		}})
		defer vlSrv.Close()
		vmSrv := emptyOverageVM(t)
		defer vmSrv.Close()

		resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, nil), "", testNow)
		if err != nil {
			t.Fatalf("buildListSessions: %v", err)
		}
		if len(resp.Sessions) != 1 {
			t.Fatalf("len(Sessions) = %d, want 1 (live row survives a missing db)", len(resp.Sessions))
		}
		if !hasSectionError(resp.Errors, "db") {
			t.Errorf("Errors = %+v, want a db SectionError", resp.Errors)
		}
	})

	t.Run("both unavailable: hard error", func(t *testing.T) {
		vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{}, fail: sessionsListQuery})
		defer vlSrv.Close()
		vmSrv := emptyOverageVM(t)
		defer vmSrv.Close()

		_, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, nil), "", testNow)
		if err == nil {
			t.Fatal("buildListSessions: want an error when neither the db nor VL is reachable")
		}
	})
}

func hasSectionError(errs []*claudeobsv1.SectionError, section string) bool {
	for _, e := range errs {
		if e.Section == section {
			return true
		}
	}
	return false
}

/* GetSession ------------------------------------------------------------- */

// A pure live session (not yet indexed): tokens, sidechain cost (via
// rules.IsMainThreadSource), models, tools, prompts and trajectory must all
// be built straight from the four session.js VL queries.
func TestBuildSessionLiveDetailConstruction(t *testing.T) {
	d := openTestDB(t)
	const id = "live1"
	t1, t2 := testNow-500, testNow-400

	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
		sessionProbeQuery(id): ndjson(map[string]string{"n": "2"}),
		sessionEventsQuery(id): ndjson(
			map[string]string{
				"_time": rfc3339(t1), "cost_usd": "1.5", "input_tokens": "100", "output_tokens": "50",
				"cache_read_tokens": "10", "cache_creation_tokens": "5",
				"model": "claude-sonnet-5", "query_source": "main",
			},
			map[string]string{
				"_time": rfc3339(t2), "cost_usd": "0.5", "input_tokens": "20", "output_tokens": "10",
				"cache_read_tokens": "2", "cache_creation_tokens": "1",
				"model": "claude-sonnet-5", "query_source": "agent:custom", "skill.name": "usage-report",
			},
		),
		sessionToolsQuery(id): ndjson(
			map[string]string{"tool_name": "Bash", "n": "3"},
			map[string]string{"tool_name": "Read", "n": "2"},
		),
		sessionPromptsQuery(id): ndjson(map[string]string{"n": "7"}),
	}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	resp, err := buildSession(context.Background(), sessionsOptions(vmSrv, vlSrv, d), id, testNow)
	if err != nil {
		t.Fatalf("buildSession: %v", err)
	}
	if !resp.Live {
		t.Error("Live = false, want true")
	}
	row := resp.Row
	if row.Project != "(live session)" {
		t.Errorf("Project = %q, want (live session)", row.Project)
	}
	if row.Start != float64(t1) || row.End != float64(t2) {
		t.Errorf("Start/End = %v/%v, want %v/%v", row.Start, row.End, t1, t2)
	}
	if row.CostUsd != 2.0 {
		t.Errorf("CostUsd = %v, want 2.0", row.CostUsd)
	}
	if row.Requests != 2 {
		t.Errorf("Requests = %v, want 2", row.Requests)
	}
	// "main" is a main-thread source (rules.IsMainThreadSource); its cost
	// must not count as sidechain. "agent:custom" is not, so its 0.5 does.
	if row.SidechainCostUsd != 0.5 {
		t.Errorf("SidechainCostUsd = %v, want 0.5", row.SidechainCostUsd)
	}
	if row.Tokens == nil || row.Tokens.Input != 120 || row.Tokens.Output != 60 ||
		row.Tokens.CacheRead != 12 || row.Tokens.CacheCreation != 6 {
		t.Errorf("Tokens = %+v, want {120,60,12,6}", row.Tokens)
	}
	if len(row.Models) != 1 || row.Models[0].Model != "claude-sonnet-5" || row.Models[0].CostUsd != 2.0 {
		t.Errorf("Models = %+v, want one claude-sonnet-5 row of 2.0", row.Models)
	}
	if len(row.Skills) != 1 || row.Skills[0] != "usage-report" {
		t.Errorf("Skills = %+v, want [usage-report]", row.Skills)
	}
	if row.ToolCalls != 5 {
		t.Errorf("ToolCalls = %v, want 5", row.ToolCalls)
	}
	if row.UserTurns != 7 {
		t.Errorf("UserTurns = %v, want 7", row.UserTurns)
	}
	if !row.HasTranscript {
		t.Error("HasTranscript = false, want true")
	}
	if resp.Tools["Bash"] != 3 || resp.Tools["Read"] != 2 {
		t.Errorf("Tools = %+v, want {Bash:3, Read:2}", resp.Tools)
	}
	if len(resp.Trajectory) != 2 {
		t.Fatalf("len(Trajectory) = %d, want 2", len(resp.Trajectory))
	}
	if resp.Trajectory[0].Ts != float64(t1) || resp.Trajectory[0].CumCostUsd != 1.5 || resp.Trajectory[0].ContextTokens != 115 {
		t.Errorf("Trajectory[0] = %+v, want {%v,1.5,115}", resp.Trajectory[0], t1)
	}
	if resp.Trajectory[1].Ts != float64(t2) || resp.Trajectory[1].CumCostUsd != 2.0 || resp.Trajectory[1].ContextTokens != 0 {
		t.Errorf("Trajectory[1] = %+v, want {%v,2.0,0}", resp.Trajectory[1], t2)
	}
}

// Two events land in the same whole second, and VL returns them in reverse
// (real) order: the 900ms event first, the 100ms event second. Sorting on
// parseVLTime's whole-second floor cannot tell them apart (both floor to the
// same second), so it must leave VL's own order standing; sorting on
// millisecond precision must reorder them correctly. The trajectory's
// cumulative cost at each point is the tell: sorted correctly, the first
// point reflects only the 100ms (cost 1) event; sorted wrongly, it reflects
// the 900ms (cost 3) event instead.
func TestBuildSessionLiveEventSortMillisecondOrder(t *testing.T) {
	d := openTestDB(t)
	const id = "msorder1"
	base := testNow - 500

	early := time.Unix(base, 100*int64(time.Millisecond)).UTC().Format(time.RFC3339Nano)
	late := time.Unix(base, 900*int64(time.Millisecond)).UTC().Format(time.RFC3339Nano)

	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
		sessionProbeQuery(id): ndjson(map[string]string{"n": "2"}),
		sessionEventsQuery(id): ndjson(
			map[string]string{"_time": late, "cost_usd": "3", "model": "claude-sonnet-5", "query_source": "main"},
			map[string]string{"_time": early, "cost_usd": "1", "model": "claude-sonnet-5", "query_source": "main"},
		),
		sessionToolsQuery(id):   "",
		sessionPromptsQuery(id): ndjson(map[string]string{"n": "1"}),
	}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	resp, err := buildSession(context.Background(), sessionsOptions(vmSrv, vlSrv, d), id, testNow)
	if err != nil {
		t.Fatalf("buildSession: %v", err)
	}
	if len(resp.Trajectory) != 2 {
		t.Fatalf("len(Trajectory) = %d, want 2", len(resp.Trajectory))
	}
	if resp.Trajectory[0].CumCostUsd != 1 {
		t.Errorf("Trajectory[0].CumCostUsd = %v, want 1 (the 100ms event, processed first)", resp.Trajectory[0].CumCostUsd)
	}
	if resp.Trajectory[1].CumCostUsd != 4 {
		t.Errorf("Trajectory[1].CumCostUsd = %v, want 4 (1+3, the 900ms event processed second)", resp.Trajectory[1].CumCostUsd)
	}
}

// The live merge retains indexed identity, project and aggregate bounds
// while taking current measurements from the live events.
func TestBuildSessionStaticMergeOverlay(t *testing.T) {
	d := openTestDB(t)
	const id = "merge1"
	if err := db.UpsertSession(d, db.SessionRow{
		ID: id, Profile: "stable-profile-id", Project: "proj-b", Start: testNow - 100000, End: testNow - 90000,
		CostUSD: 50, Requests: 100, HasTranscript: true, Analysed: true,
		MCPServers: []string{"linear"},
	}); err != nil {
		t.Fatal(err)
	}

	liveEnd := testNow - 200
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
		sessionProbeQuery(id): ndjson(map[string]string{"n": "1"}),
		sessionEventsQuery(id): ndjson(map[string]string{
			"_time": rfc3339(liveEnd), "cost_usd": "2", "input_tokens": "1", "output_tokens": "1",
			"model": "claude-sonnet-5", "query_source": "main",
		}),
		sessionToolsQuery(id):   "",
		sessionPromptsQuery(id): ndjson(map[string]string{"n": "1"}),
	}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	resp, err := buildSession(context.Background(), sessionsOptions(vmSrv, vlSrv, d), id, testNow)
	if err != nil {
		t.Fatalf("buildSession: %v", err)
	}
	row := resp.Row
	if row.Profile != "stable-profile-id" || row.Provider != "claude" {
		t.Errorf("live merge lost indexed identity: %+v", row)
	}
	if row.Project != "proj-b" {
		t.Errorf("Project = %q, want proj-b (from static)", row.Project)
	}
	if row.Start != float64(testNow-100000) {
		t.Errorf("Start = %v, want static's earlier start %v", row.Start, testNow-100000)
	}
	if row.CostUsd != 50 {
		t.Errorf("CostUsd = %v, want max(2,50)=50", row.CostUsd)
	}
	if row.Requests != 100 {
		t.Errorf("Requests = %v, want max(1,100)=100", row.Requests)
	}
	if row.End != float64(liveEnd) {
		t.Errorf("End = %v, want live's own end %v (never overlaid from static)", row.End, liveEnd)
	}
	if !resp.Live {
		t.Error("Live = false, want true")
	}
	// Analysis and extension metadata are not part of the live merge.
	if row.Analysed {
		t.Error("Analysed = true, want false (not overlaid)")
	}
	if len(row.McpServers) != 0 {
		t.Errorf("McpServers = %v, want empty (not overlaid)", row.McpServers)
	}
	if row.Tokens == nil || row.Tokens.Input != 1 {
		t.Errorf("Tokens = %+v, want live's own token totals, untouched", row.Tokens)
	}
}

// real_cost_usd only carries a figure when this session's own end falls
// on/after the overage series' observed start; unlike ListSessions'
// response-level flag, GetSession's overage_observed is that same per-session
// gate (hasStart && row.end >= seriesStart), not just whether the series
// resolved at all, so a consumer never reads a genuine "$0.00" for a session
// that predates the series. Row.BilledOverlap follows the identical gate,
// then SpanOverlapsBands, matching the list path's badge exactly.
func TestBuildSessionRealCostObservedGate(t *testing.T) {
	d := openTestDB(t)

	newRow := func(id string, end int64) db.SessionRow {
		return db.SessionRow{
			ID: id, Project: "p", Start: end - 500, End: end, CostUSD: 4, Requests: 1,
			HasTranscript: true,
			Trajectory:    [][3]float64{{float64(end - 500), 1, 0}, {float64(end), 4, 0}},
		}
	}

	t.Run("observed: real_cost_usd computed from the trajectory", func(t *testing.T) {
		const seriesStart = int64(1755000000)
		row := newRow("obs", seriesStart+1000)
		if err := db.UpsertSession(d, row); err != nil {
			t.Fatal(err)
		}
		vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
			sessionProbeQuery("obs"): ndjson(map[string]string{"n": "0"}),
		}})
		defer vlSrv.Close()
		vmSrv := httptest.NewServer(fakeVMHandler{
			ranged: map[string]string{
				"max(claude_in_overage) == 1": successBody(t,
					vmResult{Metric: map[string]string{}, Values: [][2]any{
						{float64(row.End - 400), "1"}, {float64(row.End), "1"},
					}}),
			},
			instant: map[string]string{
				"min_over_time(timestamp(claude_in_overage)[45d:10m])": successBody(t,
					vmResult{Metric: map[string]string{}, Value: []any{0, strconv.FormatInt(seriesStart, 10)}}),
			},
		})
		defer vmSrv.Close()

		resp, err := buildSession(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "obs", testNow)
		if err != nil {
			t.Fatalf("buildSession: %v", err)
		}
		if !resp.OverageObserved {
			t.Error("OverageObserved = false, want true")
		}
		if resp.RealCostUsd <= 0 {
			t.Errorf("RealCostUsd = %v, want a positive figure (trajectory rose inside the band)", resp.RealCostUsd)
		}
		if !resp.Row.BilledOverlap {
			t.Error("Row.BilledOverlap = false, want true (the session's span overlaps the band)")
		}
	})

	t.Run("session predates the series: overage_observed false, no real-cost claim", func(t *testing.T) {
		const seriesStart = int64(1755000000)
		row := newRow("predates", seriesStart-1000)
		if err := db.UpsertSession(d, row); err != nil {
			t.Fatal(err)
		}
		vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
			sessionProbeQuery("predates"): ndjson(map[string]string{"n": "0"}),
		}})
		defer vlSrv.Close()
		// The band deliberately overlaps this session's own span (it sits
		// inside [row.Start, row.End]): if the observed gate were dropped,
		// BilledOverlap would wrongly read true. The gate must still win.
		vmSrv := httptest.NewServer(fakeVMHandler{
			ranged: map[string]string{
				"max(claude_in_overage) == 1": successBody(t,
					vmResult{Metric: map[string]string{}, Values: [][2]any{
						{float64(row.End - 400), "1"}, {float64(row.End), "1"},
					}}),
			},
			instant: map[string]string{
				"min_over_time(timestamp(claude_in_overage)[45d:10m])": successBody(t,
					vmResult{Metric: map[string]string{}, Value: []any{0, strconv.FormatInt(seriesStart, 10)}}),
			},
		})
		defer vmSrv.Close()

		resp, err := buildSession(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "predates", testNow)
		if err != nil {
			t.Fatalf("buildSession: %v", err)
		}
		if resp.OverageObserved {
			t.Error("OverageObserved = true, want false (session ended before the series started)")
		}
		if resp.RealCostUsd != 0 {
			t.Errorf("RealCostUsd = %v, want 0 (session predates the observed series)", resp.RealCostUsd)
		}
		if resp.Row.BilledOverlap {
			t.Error("Row.BilledOverlap = true, want false (gated on observed, not just geometric overlap)")
		}
	})

	t.Run("series unresolved: overage_observed false, real_cost_usd zero", func(t *testing.T) {
		row := newRow("unresolved", testNow-1000)
		if err := db.UpsertSession(d, row); err != nil {
			t.Fatal(err)
		}
		vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
			sessionProbeQuery("unresolved"): ndjson(map[string]string{"n": "0"}),
		}})
		defer vlSrv.Close()
		vmSrv := emptyOverageVM(t)
		defer vmSrv.Close()

		resp, err := buildSession(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "unresolved", testNow)
		if err != nil {
			t.Fatalf("buildSession: %v", err)
		}
		if resp.OverageObserved {
			t.Error("OverageObserved = true, want false (series never resolved)")
		}
		if resp.RealCostUsd != 0 {
			t.Errorf("RealCostUsd = %v, want 0", resp.RealCostUsd)
		}
		if resp.Row.BilledOverlap {
			t.Error("Row.BilledOverlap = true, want false (the series never resolved)")
		}
	})
}

// A session with no recent VictoriaLogs activity at all comes straight from
// the db row: tools and trajectory (which have no field on SessionRow
// itself) must still surface at GetSessionResponse's top level.
func TestBuildSessionStaticOnly(t *testing.T) {
	d := openTestDB(t)
	const id = "static1"
	if err := db.UpsertSession(d, db.SessionRow{
		ID: id, Project: "proj-c", Start: 100, End: 200, CostUSD: 3, Requests: 2,
		HasTranscript: true, Analysed: true, MCPServers: []string{"linear"},
		Tools:      map[string]int{"Bash": 4},
		Trajectory: [][3]float64{{100, 1, 10}, {200, 3, 20}},
	}); err != nil {
		t.Fatal(err)
	}
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
		sessionProbeQuery(id): ndjson(map[string]string{"n": "0"}),
	}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	resp, err := buildSession(context.Background(), sessionsOptions(vmSrv, vlSrv, d), id, testNow)
	if err != nil {
		t.Fatalf("buildSession: %v", err)
	}
	if resp.Live {
		t.Error("Live = true, want false")
	}
	if resp.Row.Project != "proj-c" || resp.Row.CostUsd != 3 {
		t.Errorf("Row = %+v, want the static row untouched", resp.Row)
	}
	if !resp.Row.Analysed {
		t.Error("Analysed = false, want true (carried straight from the static row)")
	}
	if len(resp.Row.McpServers) != 1 || resp.Row.McpServers[0] != "linear" {
		t.Errorf("McpServers = %v, want [linear]", resp.Row.McpServers)
	}
	if resp.Tools["Bash"] != 4 {
		t.Errorf("Tools = %+v, want {Bash:4}", resp.Tools)
	}
	if len(resp.Trajectory) != 2 || resp.Trajectory[1].CumCostUsd != 3 {
		t.Errorf("Trajectory = %+v, want the static row's own two points", resp.Trajectory)
	}
}

// An id with no live activity and no static row is genuinely absent.
func TestBuildSessionNotFound(t *testing.T) {
	d := openTestDB(t)
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
		sessionProbeQuery("ghost"): ndjson(map[string]string{"n": "0"}),
	}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	_, err := buildSession(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "ghost", testNow)
	if err == nil {
		t.Fatal("buildSession: want an error for a genuinely absent id")
	}
	if !isSessionNotFound(err) {
		t.Errorf("buildSession error = %v, want errSessionNotFound", err)
	}
}

// Fail-soft degradation on the GetSession path.
func TestBuildSessionDegradation(t *testing.T) {
	t.Run("VL down: falls back to the static row, with a SectionError", func(t *testing.T) {
		d := openTestDB(t)
		if err := db.UpsertSession(d, db.SessionRow{
			ID: "s1", Project: "proj-a", Start: 100, End: 200, CostUSD: 3, Requests: 2, HasTranscript: true,
		}); err != nil {
			t.Fatal(err)
		}
		vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{}, fail: sessionProbeQuery("s1")})
		defer vlSrv.Close()
		vmSrv := emptyOverageVM(t)
		defer vmSrv.Close()

		resp, err := buildSession(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "s1", testNow)
		if err != nil {
			t.Fatalf("buildSession: %v", err)
		}
		if resp.Live {
			t.Error("Live = true, want false (fell back to static)")
		}
		if resp.Row.Project != "proj-a" {
			t.Errorf("Project = %q, want proj-a", resp.Row.Project)
		}
		if !hasSectionError(resp.Errors, "live") {
			t.Errorf("Errors = %+v, want a live SectionError", resp.Errors)
		}
	})

	t.Run("nil DB: pure live row still returned, with a SectionError", func(t *testing.T) {
		vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
			sessionProbeQuery("live-only"): ndjson(map[string]string{"n": "1"}),
			sessionEventsQuery("live-only"): ndjson(map[string]string{
				"_time": rfc3339(testNow - 10), "cost_usd": "1", "model": "claude-sonnet-5", "query_source": "main",
			}),
			sessionToolsQuery("live-only"):   "",
			sessionPromptsQuery("live-only"): "",
		}})
		defer vlSrv.Close()
		vmSrv := emptyOverageVM(t)
		defer vmSrv.Close()

		resp, err := buildSession(context.Background(), sessionsOptions(vmSrv, vlSrv, nil), "live-only", testNow)
		if err != nil {
			t.Fatalf("buildSession: %v", err)
		}
		if !resp.Live {
			t.Error("Live = false, want true")
		}
		if !hasSectionError(resp.Errors, "db") {
			t.Errorf("Errors = %+v, want a db SectionError", resp.Errors)
		}
	})

	t.Run("overage down: real_cost_usd zero, overage SectionError, response still built", func(t *testing.T) {
		d := openTestDB(t)
		if err := db.UpsertSession(d, db.SessionRow{
			ID: "s2", Project: "proj-a", Start: 100, End: 200, CostUSD: 3, Requests: 2, HasTranscript: true,
		}); err != nil {
			t.Fatal(err)
		}
		vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
			sessionProbeQuery("s2"): ndjson(map[string]string{"n": "0"}),
		}})
		defer vlSrv.Close()
		vmSrv := httptest.NewServer(fakeVMHandler{fail: "max(claude_in_overage) == 1"})
		defer vmSrv.Close()

		resp, err := buildSession(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "s2", testNow)
		if err != nil {
			t.Fatalf("buildSession: %v", err)
		}
		if resp.RealCostUsd != 0 || resp.OverageObserved {
			t.Errorf("RealCostUsd/OverageObserved = %v/%v, want 0/false", resp.RealCostUsd, resp.OverageObserved)
		}
		if !hasSectionError(resp.Errors, "overage") {
			t.Errorf("Errors = %+v, want an overage SectionError", resp.Errors)
		}
	})

	t.Run("both live and static unavailable: hard error, not NotFound", func(t *testing.T) {
		d := openTestDB(t)
		d.Close() // force every query against it to return a real error, not "not found"
		vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{}, fail: sessionProbeQuery("s3")})
		defer vlSrv.Close()
		vmSrv := emptyOverageVM(t)
		defer vmSrv.Close()

		_, err := buildSession(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "s3", testNow)
		if err == nil {
			t.Fatal("buildSession: want an error")
		}
		if isSessionNotFound(err) {
			t.Error("want a plain unavailable-style error, not errSessionNotFound, when the db itself errored")
		}
	})
}

/* RPC wiring --------------------------------------------------------------- */

func TestListSessionsRPC(t *testing.T) {
	d := openTestDB(t)
	if err := db.UpsertSession(d, db.SessionRow{ID: "s1", Project: "p", HasTranscript: true}); err != nil {
		t.Fatal(err)
	}
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	dir := t.TempDir()
	o := sessionsOptions(vmSrv, vlSrv, d)
	o.StaticDir = dir
	if err := writeIndexHTML(t, dir); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(o))
	defer srv.Close()
	client := claudeobsv1connect.NewObsServiceClient(http.DefaultClient, srv.URL)

	resp, err := client.ListSessions(context.Background(), connect.NewRequest(&claudeobsv1.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(resp.Msg.Sessions) != 1 {
		t.Errorf("len(Sessions) = %d, want 1", len(resp.Msg.Sessions))
	}
}

func TestGetSessionRPCNotFound(t *testing.T) {
	d := openTestDB(t)
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
		sessionProbeQuery("ghost"): ndjson(map[string]string{"n": "0"}),
	}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	dir := t.TempDir()
	o := sessionsOptions(vmSrv, vlSrv, d)
	o.StaticDir = dir
	if err := writeIndexHTML(t, dir); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(o))
	defer srv.Close()
	client := claudeobsv1connect.NewObsServiceClient(http.DefaultClient, srv.URL)

	_, err := client.GetSession(context.Background(), connect.NewRequest(&claudeobsv1.GetSessionRequest{Id: "ghost"}))
	if err == nil {
		t.Fatal("GetSession: want an error for a missing id")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want CodeNotFound", connect.CodeOf(err))
	}
}

func writeIndexHTML(t *testing.T, dir string) error {
	t.Helper()
	return os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>t</title>"), 0o644)
}

/* Profile filter ------------------------------------------------------------ */

// Both halves have to narrow, not just the indexed one. A live session
// belonging to another profile has no db row to be filtered out by, so an
// unfiltered overlay would synthesize it as a "(not yet indexed)" row and show
// it under a filter that excludes it: the one row on the page that is wrong.
func TestBuildListSessionsFiltersTheLiveOverlayToo(t *testing.T) {
	d := openTestDB(t)
	for _, r := range []db.SessionRow{
		{ID: "mine", Project: "proj-a", Profile: "studio", Start: 1755000000, End: 1755000500, CostUSD: 5, HasTranscript: true},
		{ID: "theirs", Project: "proj-b", Profile: "acme", Start: 1755000000, End: 1755000500, CostUSD: 9, HasTranscript: true},
	} {
		if err := db.UpsertSession(d, r); err != nil {
			t.Fatal(err)
		}
	}

	// The unfiltered query is answered with a live session belonging to
	// neither indexed row. Only the unfiltered call should ever see it: the
	// filtered call asks a different query string, which this stub does not
	// answer, so it gets nothing.
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
		sessionsListQuery: ndjson(map[string]string{
			"session.id": "live-elsewhere", "cost": "3", "reqs": "1",
			"first_t": rfc3339(testNow - 200), "last_t": rfc3339(testNow - 100),
		}),
	}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	ids := func(resp *claudeobsv1.ListSessionsResponse) []string {
		var out []string
		for _, r := range resp.Sessions {
			out = append(out, r.Id)
		}
		sort.Strings(out)
		return out
	}

	// The control first: unfiltered, all three are present, so the stub is
	// genuinely answering and a later empty result means filtering rather
	// than a query that never matched anything.
	all, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(all); len(got) != 3 {
		t.Fatalf("unfiltered ids = %v, want all three (mine, theirs, live-elsewhere)", got)
	}

	filtered, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "studio", testNow)
	if err != nil {
		t.Fatal(err)
	}
	got := ids(filtered)
	if len(got) != 1 || got[0] != "mine" {
		t.Errorf("filtered ids = %v, want [mine] only", got)
	}
}

// A row whose own profile is empty was indexed before the column existed, so
// it genuinely does not know where it came from. It must match no filter
// rather than be folded into whichever one was asked for, which would
// attribute one profile's money to another silently. Same rule profileLabel
// renders as a dash.
func TestBuildListSessionsExcludesUnattributedRowsFromEveryFilter(t *testing.T) {
	d := openTestDB(t)
	for _, r := range []db.SessionRow{
		{ID: "named", Project: "p", Profile: "acme", End: 1755000500, CostUSD: 1, HasTranscript: true},
		{ID: "blank", Project: "p", Profile: "", End: 1755000500, CostUSD: 100, HasTranscript: true},
	} {
		if err := db.UpsertSession(d, r); err != nil {
			t.Fatal(err)
		}
	}
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()

	resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), "acme", testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resp.Sessions {
		if r.Id == "blank" {
			t.Fatal("an unattributed row was folded into the acme filter")
		}
	}
	if len(resp.Sessions) != 1 {
		t.Errorf("len = %d, want 1 (the named row)", len(resp.Sessions))
	}
}

func TestBuildListSessionsCarriesKnownProfileForLiveOnlyRows(t *testing.T) {
	d := openTestDB(t)
	event := ndjson(map[string]string{
		"session.id": "live-unindexed", "cost": "3", "reqs": "1",
		"first_t": rfc3339(testNow - 200), "last_t": rfc3339(testNow - 100),
	})
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{
		sessionsListQueryFor("work"): event,
		sessionsListQuery:            event,
	}})
	defer vlSrv.Close()
	vmSrv := emptyOverageVM(t)
	defer vmSrv.Close()
	for _, profile := range []string{"work", ""} {
		resp, err := buildListSessions(context.Background(), sessionsOptions(vmSrv, vlSrv, d), profile, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Sessions) != 1 {
			t.Fatalf("profile %q: got %d rows", profile, len(resp.Sessions))
		}
		row := resp.Sessions[0]
		if row.Profile != profile {
			t.Errorf("profile %q: live row attribution = %q", profile, row.Profile)
		}
		if row.Provider != "claude" || !row.CostKnown || !row.UsageKnown || !row.Live {
			t.Errorf("live Claude capabilities lost: %+v", row)
		}
	}
}
