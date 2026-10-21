package history

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// fakeQR is a RangeQuerier that records every query it receives, in the
// order received, and returns a canned response keyed by the exact query
// string. Tests assert both the recorded queries (byte-identical to what
// history.js would have issued) and the merged result.
type fakeQR struct {
	queries   []string
	starts    []int64
	ends      []int64
	steps     []string
	responses map[string][]vm.Series
}

func (f *fakeQR) QR(_ context.Context, query string, start, end int64, step string) ([]vm.Series, error) {
	f.queries = append(f.queries, query)
	f.starts = append(f.starts, start)
	f.ends = append(f.ends, end)
	f.steps = append(f.steps, step)
	return f.responses[query], nil
}

func series(model string, points ...[2]float64) vm.Series {
	values := make([][2]any, len(points))
	for i, p := range points {
		values[i] = [2]any{p[0], p[1]}
	}
	metric := map[string]string{}
	if model != "" {
		metric["model"] = model
	}
	return vm.Series{Metric: metric, Values: values}
}

func modelKeyOf(labels map[string]string, _ string) (string, bool) {
	v, ok := labels["model"]
	return v, ok
}

func TestDayWindow(t *testing.T) {
	tests := []struct {
		name string
		days int
		now  int64
		want Window
	}{
		{
			name: "boundary already passed today, steps back to it",
			days: 3,
			now:  1755244800, // 2025-08-15 08:00 UTC
			want: Window{Start: 1755054000, End: 1755226800, Step: DAY},
		},
		{
			name: "now just before 03:00 UTC steps back a whole day",
			days: 1,
			now:  1755226740, // 2025-08-15 02:59 UTC
			want: Window{Start: 1755140400, End: 1755140400, Step: DAY},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DayWindow(tt.days, tt.now)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("DayWindow(%d, %d) diff:\n%s", tt.days, tt.now, diff)
			}
		})
	}
}

func TestRollingWindow(t *testing.T) {
	got := RollingWindow(7, 1000000, 3600)
	want := Window{Start: 1000000 - 6*3600, End: 1000000, Step: 3600}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RollingWindow diff:\n%s", diff)
	}
}

func TestCanMerge(t *testing.T) {
	tests := []struct {
		name string
		kind string
		by   []string
		want bool
	}{
		{"cost by model", "cost", []string{"model"}, true},
		{"cost by agent.name is not historical", "cost", []string{"agent.name"}, false},
		{"pullRequests has no history series at all", "pullRequests", []string{"model"}, false},
		{"cost by model and type, both historical", "cost", []string{"model", "type"}, true},
		{"tokens with no grouping", "tokens", []string{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CanMerge(tt.kind, tt.by)
			if got != tt.want {
				t.Errorf("CanMerge(%q, %v) = %v, want %v", tt.kind, tt.by, got, tt.want)
			}
		})
	}
}

func TestMergedTotalsDailyFillRuleAndQueries(t *testing.T) {
	f := &fakeQR{responses: map[string][]vm.Series{
		"sum by (model)(sum_over_time(claude_hist_daily_cost_usd[1d]))": {
			series("opus", [2]float64{0, 10}, [2]float64{86400, 20}),
		},
		"sum by (model)(increase(claude_code.cost.usage[1d]))": {
			series("opus", [2]float64{0, 5}, [2]float64{86400, 7}, [2]float64{172800, 3}),
		},
	}}

	got, err := MergedTotals(context.Background(), f, MergedParams{
		Kind: "cost", By: []string{"model"}, KeyOf: modelKeyOf,
		Start: 0, End: 172800, Step: DAY,
	})
	if err != nil {
		t.Fatalf("MergedTotals() error = %v", err)
	}

	// History covers buckets 0 and 1 (10 + 20 = 30). The live values at those
	// same buckets are ignored because history already filled them; only
	// live's bucket-2 value (3) counts, since history left it empty.
	want := map[string]float64{"opus": 33}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("MergedTotals() diff:\n%s", diff)
	}

	wantQueries := []string{
		"sum by (model)(sum_over_time(claude_hist_daily_cost_usd[1d]))",
		"sum by (model)(increase(claude_code.cost.usage[1d]))",
	}
	if diff := cmp.Diff(wantQueries, f.queries); diff != "" {
		t.Errorf("queries diff:\n%s", diff)
	}
}

