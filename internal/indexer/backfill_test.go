package indexer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newBackfillFixture lays out one project dir holding the shared fixture
// transcript under a fixed project directory name and session id, so the
// expected totals below (n=3, total=0.002695, ...) are pinned to those
// exact bytes.
func newBackfillFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	proj := filepath.Join(root, "-Users-test-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(proj, "11111111-1111-4111-8111-111111111111.jsonl")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestBackfillTotalsMatchParser ports test_totals_match_parser.
func TestBackfillTotalsMatchParser(t *testing.T) {
	root := newBackfillFixture(t)
	cost, tokens, daily, dailyTokens, total, n, err := collect(root, "", noRepos)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("n = %d, want 3", n)
	}
	almostEqual(t, "total", total, 0.002695)

	var costSum float64
	for _, v := range cost {
		costSum += v
	}
	almostEqual(t, "sum(cost)", costSum, total)

	var sideTotal float64
	for k, v := range cost {
		if k.Sidechain == "1" {
			sideTotal += v
		}
	}
	almostEqual(t, "sidechain cost total", sideTotal, 0.00011)

	var inputSum int64
	for k, v := range tokens {
		if k.Type == "input" {
			inputSum += v
		}
	}
	if inputSum != 115 {
		t.Errorf("input token sum = %d, want 115", inputSum)
	}

	// The rollups are a regrouping, never a second count: each must sum to
	// its own per-request map, or a panel reading one and a panel reading
	// the other disagree on the same money.
	var dailySum float64
	for _, v := range daily {
		dailySum += v
	}
	almostEqual(t, "sum(daily)", dailySum, total)

	var tokenSum, dailyTokenSum int64
	for _, v := range tokens {
		tokenSum += v
	}
	for _, v := range dailyTokens {
		dailyTokenSum += v
	}
	if tokenSum != dailyTokenSum {
		t.Errorf("sum(dailyTokens) = %d, want %d (sum(tokens))", dailyTokenSum, tokenSum)
	}
}

// TestBackfillLinesArePrometheusShaped ports test_lines_are_prometheus_shaped.
func TestBackfillLinesArePrometheusShaped(t *testing.T) {
	root := newBackfillFixture(t)
	cost, tokens, daily, dailyTokens, _, _, err := collect(root, "", noRepos)
	if err != nil {
		t.Fatal(err)
	}
	lines := toLines(cost, tokens, daily, dailyTokens)

	var hasCost, hasCacheRead, hasDailyTokens bool
	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) < 2 {
			t.Errorf("line has no space-separated timestamp: %q", l)
			continue
		}
		ts := fields[len(fields)-1]
		for _, r := range ts {
			if r < '0' || r > '9' {
				t.Errorf("timestamp field %q is not all digits, in line %q", ts, l)
				break
			}
		}
		if strings.HasPrefix(l, "claude_hist_cost_usd{") {
			hasCost = true
		}
		if strings.Contains(l, `type="cacheRead"`) {
			hasCacheRead = true
		}
		if strings.HasPrefix(l, "claude_hist_daily_tokens{") {
			hasDailyTokens = true
		}
	}
	if !hasCost {
		t.Error("no claude_hist_cost_usd line")
	}
	if !hasCacheRead {
		t.Error(`no line with type="cacheRead"`)
	}
	if !hasDailyTokens {
		t.Error("no claude_hist_daily_tokens line")
	}
}

// TestBackfillBothRollupsStampSameDayIdentically pins that the cost and
// token rollups land on the same day stamp for the same request.
func TestBackfillBothRollupsStampSameDayIdentically(t *testing.T) {
	root := newBackfillFixture(t)
	cost, tokens, daily, dailyTokens, _, _, err := collect(root, "", noRepos)
	if err != nil {
		t.Fatal(err)
	}
	lines := toLines(cost, tokens, daily, dailyTokens)

	stamp := func(prefix string) map[string]bool {
		out := map[string]bool{}
		for _, l := range lines {
			if strings.HasPrefix(l, prefix) {
				fields := strings.Fields(l)
				out[fields[len(fields)-1]] = true
			}
		}
		return out
	}
	costStamps := stamp("claude_hist_daily_cost_usd{")
	tokenStamps := stamp("claude_hist_daily_tokens{")
	if !reflect.DeepEqual(costStamps, tokenStamps) {
		t.Errorf("daily cost stamps = %v, daily token stamps = %v", costStamps, tokenStamps)
	}
}

