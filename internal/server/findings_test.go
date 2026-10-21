package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1/claudeobsv1connect"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vl"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
)

// Legacy compatibility counters are deliberately absent from production queries.
const overage7dQuery = `legacy-overage-valuation`
const overagePresentQuery = `count(count_over_time(claude_in_overage[7d]))`

// fakeVLQueryHandler serves a canned NDJSON body keyed by the exact query
// string (form-encoded, matching vl.Client.Query's POST), falling back to an
// empty result for anything not listed. When limits is non-nil, every
// request's "limit" form value is recorded against its query, so a test can
// assert the ten-queries-at-500-except-turns-at-20000 rule from the query
// inventory.
type fakeVLQueryHandler struct {
	byQuery map[string]string
	fail    string
	limits  map[string]string
}

func (h fakeVLQueryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	q := strings.TrimPrefix(r.FormValue("query"), `(provider:="claude" OR provider:="") `)
	if h.limits != nil {
		h.limits[q] = r.FormValue("limit")
	}
	if h.fail != "" && q == h.fail {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	body, ok := h.byQuery[q]
	if !ok {
		body = ""
	}
	w.Write([]byte(body))
}

// ndjson renders rows as VictoriaLogs' one-JSON-object-per-line response
// shape.
func ndjson(rows ...map[string]string) string {
	var b strings.Builder
	for _, r := range rows {
		data, err := json.Marshal(r)
		if err != nil {
			panic(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}

func findingsOptions(vmSrv, vlSrv *httptest.Server, d *sql.DB) Options {
	o := Options{Memo: store.New(store.TTL), DB: d}
	if vmSrv != nil {
		o.VM = &vm.Client{BaseURL: vmSrv.URL, HTTP: vmSrv.Client()}
	}
	if vlSrv != nil {
		o.VL = &vl.Client{BaseURL: vlSrv.URL, HTTP: vlSrv.Client()}
	}
	return o
}

// seedFindingsIndex seeds the sessions table with the given rows, the only
// source sessionFactsFromSessions reads from. It also sets the
// sessions_generated_at meta row RunIndex writes at the end of every real
// pass, regardless of session count, so a zero-row call here still
// represents a completed, legitimately empty index rather than a
// never-indexed db (see listIndexedSessions in index.go).
func seedFindingsIndex(t *testing.T, d *sql.DB, sessions ...db.SessionRow) {
	t.Helper()
	for _, row := range sessions {
		if err := db.UpsertSession(d, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.MetaSet(d, sessionsGeneratedAtKey, "1755300000"); err != nil {
		t.Fatal(err)
	}
}

// richSession is the fixture rules_test.go's TestRun ("sorts high before
// medium before info and drops nothing") and rules.test.mjs's matching case
// use for the index: one session with enough cache traffic to clear
// cacheDiscipline's floor and a model-mix leader.
func richSession() db.SessionRow {
	return db.SessionRow{
		ID:     "a",
		Tokens: transcript.Tokens{CacheRead: 10e6, CacheCreation: 2e5},
		Models: []transcript.ModelCost{{Model: "claude-opus-5", CostUSD: 10}},
	}
}

// richVLBody returns the ten canned VL query bodies for the same fixture, so
// that end to end, over the wire, real-money comes back high, exactly one
// session's cache/model data feeds cacheDiscipline/modelMix, and
// mainThreadConcentration/mcpFreight/subagentRoi all have enough traffic to
// speak. See the task report for the full per-rule severity derivation.
func richVLBody() map[string]string {
	return map[string]string{
		vlBySourceQuery: ndjson(
			map[string]string{"query_source": "repl_main_thread", "n": "364", "cost": "327.68"},
			map[string]string{"query_source": "agent:custom", "n": "1099", "cost": "41.22"},
		),
		vlSubagentsQuery: ndjson(
			map[string]string{"agent_type": "a", "runs": "10", "tok": "100", "tools": "1", "ms": "1"},
			map[string]string{"agent_type": "b", "runs": "10", "tok": "100", "tools": "1", "ms": "1"},
		),
		vlTurnsQuery:        ndjson(repeatRows(20, map[string]string{"cost": "2", "reqs": "5"})...),
		vlToolResultsQuery:  ndjson(map[string]string{"tool_name": "Bash", "success": "true", "n": "1000"}),
		vlWorstFailQuery:    "",
		vlDecisionsQuery:    ndjson(map[string]string{"source": "config", "decision": "accept", "tool_name": "Bash", "n": "200"}),
		vlHooksQuery:        ndjson(map[string]string{"hook_name": "Stop", "runs": "30", "ms": "300", "errs": "0"}),
		vlMcpConnectedQuery: ndjson(map[string]string{"server_name": "codex", "n": "3"}),
		vlMcpFailedQuery:    "",
		vlFirstQuery:        ndjson(map[string]string{"_time": "2026-05-01T00:00:00Z"}),
	}
}

func repeatRows(n int, row map[string]string) []map[string]string {
	out := make([]map[string]string, n)
	for i := range out {
		out[i] = row
	}
	return out
}

// richVMInstant returns the six canned VM instant-query bodies for the rich
// fixture: cost7d=100, overage7d=91 (with the presence probe nonempty, so
// OverageKnown is true), and zero active hours/commits/lines (absent from
// the map, so fakeVMHandler's own fallback answers them with an empty
// result, which vm.Scalar reads as 0).
func richVMInstant(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		`sum(increase({__name__="claude_code.cost.usage"}[7d]))`: successBody(t, vmResult{Metric: map[string]string{}, Value: []any{0, "100"}}),
		overage7dQuery:      successBody(t, vmResult{Metric: map[string]string{}, Value: []any{0, "91"}}),
		overagePresentQuery: successBody(t, vmResult{Metric: map[string]string{}, Value: []any{0, "1"}}),
	}
}

func findByID(fs []*claudeobsv1.Finding, id string) *claudeobsv1.Finding {
	for _, f := range fs {
		if f.Id == id {
			return f
		}
	}
	return nil
}

func idsWithSeverity(fs []*claudeobsv1.Finding, severity string) []string {
	var out []string
	for _, f := range fs {
		if f.Severity == severity {
			out = append(out, f.Id)
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// The rich fixture end to end: real-money must come back high (91% share),
// badge_count must be high+medium+info (1+2+2=5), and the clean/insufficient
// partition (derived from the findings list, the same way a consumer splits
// it) must match the severities each of the ten rules produces against this
// data.
func TestBuildFindingsRichFixtureEndToEnd(t *testing.T) {
	instant := richVMInstant(t)
	vmSrv := httptest.NewServer(fakeVMHandler{instant: instant})
	defer vmSrv.Close()

	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: richVLBody()})
	defer vlSrv.Close()

	d := openTestDB(t)
	seedFindingsIndex(t, d, richSession())

	resp, err := buildFindings(context.Background(), findingsOptions(vmSrv, vlSrv, d))
	if err != nil {
		t.Fatalf("buildFindings: %v", err)
	}
	if !resp.VmOk || !resp.VlOk || !resp.IndexOk {
		t.Fatalf("VmOk=%v VlOk=%v IndexOk=%v, want all true", resp.VmOk, resp.VlOk, resp.IndexOk)
	}
	if resp.OverageKnown {
		t.Error("OverageKnown must remain false without provider billing evidence")
	}
	if len(resp.Findings) != 10 {
		t.Fatalf("len(Findings) = %d, want 10", len(resp.Findings))
	}

	rm := findByID(resp.Findings, "real-money")
	if rm == nil || rm.Severity != "insufficient" {
		t.Fatalf("real-money = %+v, want insufficient without explicit billing attribution", rm)
	}

	if resp.BadgeCount != 3 {
		t.Errorf("BadgeCount = %d, want 3 (1 medium + 2 info; activity counts replace monetary weights)", resp.BadgeCount)
	}

	wantClean := []string{"cache-discipline", "turn-cost", "failure-loops", "permission-friction", "hook-tax", "main-thread-concentration"}
	gotClean := idsWithSeverity(resp.Findings, "clean")
	if len(gotClean) != len(wantClean) {
		t.Fatalf("clean ids = %v, want %v", gotClean, wantClean)
	}
	for _, id := range wantClean {
		if !containsStr(gotClean, id) {
			t.Errorf("clean ids %v missing %q", gotClean, id)
		}
	}

	gotInsufficient := idsWithSeverity(resp.Findings, "insufficient")
	if len(gotInsufficient) != 1 || gotInsufficient[0] != "real-money" {
		t.Errorf("insufficient ids = %v, want real-money without billing attribution", gotInsufficient)
	}

	wantMedium := []string{"mcp-freight"}
	gotMedium := idsWithSeverity(resp.Findings, "medium")
	if len(gotMedium) != len(wantMedium) {
		t.Fatalf("medium ids = %v, want %v", gotMedium, wantMedium)
	}

	if resp.LogsStartTs == 0 {
		t.Error("LogsStartTs = 0, want the parsed _time epoch from the first-event probe")
	}
}

// The presence query (OVERAGE_PRESENT) returning an empty result must read as
// overage_known=false, not a genuine zero: the poller never having run and a
// plan that covered everything must never collapse into each other.
func TestBuildFindingsOverageKnownFalseOnEmptyPresence(t *testing.T) {
	instant := map[string]string{
		`sum(increase({__name__="claude_code.cost.usage"}[7d]))`: successBody(t, vmResult{Metric: map[string]string{}, Value: []any{0, "411.7"}}),
		overagePresentQuery: successBody(t), // empty result: the poller never ran
	}
	vmSrv := httptest.NewServer(fakeVMHandler{instant: instant})
	defer vmSrv.Close()
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: map[string]string{}})
	defer vlSrv.Close()

	// A completed index pass with zero sessions (seedFindingsIndex sets the
	// sessions_generated_at meta row even with no rows to seed): the index
	// read succeeds (idxOk = true, zero rows), a legitimately empty index,
	// unlike TestBuildFindingsIndexFailureRulesStillRun below (a nil db) or
	// TestBuildFindingsNeverIndexedErrors (a real db that never completed a
	// pass at all), both of which reproduce "no index available."
	d := openTestDB(t)
	seedFindingsIndex(t, d)

	resp, err := buildFindings(context.Background(), findingsOptions(vmSrv, vlSrv, d))
	if err != nil {
		t.Fatalf("buildFindings: %v", err)
	}
	if resp.OverageKnown {
		t.Error("OverageKnown = true, want false when the presence query returns no samples")
	}
	if resp.Cost_7D != 0 {
		t.Errorf("Cost_7D = %v, compatibility valuations must not be exposed", resp.Cost_7D)
	}
}

// An unreadable session index must not blank the whole response: vm/vl still
// load, index_ok reads false, and rules.Run still produces its full ten
// findings (the index-dependent rules degrading to insufficient rather than
// the RPC failing).
func TestBuildFindingsIndexFailureRulesStillRun(t *testing.T) {
	instant := richVMInstant(t)
	vmSrv := httptest.NewServer(fakeVMHandler{instant: instant})
	defer vmSrv.Close()
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: richVLBody()})
	defer vlSrv.Close()

	// No db configured at all: listIndexedSessions fails, the direct
	// analogue of the old "sessions/index.json does not exist" case.
	resp, err := buildFindings(context.Background(), findingsOptions(vmSrv, vlSrv, nil))
	if err != nil {
		t.Fatalf("buildFindings: %v", err)
	}
	if resp.IndexOk {
		t.Error("IndexOk = true, want false when the session index cannot be read")
	}
	if !resp.VmOk || !resp.VlOk {
		t.Fatalf("VmOk=%v VlOk=%v, want both true", resp.VmOk, resp.VlOk)
	}
	if len(resp.Findings) != 10 {
		t.Fatalf("len(Findings) = %d, want 10 even with the index down", len(resp.Findings))
	}
	rm := findByID(resp.Findings, "real-money")
	if rm == nil || rm.Severity != "insufficient" {
		t.Fatalf("real-money = %+v, want insufficient without explicit billing attribution", rm)
	}
	// cacheDiscipline and modelMix read only the index: with no sessions at
	// all they must read insufficient rather than clean/info.
	cd := findByID(resp.Findings, "cache-discipline")
	if cd == nil || cd.Severity != "insufficient" {
		t.Fatalf("cache-discipline = %+v, want insufficient with no index", cd)
	}
	mm := findByID(resp.Findings, "model-mix")
	if mm == nil || mm.Severity != "insufficient" {
		t.Fatalf("model-mix = %+v, want insufficient with no index", mm)
	}
	if len(resp.Errors) == 0 {
		t.Error("Errors is empty, want a section error naming the failed index load")
	}
}

// TestBuildFindingsNeverIndexedErrors is TestBuildFindingsIndexFailureRulesStillRun's
// counterpart for a real, migrated db that has just never completed an
// indexing pass (no sessions_generated_at meta row), rather than no db at
// all. listIndexedSessions must treat it as an error, not a legitimately
// empty index, or a fresh install's session index would silently read as
// "ok, zero sessions" instead of flagging that indexing has never run.
func TestBuildFindingsNeverIndexedErrors(t *testing.T) {
	instant := richVMInstant(t)
	vmSrv := httptest.NewServer(fakeVMHandler{instant: instant})
	defer vmSrv.Close()
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: richVLBody()})
	defer vlSrv.Close()

	// A real, migrated db, but seedFindingsIndex is never called: no
	// sessions_generated_at meta row, distinct from the nil-db case above.
	d := openTestDB(t)

	resp, err := buildFindings(context.Background(), findingsOptions(vmSrv, vlSrv, d))
	if err != nil {
		t.Fatalf("buildFindings: %v", err)
	}
	if resp.IndexOk {
		t.Error("IndexOk = true, want false for a db that has never completed an indexing pass")
	}
	var idxErr *claudeobsv1.SectionError
	for _, e := range resp.Errors {
		if e.Section == "index" {
			idxErr = e
		}
	}
	if idxErr == nil {
		t.Fatalf("Errors = %+v, want a section error for \"index\"", resp.Errors)
	}
}