func TestMergedTotalsHourlyStepSwapsSeriesAndWindow(t *testing.T) {
	f := &fakeQR{responses: map[string][]vm.Series{
		"sum by (model)(sum_over_time(claude_hist_cost_usd[1h]))": {
			series("opus", [2]float64{0, 1}),
		},
		"sum by (model)(increase(claude_code.cost.usage[1h]))": {
			series("opus", [2]float64{0, 2}),
		},
	}}

	_, err := MergedTotals(context.Background(), f, MergedParams{
		Kind: "cost", By: []string{"model"}, KeyOf: modelKeyOf,
		Start: 0, End: 0, Step: HOUR,
	})
	if err != nil {
		t.Fatalf("MergedTotals() error = %v", err)
	}

	wantQueries := []string{
		"sum by (model)(sum_over_time(claude_hist_cost_usd[1h]))",
		"sum by (model)(increase(claude_code.cost.usage[1h]))",
	}
	if diff := cmp.Diff(wantQueries, f.queries); diff != "" {
		t.Errorf("queries diff:\n%s", diff)
	}
}

func TestMergedTotalsHistoryFalseSuppressesHistoryQuery(t *testing.T) {
	f := &fakeQR{responses: map[string][]vm.Series{
		"sum by (model)(increase(claude_code.cost.usage[1d]))": {
			series("opus", [2]float64{0, 9}),
		},
	}}

	historyOff := false
	got, err := MergedTotals(context.Background(), f, MergedParams{
		Kind: "cost", By: []string{"model"}, KeyOf: modelKeyOf,
		History: &historyOff,
		Start:   0, End: 0, Step: DAY,
	})
	if err != nil {
		t.Fatalf("MergedTotals() error = %v", err)
	}

	wantQueries := []string{"sum by (model)(increase(claude_code.cost.usage[1d]))"}
	if diff := cmp.Diff(wantQueries, f.queries); diff != "" {
		t.Errorf("queries diff (history=false must suppress the history query entirely):\n%s", diff)
	}
	want := map[string]float64{"opus": 9}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("MergedTotals() diff:\n%s", diff)
	}
}

func TestMergedTotalsNonMergeableByLabelSuppressesHistoryQuery(t *testing.T) {
	f := &fakeQR{responses: map[string][]vm.Series{
		"sum by (agent.name)(increase(claude_code.cost.usage[1d]))": {
			series("", [2]float64{0, 4}),
		},
	}}

	got, err := MergedTotals(context.Background(), f, MergedParams{
		Kind: "cost", By: []string{"agent.name"},
		KeyOf: func(labels map[string]string, _ string) (string, bool) {
			return "agent", true
		},
		Start: 0, End: 0, Step: DAY,
	})
	if err != nil {
		t.Fatalf("MergedTotals() error = %v", err)
	}

	wantQueries := []string{"sum by (agent.name)(increase(claude_code.cost.usage[1d]))"}
	if diff := cmp.Diff(wantQueries, f.queries); diff != "" {
		t.Errorf("queries diff (non-historical by-label must suppress the history query):\n%s", diff)
	}
	want := map[string]float64{"agent": 4}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("MergedTotals() diff:\n%s", diff)
	}
}

func TestUSD(t *testing.T) {
	tests := []struct {
		v    float64
		want string
	}{
		{1234.56, "$1,234.56"},
		{0.01, "$0.01"},
	}
	for _, tt := range tests {
		if got := usd(tt.v); got != tt.want {
			t.Errorf("usd(%v) = %q, want %q", tt.v, got, tt.want)
		}
	}
}