// TestBatchLinesSplitsAt20000 is the brief's new case: batching splits at
// 20000 lines on the line-builder alone, no network involved.
func TestBatchLinesSplitsAt20000(t *testing.T) {
	lines := make([]string, 45000)
	for i := range lines {
		lines[i] = strings.Repeat("x", 1) + string(rune('a'+i%26))
	}
	batches := batchLines(lines, BatchLines)
	if len(batches) != 3 {
		t.Fatalf("got %d batches, want 3", len(batches))
	}
	if len(batches[0]) != BatchLines || len(batches[1]) != BatchLines {
		t.Errorf("first two batches = %d, %d lines, want %d each", len(batches[0]), len(batches[1]), BatchLines)
	}
	if len(batches[2]) != 5000 {
		t.Errorf("last batch = %d lines, want 5000", len(batches[2]))
	}
	var flat []string
	for _, b := range batches {
		flat = append(flat, b...)
	}
	if !reflect.DeepEqual(flat, lines) {
		t.Error("batches do not reassemble into the original slice, in order")
	}
}

func TestBatchLinesEmptyIsNoBatches(t *testing.T) {
	if got := batchLines(nil, BatchLines); len(got) != 0 {
		t.Errorf("got %d batches for no lines, want 0", len(got))
	}
}

// TestEscEscapesQuoteBackslashNewline is the brief's other new case:
// prometheus escaping of quote/backslash/newline in label values.
func TestEscEscapesQuoteBackslashNewline(t *testing.T) {
	got := esc("a\\b\"c\nd")
	if !strings.Contains(got, `\\`) {
		t.Errorf("esc did not double the backslash: %q", got)
	}
	if !strings.Contains(got, `\"`) {
		t.Errorf("esc did not escape the quote: %q", got)
	}
	if !strings.Contains(got, `\n`) {
		t.Errorf("esc did not escape the newline: %q", got)
	}
	if strings.ContainsRune(got, '\n') {
		t.Errorf("esc left a literal newline in the output: %q", got)
	}
	if want := `a\\b\"c\nd`; got != want {
		t.Errorf("esc(%q) = %q, want %q", "a\\b\"c\nd", got, want)
	}
}

// --- HTTP bits, each tested behind httptest, no real network -------------

func TestHealthCheckOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q, want /health", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := healthCheck(srv.URL); err != nil {
		t.Fatalf("healthCheck: %v", err)
	}
}

func TestHealthCheckUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // frees the port: nothing answers there anymore
	if err := healthCheck(url); err == nil {
		t.Fatal("want error for an unreachable VM")
	}
}

func TestHealthCheckNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := healthCheck(srv.URL); err == nil {
		t.Fatal("want error for a 500 health response")
	}
}

func TestPostBatchSendsPrometheusLines(t *testing.T) {
	var gotPath, gotBody, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := postBatch(srv.URL, []string{"a 1", "b 2"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/import/prometheus" {
		t.Errorf("path = %q, want /api/v1/import/prometheus", gotPath)
	}
	if gotBody != "a 1\nb 2" {
		t.Errorf("body = %q, want %q", gotBody, "a 1\nb 2")
	}
	if gotType == "" {
		t.Error("no Content-Type header sent")
	}
}

func TestResetRollupCacheHitsEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := resetRollupCache(srv.URL); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/internal/resetRollupResultCache" {
		t.Errorf("path = %q, want /internal/resetRollupResultCache", gotPath)
	}
}

// Both cost series must carry the workspace label, since GetSpend's
// by-project attribution groups its history half on exactly that label; a
// series missing it lands every historical dollar in one "unlabelled" row.
func TestBackfillCostLinesCarryWorkspaceLabel(t *testing.T) {
	root := newBackfillFixture(t)
	resolver := mapResolver{
		tops: map[string]string{"/w/acme/switch": "/w/acme/switch"},
		urls: map[string]string{"/w/acme/switch": "git@github.com:acme/switch.git"},
	}
	cost, tokens, daily, dailyTokens, _, _, err := collect(root, "", resolver)
	if err != nil {
		t.Fatal(err)
	}
	lines := toLines(cost, tokens, daily, dailyTokens)

	var sawCost, sawDaily bool
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "claude_hist_cost_usd{"):
			sawCost = true
		case strings.HasPrefix(l, "claude_hist_daily_cost_usd{"):
			sawDaily = true
		default:
			continue
		}
		if !strings.Contains(l, `workspace="`) {
			t.Fatalf("cost line carries no workspace label: %q", l)
		}
		// project stays alongside it: it is still the key the sessions table
		// joins on, and dropping it would be a silent schema change.
		if !strings.Contains(l, `project="`) {
			t.Fatalf("cost line lost its project label: %q", l)
		}
	}
	if !sawCost || !sawDaily {
		t.Fatalf("saw cost line = %v, saw daily line = %v, want both", sawCost, sawDaily)
	}
}