// When every source is down, buildFindings must return an RPC-level error
// rather than an empty, confident-looking response.
func TestBuildFindingsErrorsWhenAllSourcesFail(t *testing.T) {
	vmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer vmSrv.Close()
	vlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer vlSrv.Close()

	_, err := buildFindings(context.Background(), findingsOptions(vmSrv, vlSrv, nil)) // no db either
	if err == nil {
		t.Fatal("want an error when vm, vl and the index all fail")
	}
}

// The ten VL queries must run at limit 500 except turns, which runs at
// 20000, per the query inventory.
func TestBuildFindingsVLQueryLimits(t *testing.T) {
	limits := map[string]string{}
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: richVLBody(), limits: limits})
	defer vlSrv.Close()
	vmSrv := httptest.NewServer(fakeVMHandler{instant: richVMInstant(t)})
	defer vmSrv.Close()

	d := openTestDB(t)
	seedFindingsIndex(t, d, richSession())

	if _, err := buildFindings(context.Background(), findingsOptions(vmSrv, vlSrv, d)); err != nil {
		t.Fatalf("buildFindings: %v", err)
	}

	if limits[vlTurnsQuery] != "20000" {
		t.Errorf("turns query limit = %q, want 20000", limits[vlTurnsQuery])
	}
	for _, q := range []string{vlBySourceQuery, vlSubagentsQuery, vlToolResultsQuery, vlWorstFailQuery,
		vlDecisionsQuery, vlHooksQuery, vlMcpConnectedQuery, vlMcpFailedQuery, vlFirstQuery} {
		if limits[q] != "500" {
			t.Errorf("query %q limit = %q, want 500", q, limits[q])
		}
	}
}