func TestLiveOnlyNote(t *testing.T) {
	f := &fakeQR{responses: map[string][]vm.Series{
		"sum(increase(claude_code.cost.usage[1d]))": {
			series("", [2]float64{0, 100}),
		},
	}}
	win := Window{Start: 0, End: 0, Step: DAY}

	got, err := LiveOnlyNote(context.Background(), f, "agent.name", 40, win)
	if err != nil {
		t.Fatalf("LiveOnlyNote() error = %v", err)
	}
	want := ". The transcript backfill records no agent.name, so this is the live collector only: $60.00 of the period's $100.00 is not broken down here"
	if got != want {
		t.Errorf("LiveOnlyNote() = %q, want %q", got, want)
	}
}

func TestLiveOnlyNoteEmptyWhenMergeable(t *testing.T) {
	f := &fakeQR{responses: map[string][]vm.Series{}}
	win := Window{Start: 0, End: 0, Step: DAY}

	got, err := LiveOnlyNote(context.Background(), f, "model", 40, win)
	if err != nil {
		t.Fatalf("LiveOnlyNote() error = %v", err)
	}
	if got != "" {
		t.Errorf("LiveOnlyNote() = %q, want empty (model is mergeable)", got)
	}
	if len(f.queries) != 0 {
		t.Errorf("LiveOnlyNote() issued queries %v, want none for a mergeable label", f.queries)
	}
}

// An unknown kind has no seriesTable entry. The JS would hit this by
// dereferencing SERIES[kind].daily on an undefined value and throwing a
// TypeError; the Go port must fail loud with a returned error instead of
// silently building a query against an empty metric name (which would read
// back as history/live totals of $0, indistinguishable from a real empty
// period).
func TestMergedTotalsUnknownKindErrors(t *testing.T) {
	f := &fakeQR{responses: map[string][]vm.Series{}}
	_, err := MergedTotals(context.Background(), f, MergedParams{
		Kind: "nonsense", By: []string{}, KeyOf: func(map[string]string, string) (string, bool) { return "x", true },
		Start: 0, End: 0, Step: DAY,
	})
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("MergedTotals() error = %v, want an error containing %q", err, "unknown kind")
	}
	if len(f.queries) != 0 {
		t.Errorf("MergedTotals() issued queries %v before failing, want none", f.queries)
	}
}

func TestHistoryBucketsUnknownKindErrors(t *testing.T) {
	f := &fakeQR{responses: map[string][]vm.Series{}}
	_, err := HistoryBuckets(context.Background(), f, "nonsense", Window{Start: 0, End: 0, Step: DAY})
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("HistoryBuckets() error = %v, want an error containing %q", err, "unknown kind")
	}
	if len(f.queries) != 0 {
		t.Errorf("HistoryBuckets() issued queries %v before failing, want none", f.queries)
	}
}

func TestHistoryBucketsExcludesZeroValuedBuckets(t *testing.T) {
	f := &fakeQR{responses: map[string][]vm.Series{
		"sum(sum_over_time(claude_hist_daily_cost_usd[1d]))": {
			series("", [2]float64{0, 5}, [2]float64{86400, 0}, [2]float64{172800, 3}),
		},
	}}
	w := Window{Start: 0, End: 172800, Step: DAY}

	got, err := HistoryBuckets(context.Background(), f, "cost", w)
	if err != nil {
		t.Fatalf("HistoryBuckets() error = %v", err)
	}
	// The middle bucket's zero value is excluded: HistoryBuckets says which
	// buckets the backfill actually covers, and a zero sample is not
	// coverage.
	want := map[int]bool{0: true, 2: true}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("HistoryBuckets() diff:\n%s", diff)
	}

	wantQueries := []string{"sum(sum_over_time(claude_hist_daily_cost_usd[1d]))"}
	if diff := cmp.Diff(wantQueries, f.queries); diff != "" {
		t.Errorf("queries diff:\n%s", diff)
	}
}