// The token series carry no project or workspace label, so a label change
// never orphans them and RunBackfill must not delete them.
func TestBackfillResetTargetsOnlyCostSeries(t *testing.T) {
	want := map[string]bool{
		`{__name__="claude_hist_cost_usd"}`:       false,
		`{__name__="claude_hist_daily_cost_usd"}`: false,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/admin/tsdb/delete_series" {
			// POST with a form body, not a query string: VictoriaMetrics
			// answers this one endpoint with 405 on GET.
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			m := r.PostForm.Get("match[]")
			if _, ok := want[m]; !ok {
				t.Errorf("deleted an unexpected series: %q", m)
			}
			want[m] = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for _, m := range costSeries {
		if err := deleteSeries(srv.URL, m); err != nil {
			t.Fatal(err)
		}
	}
	for m, deleted := range want {
		if !deleted {
			t.Errorf("series %s was never deleted", m)
		}
	}
}

func TestQueryTotalParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("query"); got != "sum(x)" {
			t.Errorf("query param = %q, want sum(x)", got)
		}
		w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"value":[1755200000,"12.34"]}]}}`))
	}))
	defer srv.Close()
	got, err := queryTotal(srv.URL, "sum(x)")
	if err != nil {
		t.Fatal(err)
	}
	almostEqual(t, "queryTotal", got, 12.34)
}

func TestQueryTotalEmptyResultIsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer srv.Close()
	got, err := queryTotal(srv.URL, "sum(x)")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

// --- RunBackfill end to end, against httptest, no real network -----------

func TestRunBackfillDryRunSkipsNetwork(t *testing.T) {
	root := newBackfillFixture(t)
	// An address nothing listens on: if DryRun leaked a network call, this
	// would fail fast with connection refused instead of silently passing.
	stats, err := RunBackfill(BackfillOpts{ProjectsRoot: root, VMURL: "http://127.0.0.1:1", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Requests != 3 {
		t.Errorf("Requests = %d, want 3", stats.Requests)
	}
	almostEqual(t, "Total", stats.Total, 0.002695)
	if stats.Reconciled {
		t.Error("dry run must not reconcile against VM")
	}
}

func TestRunBackfillImportsAndReconciles(t *testing.T) {
	root := newBackfillFixture(t)
	var imported int
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/v1/import/prometheus", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			imported += len(strings.Split(string(b), "\n"))
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/internal/resetRollupResultCache", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"value":[0,"0.002695"]}]}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stats, err := RunBackfill(BackfillOpts{ProjectsRoot: root, VMURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Reconciled {
		t.Fatal("want Reconciled = true")
	}
	if stats.Warning {
		t.Error("want no warning: computed and VM totals matched")
	}
	almostEqual(t, "VMTotal", stats.VMTotal, 0.002695)
	if imported == 0 {
		t.Error("no lines reached the import endpoint")
	}
}

// fastReconcile shrinks the reconciliation poll so tests that deliberately
// drive it to a verdict do not pay the production settle time.
func fastReconcile(t *testing.T) {
	t.Helper()
	interval, settle, timeout := reconcileInterval, reconcileMinSettle, reconcileTimeout
	reconcileInterval, reconcileMinSettle, reconcileTimeout = time.Millisecond, 0, 2*time.Second
	t.Cleanup(func() {
		reconcileInterval, reconcileMinSettle, reconcileTimeout = interval, settle, timeout
	})
}

// The bug this guards: the reconciliation used to read the total exactly once,
// one second after the import, and freshly imported samples are not queryable
// that soon. A real 135k-sample import read $0.00 on the first query and the
// exact computed total about twenty seconds later, so every backfill warned
// about an import that was fine. Polling must wait the lag out rather than
// convict on the first reading.
func TestRunBackfillWaitsOutIngestionLagBeforeWarning(t *testing.T) {
	fastReconcile(t)
	root := newBackfillFixture(t)
	var queries int
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/v1/import/prometheus", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/internal/resetRollupResultCache", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		queries++
		// Nothing queryable yet, then a partial total, then the real one.
		switch queries {
		case 1:
			w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		case 2:
			w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"value":[0,"0.001"]}]}}`))
		default:
			w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"value":[0,"0.002695"]}]}}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stats, err := RunBackfill(BackfillOpts{ProjectsRoot: root, VMURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Warning {
		t.Errorf("want no warning once the total settles, got VMTotal %v after %d queries", stats.VMTotal, queries)
	}
	almostEqual(t, "VMTotal", stats.VMTotal, 0.002695)
	if queries < 3 {
		t.Errorf("want the poll to outlast the lag, got %d queries", queries)
	}
}

// The same bug in its longer form, which --reset-cost-series makes routine: a
// delete's tombstones are applied before the re-imported samples surface, so
// the total reads a stable 0 for long enough that the stability heuristic
// calls it settled and warns. Measured on the live install: a reset run
// reported $0.00 against a computed total that was exact moments later.
// Zero against a non-zero computed total must never count as settled.
func TestRunBackfillDoesNotSettleOnZeroAfterReset(t *testing.T) {
	fastReconcile(t)
	root := newBackfillFixture(t)
	var queries int
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/v1/admin/tsdb/delete_series", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/api/v1/import/prometheus", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/internal/resetRollupResultCache", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		queries++
		// A stable zero for long enough that the old heuristic would have
		// convicted, then the true total.
		if queries <= 5 {
			w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"value":[0,"0"]}]}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"value":[0,"0.002695"]}]}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stats, err := RunBackfill(BackfillOpts{ProjectsRoot: root, VMURL: srv.URL, ResetCostSeries: true})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Reset {
		t.Error("Reset = false, want true")
	}
	if stats.Warning {
		t.Errorf("warned on a stable zero that later resolved; VMTotal %v after %d queries", stats.VMTotal, queries)
	}
	almostEqual(t, "VMTotal", stats.VMTotal, 0.002695)
}

