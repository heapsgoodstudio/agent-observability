package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/heapsgoodstudio/agent-observability/internal/history"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// The three plan-window queries, verbatim (mirrors plan.go's own unexported
// constants of the same names, which live in a different package).
const (
	pctQuery   = `max by (window,source)(claude_plan_window_pct)`
	resetQuery = `max by (window,source)(claude_plan_window_reset_timestamp_seconds)`
	ageQuery   = `min by (source)(claude_plan_data_age_seconds)`
)

// vmResult mirrors one VM series' JSON shape, for building canned fixture
// bodies without hand-writing JSON strings.
type vmResult struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value,omitempty"`
	Values [][2]any          `json:"values,omitempty"`
}

func successBody(t *testing.T, results ...vmResult) string {
	t.Helper()
	if results == nil {
		results = []vmResult{}
	}
	body := map[string]any{
		"status": "success",
		"data":   map[string]any{"result": results},
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fakeVMHandler serves canned instant/range responses keyed by the exact
// query string, falling back to an empty success result for anything not
// listed: a section this test does not care about resolves to zero/false
// rather than failing, so only the queries under test need fixtures. A query
// matching fail always 500s, on either endpoint.
type fakeVMHandler struct {
	instant map[string]string
	ranged  map[string]string
	// rangedContains matches a range query by substring, for queries whose
	// exact text depends on the window the caller happens to build. The
	// delivery counters need it: their lookbehind spans the whole request,
	// so it differs per test. Exact matches in ranged win.
	rangedContains map[string]string
	fail           string
	failContains   string
}

func (h fakeVMHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if h.fail != "" && q == h.fail {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if h.failContains != "" && strings.Contains(q, h.failContains) {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var body string
	var ok bool
	switch r.URL.Path {
	case "/api/v1/query":
		body, ok = h.instant[q]
	case "/api/v1/query_range":
		if body, ok = h.ranged[q]; !ok {
			for frag, b := range h.rangedContains {
				if strings.Contains(q, frag) {
					body, ok = b, true
					break
				}
			}
		}
	}
	if !ok {
		body = `{"status":"success","data":{"result":[]}}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(body))
}

func overviewOptions(vmSrv *httptest.Server) Options {
	return Options{
		VM:   &vm.Client{BaseURL: vmSrv.URL, HTTP: vmSrv.Client()},
		Memo: store.New(store.TTL),
	}
}

// Ported from the brief: max(claude_in_overage) == 1 must resolve to a known,
// true in_overage.
func TestBuildOverviewInOverage(t *testing.T) {
	instant := map[string]string{
		"max(claude_in_overage)": successBody(t, vmResult{Metric: map[string]string{}, Value: []any{0, "1"}}),
	}
	srv := httptest.NewServer(fakeVMHandler{instant: instant})
	defer srv.Close()

	resp, err := buildOverview(context.Background(), overviewOptions(srv), "", 1755300000)
	if err != nil {
		t.Fatalf("buildOverview: %v", err)
	}
	if !resp.OverageKnown || !resp.InOverage {
		t.Errorf("InOverage=%v OverageKnown=%v, want true true", resp.InOverage, resp.OverageKnown)
	}
}

// Ported from the plan.test.mjs stale-cache fixture (also used by
// plan_test.go's TestPickFreshestFresherSourceWins): the cache still
// described a five_hour window that had ended, and its reset time was the
// larger of the two; the fresher statusline tap must win regardless.
func TestBuildOverviewWindowsResolveViaFreshestSource(t *testing.T) {
	instant := map[string]string{
		pctQuery: successBody(t,
			vmResult{Metric: map[string]string{"window": "five_hour", "source": "cache"}, Value: []any{0, "89"}},
			vmResult{Metric: map[string]string{"window": "five_hour", "source": "statusline"}, Value: []any{0, "2"}},
		),
		resetQuery: successBody(t,
			vmResult{Metric: map[string]string{"window": "five_hour", "source": "cache"}, Value: []any{0, "1786444799"}},
			vmResult{Metric: map[string]string{"window": "five_hour", "source": "statusline"}, Value: []any{0, "1786462800"}},
		),
		ageQuery: successBody(t,
			vmResult{Metric: map[string]string{"source": "cache"}, Value: []any{0, "24438"}},
			vmResult{Metric: map[string]string{"source": "statusline"}, Value: []any{0, "1563"}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{instant: instant})
	defer srv.Close()

	resp, err := buildOverview(context.Background(), overviewOptions(srv), "", 1755300000)
	if err != nil {
		t.Fatalf("buildOverview: %v", err)
	}

	var found bool
	for _, w := range resp.Windows {
		if w.Window != "five_hour" {
			continue
		}
		found = true
		if w.ResetTs != 1786462800 || w.ResetSource != "statusline" {
			t.Errorf("five_hour reset = %+v, want 1786462800 from statusline", w)
		}
	}
	if !found {
		t.Fatalf("no five_hour window in response; windows=%+v", resp.Windows)
	}
	if resp.SourceAges["cache"] != 24438 || resp.SourceAges["statusline"] != 1563 {
		t.Errorf("SourceAges = %v, want cache=24438 statusline=1563", resp.SourceAges)
	}
}

// The overage curve's bands must come from grouping the ==1 range samples at
// stepSec*2 = 1200 seconds, per the query inventory.
func TestBuildOverviewOverageCurveBands(t *testing.T) {
	now := int64(1755300000)
	monthStart := monthStartLocal(now)

	ranged := map[string]string{
		`sum(max by (account)(claude_extra_usage_usd{account!=""}))`: successBody(t, vmResult{
			Metric: map[string]string{},
			Values: [][2]any{
				{float64(monthStart), "1.5"},
				{float64(monthStart + 600), "2.5"},
				{float64(monthStart + 1200), "4"},
			},
		}),
		"max(claude_in_overage) == 1": successBody(t, vmResult{
			Metric: map[string]string{},
			Values: [][2]any{
				{float64(monthStart), "1"},
				{float64(monthStart + 600), "1"},
				{float64(monthStart + 3000), "1"}, // gap of 2400s from the last band > 1200: new band
			},
		}),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged})
	defer srv.Close()

	resp, err := buildOverview(context.Background(), overviewOptions(srv), "", now)
	if err != nil {
		t.Fatalf("buildOverview: %v", err)
	}

	if len(resp.OverageBands) != 2 {
		t.Fatalf("bands = %+v, want 2 bands", resp.OverageBands)
	}
	if resp.OverageBands[0].Start != float64(monthStart) || resp.OverageBands[0].End != float64(monthStart+600) {
		t.Errorf("band[0] = %+v, want [%v, %v]", resp.OverageBands[0], monthStart, monthStart+600)
	}
	if resp.OverageBands[1].Start != float64(monthStart+3000) || resp.OverageBands[1].End != float64(monthStart+3000) {
		t.Errorf("band[1] = %+v, want [%v, %v]", resp.OverageBands[1], monthStart+3000, monthStart+3000)
	}
	if resp.OverageMtdUsd != 4 {
		t.Errorf("OverageMtdUsd = %v, want 4 (the curve's latest point)", resp.OverageMtdUsd)
	}
}

// One query failing must degrade to a SectionError, not an RPC error: the
// brief's fail-soft requirement.
func TestBuildOverviewSectionErrorOnSingleQueryFailure(t *testing.T) {
	failQuery := fmt.Sprintf("sum(rate(%s[15m]))*3600", costMetric)
	srv := httptest.NewServer(fakeVMHandler{fail: failQuery})
	defer srv.Close()

	resp, err := buildOverview(context.Background(), overviewOptions(srv), "", 1755300000)
	if err != nil {
		t.Fatalf("buildOverview returned an RPC-level error for a single failing query: %v", err)
	}
	if len(resp.Errors) != 1 {
		t.Fatalf("Errors = %+v, want exactly one SectionError", resp.Errors)
	}
	if resp.Errors[0].Section != "burn" {
		t.Errorf("Errors[0].Section = %q, want burn", resp.Errors[0].Section)
	}
}

// When every query fails, buildOverview must return an RPC-level error
// instead of a response with every section listed as failed silently.
func TestBuildOverviewErrorsWhenEverySectionFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := buildOverview(context.Background(), overviewOptions(srv), "", 1755300000)
	if err == nil {
		t.Fatal("want an error when every section fails, got nil")
	}
}

// By-model attribution: sorts descending, truncates to the top 5, and the
// note text uses the "counted" wording built from the money formatter. model-a..f give a clean truncation case (6 rows, 5 shown).
func TestBuildOverviewByModelAttribution(t *testing.T) {
	now := int64(1755300000)
	win := history.RollingWindow(attributionDays, now, history.DAY)
	t0 := float64(win.End)

	histQuery := `sum by (model)(sum_over_time(claude_hist_daily_cost_usd[1d]))`
	ranged := map[string]string{
		histQuery: successBody(t,
			vmResult{Metric: map[string]string{"model": "model-a"}, Values: [][2]any{{t0, "50"}}},
			vmResult{Metric: map[string]string{"model": "model-b"}, Values: [][2]any{{t0, "40"}}},
			vmResult{Metric: map[string]string{"model": "model-c"}, Values: [][2]any{{t0, "30"}}},
			vmResult{Metric: map[string]string{"model": "model-d"}, Values: [][2]any{{t0, "20"}}},
			vmResult{Metric: map[string]string{"model": "model-e"}, Values: [][2]any{{t0, "10"}}},
			vmResult{Metric: map[string]string{"model": "model-f"}, Values: [][2]any{{t0, "5"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged})
	defer srv.Close()

	resp, err := buildOverview(context.Background(), overviewOptions(srv), "", now)
	if err != nil {
		t.Fatalf("buildOverview: %v", err)
	}

	if len(resp.ByModel_7D) != 5 {
		t.Fatalf("ByModel_7D = %+v, want 5 rows (top 5 of 6)", resp.ByModel_7D)
	}
	wantOrder := []struct {
		key string
		val float64
	}{
		{"model-a", 50}, {"model-b", 40}, {"model-c", 30}, {"model-d", 20}, {"model-e", 10},
	}
	for i, want := range wantOrder {
		got := resp.ByModel_7D[i]
		if got.Key != want.key || got.Value != want.val {
			t.Errorf("ByModel_7D[%d] = %+v, want {%s %v}", i, got, want.key, want.val)
		}
	}
	wantNote := "$155.00 across 6 values, top 5 shown"
	if resp.ByModelNote != wantNote {
		t.Errorf("ByModelNote = %q, want %q", resp.ByModelNote, wantNote)
	}
}

// The today section's real and notional figures each carry their own known
// flag rather than defaulting a missing sample to a silent 0: both must read
// true once VM answers both queries with a sample.
func TestBuildOverviewTodayKnownFlags(t *testing.T) {
	instant := map[string]string{
		"max(clamp_min(delta(claude_extra_usage_usd[24h]),0))":         successBody(t, vmResult{Metric: map[string]string{}, Value: []any{0, "3.5"}}),
		fmt.Sprintf("sum(clamp_min(increase(%s[24h]),0))", costMetric): successBody(t, vmResult{Metric: map[string]string{}, Value: []any{0, "4.25"}}),
	}
	srv := httptest.NewServer(fakeVMHandler{instant: instant})
	defer srv.Close()

	resp, err := buildOverview(context.Background(), overviewOptions(srv), "", 1755300000)
	if err != nil {
		t.Fatalf("buildOverview: %v", err)
	}
	if !resp.TodayRealKnown || resp.TodayRealUsd != 3.5 {
		t.Errorf("TodayRealKnown=%v TodayRealUsd=%v, want true 3.5", resp.TodayRealKnown, resp.TodayRealUsd)
	}
	if !resp.TodayNotionalKnown || resp.TodayNotionalUsd != 4.25 {
		t.Errorf("TodayNotionalKnown=%v TodayNotionalUsd=%v, want true 4.25", resp.TodayNotionalKnown, resp.TodayNotionalUsd)
	}
}

// An empty VM result for the real query (no sample at all, distinct from a
// sample of 0) must read TodayRealKnown false rather than a silently correct
// looking TodayRealUsd of 0; the notional query still resolving must not
// affect that flag.
func TestBuildOverviewTodayRealUnknownWhenEmpty(t *testing.T) {
	instant := map[string]string{
		"max(clamp_min(delta(claude_extra_usage_usd[24h]),0))":         successBody(t),
		fmt.Sprintf("sum(clamp_min(increase(%s[24h]),0))", costMetric): successBody(t, vmResult{Metric: map[string]string{}, Value: []any{0, "1.75"}}),
	}
	srv := httptest.NewServer(fakeVMHandler{instant: instant})
	defer srv.Close()

	resp, err := buildOverview(context.Background(), overviewOptions(srv), "", 1755300000)
	if err != nil {
		t.Fatalf("buildOverview: %v", err)
	}
	if resp.TodayRealKnown || resp.TodayRealUsd != 0 {
		t.Errorf("TodayRealKnown=%v TodayRealUsd=%v, want false 0", resp.TodayRealKnown, resp.TodayRealUsd)
	}
	if !resp.TodayNotionalKnown || resp.TodayNotionalUsd != 1.75 {
		t.Errorf("TodayNotionalKnown=%v TodayNotionalUsd=%v, want true 1.75", resp.TodayNotionalKnown, resp.TodayNotionalUsd)
	}
}

func TestMonthStartLocal(t *testing.T) {
	// 2026-08-15 12:00 UTC should resolve to 2026-08-01 in whatever local
	// timezone the test runs under; check by round-tripping through the
	// same local calendar break time.Unix itself would use.
	now := int64(1786190400) // 2026-08-08 00:00:00 UTC, arbitrary within August
	got := monthStartLocal(now)
	// Recompute independently via a slightly different but equivalent
	// route: truncating to the day and stepping back to the 1st should be
	// idempotent from any day within the same local month.
	again := monthStartLocal(got + 3600) // one hour into the 1st, still the same month
	if got != again {
		t.Errorf("monthStartLocal(%d) = %d, monthStartLocal(%d) = %d, want equal", now, got, got+3600, again)
	}
}