func TestHistoryBucketsEmptyResponseYieldsEmptySet(t *testing.T) {
	f := &fakeQR{responses: map[string][]vm.Series{}}
	w := Window{Start: 0, End: 172800, Step: DAY}

	got, err := HistoryBuckets(context.Background(), f, "cost", w)
	if err != nil {
		t.Fatalf("HistoryBuckets() error = %v", err)
	}
	if diff := cmp.Diff(map[int]bool{}, got); diff != "" {
		t.Errorf("HistoryBuckets() diff:\n%s", diff)
	}
}

// sidSeries builds one session's series, labelled by session.id.
func sidSeries(sid string, points ...[2]float64) vm.Series {
	values := make([][2]any, len(points))
	for i, p := range points {
		values[i] = [2]any{p[0], p[1]}
	}
	return vm.Series{Metric: map[string]string{"session.id": sid}, Values: values}
}

// Commits and pull requests are running totals for one session, not flows.
// Reading them with increase() summed across buckets was wrong in both
// directions on real data: a session whose commits all landed before the
// window exports a flat series, and increase() over flat is 0, so a session
// with 50 commits read as 0; another, extrapolated at the window edge, read
// 48 against a true 30.
func TestMergedTotalsReadsDeliveryCountersAsRunningTotals(t *testing.T) {
	const start, end = 0, 3 * DAY
	f := &fakeQR{responses: map[string][]vm.Series{}}

	// Ask once to learn the query this kind now issues, then answer it.
	if _, err := MergedTotals(context.Background(), f, MergedParams{
		Kind: "commits", By: []string{"session.id"}, Start: start, End: end, Step: DAY,
	}); err != nil {
		t.Fatal(err)
	}
	q := f.queries[0]
	if !strings.Contains(q, "max_over_time") {
		t.Fatalf("delivery counter still read with increase(): %s", q)
	}
	// The lookbehind must span the whole request, not one bucket: a session
	// split across two series by a mid-session restart is only summed
	// correctly when both are inside the window.
	if !strings.Contains(q, "[345600]") { // end-start+step
		t.Errorf("lookbehind is not the full span: %s", q)
	}

	f.responses[q] = []vm.Series{
		// Flat across every bucket: the tally predates the window. Must
		// report 50, not 0, and not 50 per bucket summed to 200.
		sidSeries("flat", [2]float64{0, 50}, [2]float64{DAY, 50}, [2]float64{2 * DAY, 50}, [2]float64{3 * DAY, 50}),
		// Rising within the window: the answer is the final value.
		sidSeries("rising", [2]float64{0, 2}, [2]float64{DAY, 11}, [2]float64{2 * DAY, 24}, [2]float64{3 * DAY, 30}),
	}
	got, err := MergedTotals(context.Background(), f, MergedParams{
		Kind: "commits", By: []string{"session.id"}, Start: start, End: end, Step: DAY,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"flat": 50, "rising": 30}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("delivery totals (-want +got):\n%s", diff)
	}
}