// A total that never converges must still reach a warning rather than poll to
// the timeout, so a genuine mismatch is reported promptly.
func TestRunBackfillWarnsOnceAStableMismatchSettles(t *testing.T) {
	fastReconcile(t)
	root := newBackfillFixture(t)
	var queries int
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/v1/import/prometheus", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/internal/resetRollupResultCache", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		queries++
		w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"value":[0,"1.0"]}]}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	start := time.Now()
	stats, err := RunBackfill(BackfillOpts{ProjectsRoot: root, VMURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Warning {
		t.Error("want Warning = true for a stable >1% mismatch")
	}
	if elapsed := time.Since(start); elapsed >= reconcileTimeout {
		t.Errorf("want a verdict from stability, not the timeout; took %v", elapsed)
	}
}

func TestRunBackfillWarnsOnMismatch(t *testing.T) {
	fastReconcile(t)
	root := newBackfillFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/v1/import/prometheus", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/internal/resetRollupResultCache", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		// Far off from the fixture's 0.002695 computed total: >1% mismatch.
		w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"value":[0,"1.0"]}]}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stats, err := RunBackfill(BackfillOpts{ProjectsRoot: root, VMURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Warning {
		t.Error("want Warning = true for a >1% reconciliation mismatch")
	}
}

func TestRunBackfillHealthCheckFailureIsError(t *testing.T) {
	root := newBackfillFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := RunBackfill(BackfillOpts{ProjectsRoot: root, VMURL: srv.URL}); err == nil {
		t.Fatal("want error when VM is unreachable")
	}
}

// TestToLinesOmitsTheProfileLabelEntirelyWhenThereIsNone.
//
// This is the whole safety property of adding the label. Emitting
// profile="" would be a *different* series identity from the one an
// unlabelled install already holds, so the old samples would not be
// overwritten and a query summing the metric would read both: every
// historical dollar counted twice, which reads as real. Only an absent label
// keeps a re-run idempotent.
func TestToLinesOmitsTheProfileLabelEntirelyWhenThereIsNone(t *testing.T) {
	cost := map[costKey]float64{{Model: "m", Project: "p", Workspace: "w", Sidechain: "false", TSMs: 1000}: 1.5}
	daily := map[dailyKey]float64{{Day: "2026-01-01", Model: "m", Project: "p", Workspace: "w", Sidechain: "false"}: 1.5}

	for _, line := range toLines(cost, nil, daily, nil) {
		if strings.Contains(line, "profile") {
			t.Errorf("unlabelled backfill emitted a profile label, which forks the series identity:\n%s", line)
		}
	}
}

// TestToLinesAddsTheProfileLabelWhenThereIsOne, and only to the cost series:
// the token series carry no project or workspace label either, so nothing
// about them needs a profile.
func TestToLinesAddsTheProfileLabelWhenThereIsOne(t *testing.T) {
	cost := map[costKey]float64{{Model: "m", Project: "p", Workspace: "w", Sidechain: "false", Profile: "acme", TSMs: 1000}: 1.5}
	tokens := map[tokenKey]int64{{Model: "m", Type: "input", TSMs: 1000}: 10}

	var sawCost, sawTokens bool
	for _, line := range toLines(cost, tokens, nil, nil) {
		switch {
		case strings.HasPrefix(line, "claude_hist_cost_usd"):
			sawCost = true
			if !strings.Contains(line, `profile="acme"`) {
				t.Errorf("cost line missing the profile label:\n%s", line)
			}
		case strings.HasPrefix(line, "claude_hist_tokens"):
			sawTokens = true
			if strings.Contains(line, "profile") {
				t.Errorf("token line gained a profile label it does not need:\n%s", line)
			}
		}
	}
	if !sawCost || !sawTokens {
		t.Fatal("expected both a cost and a token line")
	}
}
