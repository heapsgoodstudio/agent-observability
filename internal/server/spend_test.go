package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/history"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

func spendOptions(vmSrv *httptest.Server, d *sql.DB) Options {
	return Options{
		VM:   &vm.Client{BaseURL: vmSrv.URL, HTTP: vmSrv.Client()},
		Memo: store.New(store.TTL),
		DB:   d,
	}
}

// seedSpendIndex seeds the sessions table with one row per session id ->
// raw project name entry, the only source the project join reads from. It
// also sets the sessions_generated_at meta row RunIndex
// writes at the end of every real pass, regardless of session count, so a
// zero-session call here still represents a completed, legitimately empty
// index rather than a never-indexed db: listIndexedSessions treats those
// two states differently (a present meta row with zero sessions succeeds;
// no meta row at all errors as errNeverIndexed). A nil d (rather than an
// empty seed) is this package's own stand-in for "no index at all."
func seedSpendIndex(t *testing.T, d *sql.DB, sessions map[string]string) {
	t.Helper()
	for id, project := range sessions {
		if err := db.UpsertSession(d, db.SessionRow{ID: id, Project: project}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.MetaSet(d, sessionsGeneratedAtKey, "1755300000"); err != nil {
		t.Fatal(err)
	}
}

/* periodBounds -------------------------------------------------------------- */

// All five periods, checked against periodBounds' own math rather than
// against magic numbers: this makes the test assert the relationship
// periodBounds embeds, not a frozen snapshot of it.
func TestPeriodBoundsAllFivePeriods(t *testing.T) {
	now := int64(1755300000) // 2025-08-16 00:00:00 UTC, arbitrary

	t.Run("24h", func(t *testing.T) {
		w := periodBounds("24h", now)
		if w.Chart.Start != now-history.DAY || w.Chart.End != now || w.Chart.Step != history.HOUR {
			t.Errorf("24h chart = %+v", w.Chart)
		}
		wantTable := history.RollingWindow(24, now, history.HOUR)
		if w.Table != wantTable {
			t.Errorf("24h table = %+v, want %+v", w.Table, wantTable)
		}
		if w.BilledStart != now-history.DAY || w.BilledEnd != now {
			t.Errorf("24h billed = [%d,%d], want [%d,%d]", w.BilledStart, w.BilledEnd, now-history.DAY, now)
		}
	})

	t.Run("7d", func(t *testing.T) {
		w := periodBounds("7d", now)
		wantChart := history.DayWindow(7, now)
		if w.Chart != wantChart {
			t.Errorf("7d chart = %+v, want %+v", w.Chart, wantChart)
		}
		wantTable := history.RollingWindow(7, now, history.DAY)
		if w.Table != wantTable {
			t.Errorf("7d table = %+v, want %+v", w.Table, wantTable)
		}
		if w.BilledStart != now-7*history.DAY || w.BilledEnd != now {
			t.Errorf("7d billed = [%d,%d]", w.BilledStart, w.BilledEnd)
		}
	})

	t.Run("30d", func(t *testing.T) {
		w := periodBounds("30d", now)
		wantChart := history.DayWindow(30, now)
		if w.Chart != wantChart {
			t.Errorf("30d chart = %+v, want %+v", w.Chart, wantChart)
		}
		if w.BilledStart != now-30*history.DAY || w.BilledEnd != now {
			t.Errorf("30d billed = [%d,%d]", w.BilledStart, w.BilledEnd)
		}
	})

	t.Run("mtd starts at the local month start", func(t *testing.T) {
		w := periodBounds("mtd", now)
		monthStart := monthStartLocal(now)
		if w.BilledStart != monthStart || w.BilledEnd != now {
			t.Errorf("mtd billed = [%d,%d], want [%d,%d]", w.BilledStart, w.BilledEnd, monthStart, now)
		}
		wantDays := int((now-monthStart)/history.DAY) + 1
		wantChart := history.DayWindow(wantDays, now)
		if w.Chart != wantChart {
			t.Errorf("mtd chart = %+v, want %+v (days=%d)", w.Chart, wantChart, wantDays)
		}
	})

	// "all" clamps to a fixed 730-day span, not a probe for the earliest
	// sample (see buildDailyCost's own leading-bucket trim, tested below):
	// billedStart reuses the day-aligned chart.start.
	t.Run("all uses a fixed 730-day window, not a probe", func(t *testing.T) {
		w := periodBounds("all", now)
		wantChart := history.DayWindow(allDays, now)
		if w.Chart != wantChart {
			t.Errorf("all chart = %+v, want %+v", w.Chart, wantChart)
		}
		if w.BilledStart != wantChart.Start || w.BilledEnd != now {
			t.Errorf("all billed = [%d,%d], want [%d,%d]", w.BilledStart, w.BilledEnd, wantChart.Start, now)
		}
	})

	t.Run("unrecognized period falls back to 30d", func(t *testing.T) {
		if normalizePeriod("bogus") != "30d" || normalizePeriod("") != "30d" {
			t.Error("normalizePeriod should default unknown/empty values to 30d")
		}
		if normalizeMeasure("bogus") != "notional" || normalizeMeasure("") != "notional" {
			t.Error("normalizeMeasure should default unknown/empty values to notional")
		}
		if normalizeMeasure("billed") != "billed" {
			t.Error("normalizeMeasure should preserve billed")
		}
	})
}

/* Daily cost: query shape per period, and the fill rule -------------------- */

// recordingHandler wraps a fakeVMHandler and records every "query" param it
// sees, in order: the "all" period's own test uses this to prove there is no
// separate earliest-sample probe query, only the two the chart always
// issues.
type recordingHandler struct {
	fakeVMHandler
	seen *[]string
}

func (h recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	*h.seen = append(*h.seen, r.URL.Query().Get("query")+"|step="+r.URL.Query().Get("step"))
	h.fakeVMHandler.ServeHTTP(w, r)
}

// 24h must query the hourly per-request history series (claude_hist_cost_usd)
// at step 3600, not the daily rollup.
func TestBuildDailyCost24hUsesHourlyPerRequestSeries(t *testing.T) {
	histQuery := "sum by (model)(sum_over_time(claude_hist_cost_usd[1h]))"
	liveQuery := "sum by (model)(increase(claude_code.cost.usage[1h]))"
	ranged := map[string]string{
		histQuery: successBody(t, vmResult{Metric: map[string]string{"model": "claude-opus-4-5-20250101"}, Values: [][2]any{{float64(1755300000 - 3600), "3"}}}),
	}
	var seen []string
	srv := httptest.NewServer(recordingHandler{fakeVMHandler{ranged: ranged}, &seen})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	win := periodBounds("24h", 1755300000)
	series, total, err := buildDailyCost(context.Background(), vmc, profileScope{}, "24h", win.Chart)
	if err != nil {
		t.Fatalf("buildDailyCost: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %v, want 3", total)
	}
	if len(series) != 1 || series[0].Label != "opus-4-5" {
		t.Fatalf("series = %+v, want one opus-4-5 series", series)
	}

	wantHist := histQuery + "|step=3600"
	wantLive := liveQuery + "|step=3600"
	var gotHist, gotLive bool
	for _, q := range seen {
		if q == wantHist {
			gotHist = true
		}
		if q == wantLive {
			gotLive = true
		}
	}
	if !gotHist || !gotLive {
		t.Errorf("seen queries = %v, want %q and %q", seen, wantHist, wantLive)
	}
}

// The "all" period issues exactly the chart's own hist+live queries, never a
// third probe query for the earliest sample: buildDailyCost trims leading
// empty buckets from the data it already has instead.
func TestBuildDailyCostAllPeriodIssuesNoProbeQuery(t *testing.T) {
	now := int64(1755300000)
	win := periodBounds("all", now)
	// Cost starts 3 days before "now" only; every earlier bucket in the
	// 730-day chart window is empty.
	t0 := float64(win.Chart.End - 3*history.DAY)
	ranged := map[string]string{
		"sum by (model)(sum_over_time(claude_hist_daily_cost_usd[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"model": "claude-opus-4-5-20250101"}, Values: [][2]any{{t0, "7"}}},
		),
	}
	var seen []string
	srv := httptest.NewServer(recordingHandler{fakeVMHandler{ranged: ranged}, &seen})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	series, total, err := buildDailyCost(context.Background(), vmc, profileScope{}, "all", win.Chart)
	if err != nil {
		t.Fatalf("buildDailyCost: %v", err)
	}
	if total != 7 {
		t.Errorf("total = %v, want 7", total)
	}
	if len(series) != 1 || len(series[0].Points) != 4 {
		// 3 days back through "now" inclusive at a day step is 4 points,
		// once the ~730 empty leading buckets are trimmed.
		t.Fatalf("series = %+v, want 1 series of 4 trimmed points", series)
	}

	// Exactly two distinct queries issued (hist range + live range), no
	// third probe query.
	distinct := map[string]bool{}
	for _, q := range seen {
		distinct[q] = true
	}
	if len(distinct) != 2 {
		t.Errorf("distinct queries issued = %v, want exactly 2 (hist + live), no earliest-sample probe", distinct)
	}
}

// The merge rule: a bucket history already covered is history's; the live
// counter only fills the gaps history left. Two buckets, hist fills the
// first and a live point that lands in the same bucket must be dropped,
// while a live point in the second (hist-empty) bucket must count.
func TestBuildDailyCostFillRule(t *testing.T) {
	now := int64(1755300000)
	chart := history.DayWindow(2, now)
	t0 := float64(chart.Start)
	t1 := float64(chart.Start + history.DAY)

	ranged := map[string]string{
		"sum by (model)(sum_over_time(claude_hist_daily_cost_usd[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"model": "claude-opus-4-5-20250101"}, Values: [][2]any{{t0, "10"}}},
		),
		"sum by (model)(increase(claude_code.cost.usage[1d]))": successBody(t,
			// Same bucket as history: must be ignored.
			vmResult{Metric: map[string]string{"model": "claude-opus-4-5-20250101"}, Values: [][2]any{{t0, "999"}}},
			// The bucket history left empty: must be counted.
			vmResult{Metric: map[string]string{"model": "claude-haiku-4-5-20250101"}, Values: [][2]any{{t1, "5"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	series, total, err := buildDailyCost(context.Background(), vmc, profileScope{}, "7d", chart)
	if err != nil {
		t.Fatalf("buildDailyCost: %v", err)
	}
	if total != 15 {
		t.Fatalf("total = %v, want 15 (10 from history + 5 from live, not 1014)", total)
	}

	byModel := map[string][]float64{}
	for _, s := range series {
		byModel[s.Label] = []float64{s.Points[0].Value, s.Points[1].Value}
	}
	if got := byModel["opus-4-5"]; len(got) != 2 || got[0] != 10 || got[1] != 0 {
		t.Errorf("opus-4-5 points = %v, want [10, 0]", got)
	}
	if got := byModel["haiku-4-5"]; len(got) != 2 || got[0] != 0 || got[1] != 5 {
		t.Errorf("haiku-4-5 points = %v, want [0, 5]", got)
	}
}

// topNWithOther's "other" bucket, exercised through buildDailyCost: six
// distinct models must collapse to the top 4 by total plus one "other"
// series summing the remaining two.
func TestBuildDailyCostOtherBucketAggregatesTail(t *testing.T) {
	now := int64(1755300000)
	// A single-bucket chart window keeps the per-bucket values equal to each
	// model's whole total, so the assertions can compare directly against
	// the input values.
	chart := history.Window{Start: now, End: now, Step: history.DAY}

	modelValues := []struct {
		raw string
		val string
	}{
		{"claude-model-a-20250101", "60"},
		{"claude-model-b-20250101", "50"},
		{"claude-model-c-20250101", "40"},
		{"claude-model-d-20250101", "30"},
		{"claude-model-e-20250101", "20"},
		{"claude-model-f-20250101", "10"},
	}
	results := make([]vmResult, len(modelValues))
	for i, m := range modelValues {
		results[i] = vmResult{Metric: map[string]string{"model": m.raw}, Values: [][2]any{{float64(now), m.val}}}
	}
	ranged := map[string]string{
		"sum by (model)(sum_over_time(claude_hist_daily_cost_usd[1d]))": successBody(t, results...),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	series, total, err := buildDailyCost(context.Background(), vmc, profileScope{}, "7d", chart)
	if err != nil {
		t.Fatalf("buildDailyCost: %v", err)
	}
	if total != 210 {
		t.Fatalf("total = %v, want 210 (60+50+40+30+20+10)", total)
	}
	if len(series) != 5 {
		t.Fatalf("series = %+v, want 5 (top 4 by total plus other)", series)
	}
	byLabel := map[string]float64{}
	for _, s := range series {
		if len(s.Points) != 1 {
			t.Fatalf("series %q has %d points, want 1", s.Label, len(s.Points))
		}
		byLabel[s.Label] = s.Points[0].Value
	}
	want := map[string]float64{"model-a": 60, "model-b": 50, "model-c": 40, "model-d": 30, "other": 30}
	for label, v := range want {
		if byLabel[label] != v {
			t.Errorf("byLabel[%q] = %v, want %v (byLabel=%+v)", label, byLabel[label], v, byLabel)
		}
	}
	if _, ok := byLabel["model-e"]; ok {
		t.Error("model-e should have been folded into \"other\", not kept as its own series")
	}
	if _, ok := byLabel["model-f"]; ok {
		t.Error("model-f should have been folded into \"other\", not kept as its own series")
	}
}

/* Heatmap --------------------------------------------------------------- */

// A sum_over_time([1h]) sample belongs at the hour it covers, not the
// timestamp VM reports: the sample at t covers the hour ENDING at t, so it
// belongs at t - 3600. heatmapPointsToProto must apply that shift.
func TestHeatmapPointsToProtoShiftsByOneHour(t *testing.T) {
	ts := float64(1755300000)
	pts := []vm.Point{{TS: ts, V: 4.5}}
	out := heatmapPointsToProto(pts)
	if len(out) != 1 {
		t.Fatalf("out = %+v, want 1 point", out)
	}
	if out[0].Ts != ts-3600 {
		t.Errorf("Ts = %v, want %v (input minus 3600)", out[0].Ts, ts-3600)
	}
	if out[0].Value != 4.5 {
		t.Errorf("Value = %v, want 4.5, unchanged by the shift", out[0].Value)
	}
}

// End-to-end through the RPC: a fake VM heatmap response with a known
// timestamp must come back with ts shifted by -3600 in GetSpendResponse.
func TestBuildSpendHeatmapShiftsBuckets(t *testing.T) {
	d := openTestDB(t)
	seedSpendIndex(t, d, map[string]string{})
	now := int64(1755300000)
	win := periodBounds("7d", now)
	sampleTs := float64(win.Chart.End)
	ranged := map[string]string{
		"sum(sum_over_time(claude_hist_cost_usd[1h]))": successBody(t,
			vmResult{Metric: map[string]string{}, Values: [][2]any{{sampleTs, "2.5"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()

	resp, err := buildSpend(context.Background(), spendOptions(srv, d), "7d", "notional", "", "", now)
	if err != nil {
		t.Fatalf("buildSpend: %v", err)
	}
	if len(resp.HeatmapHourly) != 1 {
		t.Fatalf("HeatmapHourly = %+v, want 1 point", resp.HeatmapHourly)
	}
	if resp.HeatmapHourly[0].Ts != sampleTs-3600 {
		t.Errorf("Ts = %v, want %v (sample ts minus 3600)", resp.HeatmapHourly[0].Ts, sampleTs-3600)
	}
}

/* Billed masking ------------------------------------------------------------ */

// End-to-end billed masking on a synthetic band: points inside the band
// count, points outside do not, matching overage.MaskSeries's inclusive edges.
func TestBilledTotalsMasksAgainstBands(t *testing.T) {
	t0 := int64(1755300000)
	end := t0 + 1800
	ranged := map[string]string{
		"sum by (model)(increase(claude_code.cost.usage[10m]))": successBody(t,
			vmResult{Metric: map[string]string{"model": "claude-opus-4-5-20250101"}, Values: [][2]any{
				{float64(t0), "2"},        // inside the band
				{float64(t0 + 600), "3"},  // inside the band
				{float64(t0 + 1200), "4"}, // outside the band
				{float64(t0 + 1800), "5"}, // outside the band
			}},
		),
		"max(claude_in_overage) == 1": successBody(t,
			vmResult{Metric: map[string]string{}, Values: [][2]any{{float64(t0), "1"}, {float64(t0 + 600), "1"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	res, err := billedTotals(context.Background(), vmc, profileScope{}, "model", func(v string) string { return v }, t0, end)
	if err != nil {
		t.Fatalf("billedTotals: %v", err)
	}
	if len(res.Bands) != 1 || res.Bands[0][0] != float64(t0) || res.Bands[0][1] != float64(t0+600) {
		t.Fatalf("bands = %+v, want one band [%d, %d]", res.Bands, t0, t0+600)
	}
	if got := res.Totals["claude-opus-4-5-20250101"]; got != 5 {
		t.Errorf("masked total = %v, want 5 (2+3, not 2+3+4+5=14)", got)
	}
	if res.Clamped {
		t.Error("Clamped = true, want false: span is well under MAX_POINTS")
	}
}

// A window wider than maxBilledPoints*billedStepS must clamp its start to end
// nearest "now", and report Clamped=true so billedNote can say so.
func TestBilledTotalsClampsToMaxPoints(t *testing.T) {
	end := int64(1755300000)
	rawStart := end - (maxBilledPoints+1000)*billedStepS // well past the cap
	srv := httptest.NewServer(fakeVMHandler{})           // empty success for everything
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	res, err := billedTotals(context.Background(), vmc, profileScope{}, "model", func(v string) string { return v }, rawStart, end)
	if err != nil {
		t.Fatalf("billedTotals: %v", err)
	}
	wantStart := end - (maxBilledPoints-1)*billedStepS
	if res.Start != wantStart {
		t.Errorf("Start = %d, want %d", res.Start, wantStart)
	}
	if !res.Clamped {
		t.Error("Clamped = false, want true")
	}
}

/* Model usage note ----------------------------------------------------------- */

// A cost bucket the backfill covers but the token backfill does not must
// produce the gap warning: singular "1 day", the unit picked from the
// window's step.
func TestBuildModelUsageNoteWithGap(t *testing.T) {
	t0 := int64(1755200000)
	table := history.Window{Start: t0, End: t0 + history.DAY, Step: history.DAY}

	ranged := map[string]string{
		"sum by (model)(sum_over_time(claude_hist_daily_cost_usd[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"model": "claude-model-a-20250101"}, Values: [][2]any{
				{float64(t0), "10"}, {float64(t0 + history.DAY), "5"},
			}},
		),
		"sum by (model,type)(sum_over_time(claude_hist_daily_tokens[1d]))": successBody(t,
			// Token history exists only for the first bucket; the second is
			// the gap.
			vmResult{Metric: map[string]string{"model": "claude-model-a-20250101", "type": "output"}, Values: [][2]any{{float64(t0), "1000"}}},
		),
		"sum(sum_over_time(claude_hist_daily_cost_usd[1d]))": successBody(t,
			vmResult{Metric: map[string]string{}, Values: [][2]any{{float64(t0), "10"}, {float64(t0 + history.DAY), "5"}}},
		),
		"sum(sum_over_time(claude_hist_daily_tokens[1d]))": successBody(t,
			vmResult{Metric: map[string]string{}, Values: [][2]any{{float64(t0), "1000"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	rows, note, err := buildModelUsage(context.Background(), vmc, profileScope{}, "7d", table)
	if err != nil {
		t.Fatalf("buildModelUsage: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "model-a" || rows[0].CostUsd != 15 {
		t.Fatalf("rows = %+v, want one model-a row costing 15", rows)
	}
	want := "$15.00 and 1,000 tokens across 1 model, 7 days" +
		". 1 day of backfilled history carry cost but no token detail," +
		" so the rate columns read high for them: re-run claude-obs backfill to record it"
	if note != want {
		t.Errorf("note = %q, want %q", note, want)
	}
}

// The same shape with no gap (token history covers every bucket cost does)
// must produce the plain note, with no gap sentence appended.
func TestBuildModelUsageNoteWithoutGap(t *testing.T) {
	t0 := int64(1755200000)
	table := history.Window{Start: t0, End: t0 + history.DAY, Step: history.DAY}

	ranged := map[string]string{
		"sum by (model)(sum_over_time(claude_hist_daily_cost_usd[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"model": "claude-model-a-20250101"}, Values: [][2]any{
				{float64(t0), "10"}, {float64(t0 + history.DAY), "5"},
			}},
		),
		"sum by (model,type)(sum_over_time(claude_hist_daily_tokens[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"model": "claude-model-a-20250101", "type": "output"}, Values: [][2]any{
				{float64(t0), "1000"}, {float64(t0 + history.DAY), "500"},
			}},
		),
		"sum(sum_over_time(claude_hist_daily_cost_usd[1d]))": successBody(t,
			vmResult{Metric: map[string]string{}, Values: [][2]any{{float64(t0), "10"}, {float64(t0 + history.DAY), "5"}}},
		),
		"sum(sum_over_time(claude_hist_daily_tokens[1d]))": successBody(t,
			vmResult{Metric: map[string]string{}, Values: [][2]any{{float64(t0), "1000"}, {float64(t0 + history.DAY), "500"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	_, note, err := buildModelUsage(context.Background(), vmc, profileScope{}, "7d", table)
	if err != nil {
		t.Fatalf("buildModelUsage: %v", err)
	}
	want := "$15.00 and 1,500 tokens across 1 model, 7 days"
	if note != want {
		t.Errorf("note = %q, want %q (no gap sentence)", note, want)
	}
}

// An hourly table window (the 24h period's own shape, matching
// TestBuildDailyCost24hUsesHourlyPerRequestSeries's pattern) must pick
// "hour" as the gap sentence's unit, not "day".
func TestBuildModelUsageNoteHourlyWindowUsesHourUnit(t *testing.T) {
	t0 := int64(1755200000)
	table := history.Window{Start: t0, End: t0 + history.HOUR, Step: history.HOUR}

	ranged := map[string]string{
		"sum by (model)(sum_over_time(claude_hist_cost_usd[1h]))": successBody(t,
			vmResult{Metric: map[string]string{"model": "claude-model-a-20250101"}, Values: [][2]any{
				{float64(t0), "10"}, {float64(t0 + history.HOUR), "5"},
			}},
		),
		"sum by (model,type)(sum_over_time(claude_hist_tokens[1h]))": successBody(t,
			// Token history exists only for the first bucket; the second is
			// the gap.
			vmResult{Metric: map[string]string{"model": "claude-model-a-20250101", "type": "output"}, Values: [][2]any{{float64(t0), "1000"}}},
		),
		"sum(sum_over_time(claude_hist_cost_usd[1h]))": successBody(t,
			vmResult{Metric: map[string]string{}, Values: [][2]any{{float64(t0), "10"}, {float64(t0 + history.HOUR), "5"}}},
		),
		"sum(sum_over_time(claude_hist_tokens[1h]))": successBody(t,
			vmResult{Metric: map[string]string{}, Values: [][2]any{{float64(t0), "1000"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	rows, note, err := buildModelUsage(context.Background(), vmc, profileScope{}, "24h", table)
	if err != nil {
		t.Fatalf("buildModelUsage: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "model-a" || rows[0].CostUsd != 15 {
		t.Fatalf("rows = %+v, want one model-a row costing 15", rows)
	}
	want := "$15.00 and 1,000 tokens across 1 model, 24 hours" +
		". 1 hour of backfilled history carry cost but no token detail," +
		" so the rate columns read high for them: re-run claude-obs backfill to record it"
	if note != want {
		t.Errorf("note = %q, want %q", note, want)
	}
}

// A gap of 2 or more buckets must pluralize the unit ("2 days").
func TestBuildModelUsageNoteGapPluralizesUnit(t *testing.T) {
	t0 := int64(1755200000)
	table := history.Window{Start: t0, End: t0 + 2*history.DAY, Step: history.DAY}

	ranged := map[string]string{
		"sum by (model)(sum_over_time(claude_hist_daily_cost_usd[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"model": "claude-model-a-20250101"}, Values: [][2]any{
				{float64(t0), "10"}, {float64(t0 + history.DAY), "5"}, {float64(t0 + 2*history.DAY), "3"},
			}},
		),
		"sum by (model,type)(sum_over_time(claude_hist_daily_tokens[1d]))": successBody(t,
			// Token history exists only for the first bucket; the second
			// and third are both gaps.
			vmResult{Metric: map[string]string{"model": "claude-model-a-20250101", "type": "output"}, Values: [][2]any{{float64(t0), "1000"}}},
		),
		"sum(sum_over_time(claude_hist_daily_cost_usd[1d]))": successBody(t,
			vmResult{Metric: map[string]string{}, Values: [][2]any{
				{float64(t0), "10"}, {float64(t0 + history.DAY), "5"}, {float64(t0 + 2*history.DAY), "3"},
			}},
		),
		"sum(sum_over_time(claude_hist_daily_tokens[1d]))": successBody(t,
			vmResult{Metric: map[string]string{}, Values: [][2]any{{float64(t0), "1000"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	rows, note, err := buildModelUsage(context.Background(), vmc, profileScope{}, "7d", table)
	if err != nil {
		t.Fatalf("buildModelUsage: %v", err)
	}
	if len(rows) != 1 || rows[0].Model != "model-a" || rows[0].CostUsd != 18 {
		t.Fatalf("rows = %+v, want one model-a row costing 18", rows)
	}
	want := "$18.00 and 1,000 tokens across 1 model, 7 days" +
		". 2 days of backfilled history carry cost but no token detail," +
		" so the rate columns read high for them: re-run claude-obs backfill to record it"
	if note != want {
		t.Errorf("note = %q, want %q", note, want)
	}
}

/* Delivery / project join --------------------------------------------------- */

// The delivery panel's session.id -> project join: a session absent from the
// index must land under notIndexed rather than being dropped.
// deliveryPRFrag matches the pull-request counter by substring: it is read
// with max_over_time over a lookbehind spanning the whole request, so the
// exact query text depends on the window under test.
const deliveryPRFrag = `claude_code.pull_request.count`

func TestBuildDeliveryJoinsSessionsToProjectsAndFlagsUnindexed(t *testing.T) {
	now := int64(1755300000)
	table := history.RollingWindow(30, now, history.DAY)
	d := openTestDB(t)
	seedSpendIndex(t, d, map[string]string{
		"s1": "-Users-test-proja",
	})

	ranged := map[string]string{
		deliveryPRFrag: successBody(t,
			vmResult{Metric: map[string]string{"session.id": "s1"}, Values: [][2]any{{float64(table.End), "2"}}},
			vmResult{Metric: map[string]string{"session.id": "s2"}, Values: [][2]any{{float64(table.End), "1"}}},
		),
		"sum by (session.id)(increase(claude_code.cost.usage[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"session.id": "s1"}, Values: [][2]any{{float64(table.End), "10"}}},
			vmResult{Metric: map[string]string{"session.id": "s2"}, Values: [][2]any{{float64(table.End), "4"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	prs, commits, sessionCost, rows, err := buildDelivery(context.Background(), vmc, d, profileScope{}, table)
	if err != nil {
		t.Fatalf("buildDelivery: %v", err)
	}
	if prs != 3 || commits != 0 {
		t.Errorf("prs=%v commits=%v, want prs=3 commits=0", prs, commits)
	}
	if sessionCost != 14 {
		t.Errorf("sessionCost = %v, want 14", sessionCost)
	}
	byProject := map[string]*claudeobsProject{}
	for _, r := range rows {
		byProject[r.Project] = &claudeobsProject{prs: r.Prs, cost: r.SessionCostUsd}
	}
	if p, ok := byProject["proja"]; !ok || p.prs != 2 || p.cost != 10 {
		t.Errorf("rows = %+v, want a proja row with prs=2 cost=10", rows)
	}
	if p, ok := byProject[notIndexed]; !ok || p.prs != 1 || p.cost != 4 {
		t.Errorf("rows = %+v, want a %q row with prs=1 cost=4", rows, notIndexed)
	}
}

// claudeobsProject is a small local shape for the delivery test above; it
// only exists to make the assertions read as a lookup rather than a linear
// scan over rows.
type claudeobsProject struct {
	prs, cost float64
}

// The by-project attribution: the client-side join through the index,
// merged with the historical "project" label where the backfill has it, and
// NOT_INDEXED for a session the index does not know about.
func TestBuildProjectAttributionJoinsAndFlagsUnindexed(t *testing.T) {
	now := int64(1755300000)
	win := periodBounds("30d", now)
	d := openTestDB(t)
	seedSpendIndex(t, d, map[string]string{
		"s1": "-Users-test-proja",
		"s2": "-Users-test-projb",
	})

	ranged := map[string]string{
		"sum by (session.id)(increase(claude_code.cost.usage[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"session.id": "s1"}, Values: [][2]any{{float64(win.Table.End), "10"}}},
			vmResult{Metric: map[string]string{"session.id": "s2"}, Values: [][2]any{{float64(win.Table.End), "5"}}},
			vmResult{Metric: map[string]string{"session.id": "s3"}, Values: [][2]any{{float64(win.Table.End), "7"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	a := buildProjectAttribution(context.Background(), vmc, d, profileScope{}, "notional", win, win, "")
	if a.Error != "" {
		t.Fatalf("Error = %q, want none", a.Error)
	}
	if len(a.Rows) != 3 {
		t.Fatalf("Rows = %+v, want 3 rows", a.Rows)
	}
	byKey := map[string]float64{}
	for _, r := range a.Rows {
		byKey[r.Key] = r.Value
	}
	if byKey["proja"] != 10 || byKey["projb"] != 5 || byKey[notIndexed] != 7 {
		t.Errorf("byKey = %+v, want proja=10 projb=5 %q=7", byKey, notIndexed)
	}
}

// TestBuildProjectAttributionNeverIndexedErrors is
// TestBuildProjectAttributionJoinsAndFlagsUnindexed's counterpart for a
// real, migrated db that has just never completed an indexing pass (no
// sessions_generated_at meta row), rather than one that has and legitimately
// has no rows in it yet. listIndexedSessions must surface it as an
// Attribution.Error rather than silently attributing every session to
// notIndexed.
func TestBuildProjectAttributionNeverIndexedErrors(t *testing.T) {
	now := int64(1755300000)
	win := periodBounds("30d", now)
	// A real, migrated db, but seedSpendIndex is never called: no
	// sessions_generated_at meta row.
	d := openTestDB(t)

	ranged := map[string]string{
		"sum by (session.id)(increase(claude_code.cost.usage[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"session.id": "s1"}, Values: [][2]any{{float64(win.Table.End), "10"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	a := buildProjectAttribution(context.Background(), vmc, d, profileScope{}, "notional", win, win, "")
	if a.Error == "" {
		t.Error("Error is empty, want a failure surfaced for a db that has never completed an indexing pass")
	}
}

// A missing session index must fail the delivery panel (a page-level
// SectionError through buildSpend), not silently drop the join.
func TestBuildDeliveryMissingIndexErrors(t *testing.T) {
	now := int64(1755300000)
	table := history.RollingWindow(30, now, history.DAY)
	ranged := map[string]string{
		deliveryPRFrag: successBody(t,
			vmResult{Metric: map[string]string{"session.id": "s1"}, Values: [][2]any{{float64(table.End), "1"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	_, _, _, _, err := buildDelivery(context.Background(), vmc, nil, profileScope{}, table)
	if err == nil {
		t.Fatal("want an error when the session index is missing but there is delivery data to join")
	}
}

// TestBuildDeliveryNeverIndexedErrors is TestBuildDeliveryMissingIndexErrors'
// counterpart for a real, migrated db that has just never completed an
// indexing pass (no sessions_generated_at meta row), rather than no db at
// all. listIndexedSessions must treat it as an error, not a legitimately
// empty index (zero rows with a real db is not
// itself an error, per TestBuildDeliveryJoinsSessionsToProjectsAndFlagsUnindexed's
// own seeded-but-real db).
func TestBuildDeliveryNeverIndexedErrors(t *testing.T) {
	now := int64(1755300000)
	table := history.RollingWindow(30, now, history.DAY)
	ranged := map[string]string{
		deliveryPRFrag: successBody(t,
			vmResult{Metric: map[string]string{"session.id": "s1"}, Values: [][2]any{{float64(table.End), "1"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	// A real, migrated db, but seedSpendIndex is never called: no
	// sessions_generated_at meta row, distinct from the nil-db case above.
	d := openTestDB(t)

	_, _, _, _, err := buildDelivery(context.Background(), vmc, d, profileScope{}, table)
	if err == nil {
		t.Fatal("want an error when the db has never completed an indexing pass but there is delivery data to join")
	}
}

/* Per-attribution degradation ------------------------------------------------ */

// One attribution's query failing must land in that Attribution's own Error
// field, not blank the other six.
func TestBuildAttributionsPerAttributionErrorDegrades(t *testing.T) {
	now := int64(1755300000)
	win := periodBounds("7d", now)
	d := openTestDB(t)
	seedSpendIndex(t, d, map[string]string{})

	failQuery := "sum by (query_source)(increase(claude_code.cost.usage[1d]))"
	srv := httptest.NewServer(fakeVMHandler{fail: failQuery})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	attrs := buildAttributions(context.Background(), vmc, d, profileScope{}, "notional", win, "")
	if len(attrs) != len(simpleAttributions)+1 {
		t.Fatalf("attrs = %d, want %d (every simple label plus project)", len(attrs), len(simpleAttributions)+1)
	}
	byLabel := map[string]*struct {
		Error string
	}{}
	for _, a := range attrs {
		byLabel[a.Label] = &struct{ Error string }{a.Error}
	}
	if byLabel["query_source"].Error == "" {
		t.Error("query_source Attribution.Error is empty, want the failure surfaced there")
	}
	for _, label := range []string{"model", "profile", "agent.name", "skill.name", "effort", "mcp_server.name", "project"} {
		if byLabel[label].Error != "" {
			t.Errorf("%s Attribution.Error = %q, want empty (the failure is scoped to query_source alone)", label, byLabel[label].Error)
		}
	}

	wantOrder := []string{"model", "profile", "query_source", "agent.name", "skill.name", "effort", "mcp_server.name", "project"}
	for i, w := range wantOrder {
		if attrs[i].Label != w {
			t.Errorf("attrs[%d].Label = %q, want %q (response order is fixed)", i, attrs[i].Label, w)
		}
	}
}

// The project request field filters the project attribution's rows only,
// keeping the requested (raw, unshortened) project's row alone.
func TestProjectFilterAppliesOnlyToProjectAttribution(t *testing.T) {
	now := int64(1755300000)
	win := periodBounds("7d", now)
	d := openTestDB(t)
	seedSpendIndex(t, d, map[string]string{"s1": "-Users-test-proja", "s2": "-Users-test-projb"})

	ranged := map[string]string{
		"sum by (session.id)(increase(claude_code.cost.usage[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"session.id": "s1"}, Values: [][2]any{{float64(win.Table.End), "10"}}},
			vmResult{Metric: map[string]string{"session.id": "s2"}, Values: [][2]any{{float64(win.Table.End), "5"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	// The filter matches the workspace name the rows are keyed by, not the
	// raw project key. These rows carry no workspace (seeded before the
	// column existed, as a real stale row would), so the name falls back to
	// the shortened project key and "proja" is both the key and the filter.
	a := buildProjectAttribution(context.Background(), vmc, d, profileScope{}, "notional", win, win, "proja")
	if len(a.Rows) != 1 || a.Rows[0].Key != "proja" || a.Rows[0].Value != 10 {
		t.Fatalf("filtered rows = %+v, want one proja=10 row", a.Rows)
	}
}

// Two sessions under one project directory, working in different
// repositories, must land in two rows keyed by repository. Keying on the
// project directory would merge them, which is the whole reason the workspace
// name exists.
func TestProjectAttributionKeysOnWorkspace(t *testing.T) {
	now := int64(1755300000)
	win := periodBounds("7d", now)
	d := openTestDB(t)
	for _, r := range []db.SessionRow{
		{ID: "s1", Project: "-Users-test-mono", Workspace: "acme/switch"},
		{ID: "s2", Project: "-Users-test-mono", Workspace: "acme/terraform"},
	} {
		if err := db.UpsertSession(d, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.MetaSet(d, sessionsGeneratedAtKey, "1755300000"); err != nil {
		t.Fatal(err)
	}

	ranged := map[string]string{
		"sum by (session.id)(increase(claude_code.cost.usage[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"session.id": "s1"}, Values: [][2]any{{float64(win.Table.End), "10"}}},
			vmResult{Metric: map[string]string{"session.id": "s2"}, Values: [][2]any{{float64(win.Table.End), "5"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	a := buildProjectAttribution(context.Background(), vmc, d, profileScope{}, "notional", win, win, "")
	got := map[string]float64{}
	for _, r := range a.Rows {
		got[r.Key] = r.Value
	}
	if len(got) != 2 || got["acme/switch"] != 10 || got["acme/terraform"] != 5 {
		t.Fatalf("rows = %+v, want acme/switch=10 and acme/terraform=5", got)
	}
}

// The history half is grouped by the backfill's own workspace label, so a
// session's backfilled dollars and its live dollars land in one row rather
// than two. A mismatch here is the split this design exists to prevent.
func TestProjectAttributionMergesHistoryOnWorkspaceLabel(t *testing.T) {
	now := int64(1755300000)
	win := periodBounds("7d", now)
	d := openTestDB(t)
	if err := db.UpsertSession(d, db.SessionRow{ID: "s1", Project: "-Users-test-mono", Workspace: "acme/switch"}); err != nil {
		t.Fatal(err)
	}
	if err := db.MetaSet(d, sessionsGeneratedAtKey, "1755300000"); err != nil {
		t.Fatal(err)
	}

	// History covers the first bucket; the live counter covers a later one,
	// so both contribute and neither is skipped as an already-filled bucket.
	ranged := map[string]string{
		"sum by (workspace)(sum_over_time(claude_hist_daily_cost_usd[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"workspace": "acme/switch"}, Values: [][2]any{{float64(win.Table.Start), "7"}}},
		),
		"sum by (session.id)(increase(claude_code.cost.usage[1d]))": successBody(t,
			vmResult{Metric: map[string]string{"session.id": "s1"}, Values: [][2]any{{float64(win.Table.End), "3"}}},
		),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged, rangedContains: ranged})
	defer srv.Close()
	vmc := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	a := buildProjectAttribution(context.Background(), vmc, d, profileScope{}, "notional", win, win, "")
	if len(a.Rows) != 1 || a.Rows[0].Key != "acme/switch" || a.Rows[0].Value != 10 {
		t.Fatalf("rows = %+v, want one acme/switch=10 row (7 history + 3 live)", a.Rows)
	}
}

/* Page-level section degradation -------------------------------------------- */

// A single failing page-level query (the hour-heatmap's own query, which no
// other section shares) must degrade to a SectionError, not an RPC-level
// error, and must not touch the other three page-level sections or the
// attributions. daily-cost and model-usage both query "cost by model" over
// their own windows and so share a query shape (deduped by Memo in
// production on the full query+range key); the heatmap's bare
// sum(sum_over_time(...)) with no grouping is the one query no other
// section issues, which is what makes it useful as an isolation point here.
func TestBuildSpendSectionErrorOnSingleQueryFailure(t *testing.T) {
	d := openTestDB(t)
	seedSpendIndex(t, d, map[string]string{})
	failQuery := "sum(sum_over_time(claude_hist_cost_usd[1h]))"
	srv := httptest.NewServer(fakeVMHandler{fail: failQuery})
	defer srv.Close()

	resp, err := buildSpend(context.Background(), spendOptions(srv, d), "7d", "notional", "", "", 1755300000)
	if err != nil {
		t.Fatalf("buildSpend returned an RPC-level error for one failing section: %v", err)
	}
	if len(resp.Errors) != 1 || resp.Errors[0].Section != "hour-heatmap" {
		t.Fatalf("Errors = %+v, want exactly one SectionError for hour-heatmap", resp.Errors)
	}
	if len(resp.Attributions) != len(simpleAttributions)+1 {
		t.Errorf("Attributions = %d, want %d even though the heatmap failed", len(resp.Attributions), len(simpleAttributions)+1)
	}
}

// When every page-level section fails, buildSpend must return an RPC-level
// error, matching buildOverview's own all-failed behavior.
func TestBuildSpendErrorsWhenEveryPageSectionFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := buildSpend(context.Background(), spendOptions(srv, nil), "7d", "notional", "", "", 1755300000)
	if err == nil {
		t.Fatal("want an error when every page-level section fails, got nil")
	}
}

/* measure_note ---------------------------------------------------------------- */

// measure_note must carry MEASURE_NOT_APPLIED verbatim when billed was
// requested, and stay empty for notional.
func TestBuildSpendMeasureNote(t *testing.T) {
	d := openTestDB(t)
	seedSpendIndex(t, d, map[string]string{})
	srv := httptest.NewServer(fakeVMHandler{}) // empty success for everything
	defer srv.Close()

	billed, err := buildSpend(context.Background(), spendOptions(srv, d), "7d", "billed", "", "", 1755300000)
	if err != nil {
		t.Fatalf("buildSpend (billed): %v", err)
	}
	if billed.MeasureNote != measureNotApplied {
		t.Errorf("MeasureNote = %q, want %q", billed.MeasureNote, measureNotApplied)
	}

	notional, err := buildSpend(context.Background(), spendOptions(srv, d), "7d", "notional", "", "", 1755300000)
	if err != nil {
		t.Fatalf("buildSpend (notional): %v", err)
	}
	if notional.MeasureNote != "" {
		t.Errorf("MeasureNote = %q, want empty for notional", notional.MeasureNote)
	}
}