// The same machinery must keep treating cost as a flow: its buckets are
// disjoint arrivals and adding them up is the whole point.
func TestMergedTotalsStillSumsCostBuckets(t *testing.T) {
	const start, end = 0, 2 * DAY
	f := &fakeQR{responses: map[string][]vm.Series{}}
	if _, err := MergedTotals(context.Background(), f, MergedParams{
		Kind: "cost", By: []string{"session.id"}, Start: start, End: end, Step: DAY, History: boolPtr(false),
	}); err != nil {
		t.Fatal(err)
	}
	q := f.queries[0]
	if !strings.Contains(q, "increase(") || strings.Contains(q, "max_over_time") {
		t.Fatalf("cost is not a running total and must stay on increase(): %s", q)
	}
	f.responses[q] = []vm.Series{
		sidSeries("s", [2]float64{0, 1.5}, [2]float64{DAY, 2.5}, [2]float64{2 * DAY, 3}),
	}
	got, err := MergedTotals(context.Background(), f, MergedParams{
		Kind: "cost", By: []string{"session.id"}, Start: start, End: end, Step: DAY, History: boolPtr(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := 7.0; got["s"] != want {
		t.Errorf("cost total = %v, want %v (buckets summed)", got["s"], want)
	}
}

func boolPtr(b bool) *bool { return &b }

/* Selector and Match -------------------------------------------------------- */

// VictoriaMetrics caches instant-query results by query string, so a selector
// whose label order wandered between calls would miss its own cache every
// time. Map iteration order in Go is deliberately random, which is exactly the
// shape of bug that only shows under load.
func TestSelectorIsStableAndOptional(t *testing.T) {
	if got := Selector("claude_hist_daily_cost_usd", nil); got != "claude_hist_daily_cost_usd" {
		t.Errorf("no match rewrote the metric name: %q", got)
	}
	if got := Selector("m", map[string]string{}); got != "m" {
		t.Errorf("an empty match rewrote the metric name: %q", got)
	}

	match := map[string]string{"profile": "acme", "account": "5e6f7a8b", "source": "cache"}
	want := `m{account="5e6f7a8b",profile="acme",source="cache"}`
	// Repeated, because one pass cannot show that map order was defeated.
	for i := 0; i < 20; i++ {
		if got := Selector("m", match); got != want {
			t.Fatalf("pass %d: Selector = %q, want %q", i, got, want)
		}
	}
}

// Filtering on a label the backfill never recorded is the same hazard as
// grouping by one: the history selector matches nothing, the total silently
// becomes the live counter alone, and reading the live counter alone once made
// one panel report a thirteenth of what the chart beside it read.
func TestMergedTotalsRefusesToMergeOnANonHistoricalFilter(t *testing.T) {
	if !HasHistory("profile") {
		t.Fatal("profile is expected to be a label the backfill records")
	}
	if HasHistory("agent.name") {
		t.Fatal("agent.name is expected NOT to be a backfilled label")
	}

	var asked []string
	qr := queryRecorder{seen: &asked}
	ctx := context.Background()

	// A historical filter queries both halves.
	asked = nil
	if _, err := MergedTotals(ctx, qr, MergedParams{
		Kind: "cost", By: []string{"model"}, Match: map[string]string{"profile": "acme"},
		Start: 0, End: DAY, Step: DAY,
	}); err != nil {
		t.Fatal(err)
	}
	if !containsSub(asked, "claude_hist_daily_cost_usd") {
		t.Errorf("a historical filter skipped the history half: %v", asked)
	}
	if !containsSub(asked, `profile="acme"`) {
		t.Errorf("the filter never reached the query: %v", asked)
	}

	// A non-historical one queries only the live half, rather than asking
	// history a question it cannot answer and reading the empty result as $0.
	asked = nil
	if _, err := MergedTotals(ctx, qr, MergedParams{
		Kind: "cost", By: []string{"model"}, Match: map[string]string{"agent.name": "Explore"},
		Start: 0, End: DAY, Step: DAY,
	}); err != nil {
		t.Fatal(err)
	}
	if containsSub(asked, "claude_hist_daily_cost_usd") {
		t.Errorf("a non-historical filter still queried the history half: %v", asked)
	}
	if !containsSub(asked, costMetric) {
		t.Errorf("the live half was not queried at all: %v", asked)
	}
}

type queryRecorder struct{ seen *[]string }

func (q queryRecorder) QR(_ context.Context, query string, _, _ int64, _ string) ([]vm.Series, error) {
	*q.seen = append(*q.seen, query)
	return nil, nil
}

func containsSub(all []string, sub string) bool {
	for _, s := range all {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
