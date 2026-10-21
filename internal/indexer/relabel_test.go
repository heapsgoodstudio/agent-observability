package indexer

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSeriesToPromLinesAddsTheLabelAndKeepsEverythingElse is the heart of the
// relabel: the samples must come back out carrying exactly what they went in
// with, plus one label. Anything dropped here is history that cannot be
// recovered, since the transcripts behind it have aged out.
func TestSeriesToPromLinesAddsTheLabelAndKeepsEverythingElse(t *testing.T) {
	s := exported{
		Metric: map[string]string{
			"__name__":  "claude_hist_daily_cost_usd",
			"model":     "claude-opus-5",
			"project":   "-Users-x-proj",
			"workspace": "owner/repo",
			"sidechain": "false",
		},
		Values:     []float64{12.5, 0.25},
		Timestamps: []int64{1000, 2000},
	}

	lines := seriesToPromLines(s, "profile", "acme")
	if len(lines) != 2 {
		t.Fatalf("want one line per sample, got %d: %v", len(lines), lines)
	}
	for _, want := range []string{
		`model="claude-opus-5"`, `project="-Users-x-proj"`,
		`workspace="owner/repo"`, `sidechain="false"`, `profile="acme"`,
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line missing %s:\n%s", want, lines[0])
		}
	}
	if !strings.HasSuffix(lines[0], " 1000") || !strings.HasSuffix(lines[1], " 2000") {
		t.Errorf("timestamps not preserved:\n%s\n%s", lines[0], lines[1])
	}
	if !strings.Contains(lines[0], "12.5") {
		t.Errorf("value not preserved:\n%s", lines[0])
	}
}

// TestSeriesToPromLinesReplacesAnExistingLabel keeps a repeated run
// idempotent rather than producing two profile= labels on one series, which
// is not a valid line at all.
func TestSeriesToPromLinesReplacesAnExistingLabel(t *testing.T) {
	s := exported{
		Metric:     map[string]string{"__name__": "m", "profile": "old"},
		Values:     []float64{1},
		Timestamps: []int64{1},
	}
	line := seriesToPromLines(s, "profile", "new")[0]
	if strings.Count(line, "profile=") != 1 {
		t.Fatalf("want exactly one profile label, got: %s", line)
	}
	if !strings.Contains(line, `profile="new"`) {
		t.Errorf("want the new value, got: %s", line)
	}
}

// TestFormatValueNeverUsesExponentNotation: the Prometheus text format does
// not accept it, so a very small or very large cost would be rejected at
// import and silently lost from the re-import.
func TestFormatValueNeverUsesExponentNotation(t *testing.T) {
	for _, v := range []float64{0.0000001, 123456789.5, 1e-9, 0} {
		if got := formatValue(v); strings.ContainsAny(got, "eE") {
			t.Errorf("formatValue(%v) = %q, which VictoriaMetrics will reject", v, got)
		}
	}
}

// TestResetSafetyRefusesWhenHistoryOutlivesItsTranscripts is the guard's
// reason for existing, in the exact shape measured on a real install: cost
// history from 2026-07-16 to 2026-08-17, oldest surviving transcript
// 2026-08-19, no overlap at all.
func TestResetSafetyRefusesWhenHistoryOutlivesItsTranscripts(t *testing.T) {
	histStart := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC).Unix()
	oldest := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC).Unix()
	r := ResetSafety{
		HistoryStart:     histStart,
		OldestTranscript: oldest,
		Unreparseable:    time.Duration(oldest-histStart) * time.Second,
		AtRisk:           12345.67,
	}
	if r.Safe() {
		t.Fatal("must not call this safe: the reset would delete a month it cannot rebuild")
	}
	msg := r.Explain()
	for _, want := range []string{"2026-07-16", "2026-08-19", "12345.67", "migrate-history-labels"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, msg)
		}
	}
}

// TestResetSafetyAllowsWhenTranscriptsCoverTheHistory: the flag stays usable
// for the case it was written for, a label change on a young series.
func TestResetSafetyAllowsWhenTranscriptsCoverTheHistory(t *testing.T) {
	now := time.Now().Unix()
	r := ResetSafety{HistoryStart: now - 3600, OldestTranscript: now - 86400}
	if !r.Safe() {
		t.Error("history that transcripts fully cover must stay resettable")
	}
	if r.Explain() != "" {
		t.Error("a safe reset must explain nothing")
	}
}

// TestQueryTotalDisablesTheQueryCache.
//
// VictoriaMetrics caches instant-query results by query string, and every
// caller of queryTotal polls one query string repeatedly while an import is
// still being ingested. Without nocache the first read lands mid-ingestion
// and every later read returns that same partial figure, unchanged, for as
// long as you poll: indistinguishable from a settled total that is short.
//
// Measured 2026-09-21 on a 147 series import: the cached query held a total
// 4.6% short indefinitely while the same query with nocache=1 returned the
// dump's own sum to the cent. That false shortfall is reported to an operator
// as "your history did not survive", which is the worst possible thing for it
// to be wrong about.
func TestQueryTotalDisablesTheQueryCache(t *testing.T) {
	var gotNocache string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotNocache = r.URL.Query().Get("nocache")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"result":[{"value":[1,"1.5"]}]}}`)
	}))
	defer srv.Close()

	if _, err := queryTotal(srv.URL, "sum(x)"); err != nil {
		t.Fatalf("queryTotal: %v", err)
	}
	if gotNocache != "1" {
		t.Errorf("nocache=%q, want \"1\"; a cached poll loop cannot tell a partial import from a lost one", gotNocache)
	}
}