// A smoke test that GetFindings is wired through Handler/Options, not just
// that buildFindings works in isolation.
func TestGetFindingsRPC(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>t</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	vmSrv := httptest.NewServer(fakeVMHandler{instant: richVMInstant(t)})
	defer vmSrv.Close()
	vlSrv := httptest.NewServer(fakeVLQueryHandler{byQuery: richVLBody()})
	defer vlSrv.Close()

	d := openTestDB(t)
	seedFindingsIndex(t, d, richSession())

	o := Options{
		StaticDir: dir, InstallDir: "/tmp", Version: "test",
		VM:   &vm.Client{BaseURL: vmSrv.URL, HTTP: vmSrv.Client()},
		VL:   &vl.Client{BaseURL: vlSrv.URL, HTTP: vlSrv.Client()},
		Memo: store.New(store.TTL),
		DB:   d,
	}
	srv := httptest.NewServer(Handler(o))
	defer srv.Close()
	client := claudeobsv1connect.NewObsServiceClient(http.DefaultClient, srv.URL)

	resp, err := client.GetFindings(context.Background(), connect.NewRequest(&claudeobsv1.GetFindingsRequest{}))
	if err != nil {
		t.Fatalf("GetFindings: %v", err)
	}
	if resp.Msg.Identity == nil {
		t.Error("Identity is nil")
	}
	if len(resp.Msg.Findings) != 10 {
		t.Errorf("len(Findings) = %d, want 10", len(resp.Msg.Findings))
	}
}
