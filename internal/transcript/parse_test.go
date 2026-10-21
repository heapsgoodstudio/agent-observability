package transcript

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// fixturePath resolves internal/testdata/fixture-session.jsonl relative to
// this file rather than the working directory, so `go test ./...` from any
// directory finds it.
func fixturePath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve caller for fixture path")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixture-session.jsonl")
}

// mustParseFixture is the shared setup every parser case below uses: parse
// the fixture, asserted non-nil.
func mustParseFixture(t *testing.T) *Summary {
	t.Helper()
	s, err := Parse(fixturePath(t), nil)
	if err != nil {
		t.Fatalf("Parse(fixture) error: %v", err)
	}
	if s == nil {
		t.Fatal("Parse(fixture) = nil, want a Summary")
	}
	return s
}

func almostEqual(t *testing.T, name string, got, want float64) {
	t.Helper()
	const eps = 1e-9
	if math.Abs(got-want) > eps {
		t.Errorf("%s = %.9f, want %.9f", name, got, want)
	}
}

// test_dedup_and_totals
func TestDedupAndTotals(t *testing.T) {
	s := mustParseFixture(t)
	if s.Requests != 3 {
		t.Errorf("Requests = %d, want 3", s.Requests)
	}
	almostEqual(t, "CostUSD", s.CostUSD, 0.002695)
	almostEqual(t, "SidechainCostUSD", s.SidechainCostUSD, 0.00011)
}

// test_tokens
func TestTokens(t *testing.T) {
	s := mustParseFixture(t)
	want := Tokens{Input: 115, Output: 225, CacheRead: 1000, CacheCreation: 50}
	if s.Tokens != want {
		t.Errorf("Tokens = %+v, want %+v", s.Tokens, want)
	}
}

// test_models_normalized
func TestModelsNormalized(t *testing.T) {
	s := mustParseFixture(t)
	byModel := map[string]float64{}
	for _, m := range s.Models {
		byModel[m.Model] = m.CostUSD
	}
	names := make([]string, 0, len(byModel))
	for m := range byModel {
		names = append(names, m)
	}
	sort.Strings(names)
	if diff := cmp.Diff([]string{"claude-haiku-4-5", "claude-sonnet-5"}, names); diff != "" {
		t.Errorf("model set diff:\n%s", diff)
	}
	almostEqual(t, "sonnet-5 cost", byModel["claude-sonnet-5"], 0.002585)
	almostEqual(t, "haiku-4-5 cost", byModel["claude-haiku-4-5"], 0.00011)

	// The interface contract calls out cost-descending order explicitly, and
	// the fixture's costs are unequal, so this order is exercised for free
	// here.
	if len(s.Models) != 2 || s.Models[0].Model != "claude-sonnet-5" || s.Models[1].Model != "claude-haiku-4-5" {
		t.Errorf("Models = %+v, want [sonnet-5, haiku-4-5] cost-descending", s.Models)
	}
}

// Models orders by cost descending, ties broken by first-appearance order
// among requests sorted by timestamp. The fixture has no tied costs, so this
// is covered separately with
// a synthetic transcript: two distinct models each costing exactly 0.0002,
// the cheaper-priced model first by timestamp.
func TestModelsTieBreakByInsertionOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tie.jsonl")
	lines := `{"type":"assistant","isSidechain":false,"requestId":"req_a","message":{"id":"msg_a","model":"claude-sonnet-5-20250101","role":"assistant","content":[],"usage":{"input_tokens":100,"output_tokens":0}},"timestamp":"2026-01-01T00:00:00.000Z"}
{"type":"assistant","isSidechain":false,"requestId":"req_b","message":{"id":"msg_b","model":"claude-haiku-4-5-20250101","role":"assistant","content":[],"usage":{"input_tokens":200,"output_tokens":0}},"timestamp":"2026-01-01T00:00:01.000Z"}
`
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Parse(path, nil)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if s == nil {
		t.Fatal("Parse = nil, want a Summary")
	}
	want := []ModelCost{
		{Model: "claude-sonnet-5", CostUSD: 0.0002},
		{Model: "claude-haiku-4-5", CostUSD: 0.0002},
	}
	if diff := cmp.Diff(want, s.Models); diff != "" {
		t.Errorf("Models diff:\n%s", diff)
	}
}

// test_behaviour
func TestBehaviour(t *testing.T) {
	s := mustParseFixture(t)
	wantTools := map[string]int{
		"Bash": 1, "Read": 1, "Skill": 1,
		"mcp__plugin_linear_linear__get_issue": 1,
	}
	if diff := cmp.Diff(wantTools, s.Tools); diff != "" {
		t.Errorf("Tools diff:\n%s", diff)
	}
	if s.ToolCalls != 4 {
		t.Errorf("ToolCalls = %d, want 4", s.ToolCalls)
	}
	if s.UserTurns != 2 {
		t.Errorf("UserTurns = %d, want 2", s.UserTurns)
	}
	if diff := cmp.Diff([]string{"usage-report"}, s.Skills); diff != "" {
		t.Errorf("Skills diff:\n%s", diff)
	}
}

// test_mcp_servers_derived_from_tool_names
func TestMCPServersDerivedFromToolNames(t *testing.T) {
	s := mustParseFixture(t)
	if diff := cmp.Diff([]string{"plugin_linear_linear"}, s.MCPServers); diff != "" {
		t.Errorf("MCPServers diff:\n%s", diff)
	}
}

// test_mcp_server_name_normalisation
func TestMCPServerOf(t *testing.T) {
	if got, ok := MCPServerOf("mcp__plugin_linear_linear__get_issue"); !ok || got != "plugin_linear_linear" {
		t.Errorf("MCPServerOf(plugin_linear_linear tool) = (%q, %v), want (%q, true)", got, ok, "plugin_linear_linear")
	}
	if got, ok := MCPServerOf("mcp__claude_ai_Google_Drive__authenticate"); !ok || got != "claude_ai_google_drive" {
		t.Errorf("MCPServerOf(Google Drive tool) = (%q, %v), want (%q, true)", got, ok, "claude_ai_google_drive")
	}
	if _, ok := MCPServerOf("Bash"); ok {
		t.Error("MCPServerOf(Bash) should be (_, false)")
	}
	if _, ok := MCPServerOf("mcp__nope"); ok {
		t.Error("MCPServerOf(mcp__nope) should be (_, false)")
	}
}

// test_span
func TestSpan(t *testing.T) {
	s := mustParseFixture(t)
	wantStart, err := iso2epoch("2026-08-01T10:00:05.000Z")
	if err != nil {
		t.Fatal(err)
	}
	wantEnd, err := iso2epoch("2026-08-01T10:01:00.000Z")
	if err != nil {
		t.Fatal(err)
	}
	if s.Start != wantStart {
		t.Errorf("Start = %d, want %d", s.Start, wantStart)
	}
	if s.End != wantEnd {
		t.Errorf("End = %d, want %d", s.End, wantEnd)
	}
}

// test_trajectory
func TestTrajectory(t *testing.T) {
	s := mustParseFixture(t)
	if len(s.Trajectory) != 3 {
		t.Fatalf("len(Trajectory) = %d, want 3", len(s.Trajectory))
	}
	wantFirstEpoch, err := iso2epoch("2026-08-01T10:00:05.000Z")
	if err != nil {
		t.Fatal(err)
	}
	first := s.Trajectory[0]
	if int64(first[0]) != wantFirstEpoch {
		t.Errorf("Trajectory[0][0] = %v, want %d", first[0], wantFirstEpoch)
	}
	almostEqual(t, "Trajectory[0][1]", first[1], 0.002525)
	if first[2] != 1150 {
		t.Errorf("Trajectory[0][2] = %v, want 1150", first[2])
	}
	if s.Trajectory[1][2] != 0 {
		t.Errorf("subagent context must not be plotted as the main conversation: %v", s.Trajectory[1])
	}
	last := s.Trajectory[2]
	almostEqual(t, "Trajectory[2][1]", last[1], 0.002695)
	if last[2] != 5 {
		t.Errorf("Trajectory[2][2] = %v, want 5", last[2])
	}
}

// test_downsample
func TestDownsample(t *testing.T) {
	pts := make([][3]float64, 1000)
	for i := range pts {
		pts[i] = [3]float64{float64(i), float64(i), float64(i)}
	}
	out := Downsample(pts, 300)
	if len(out) != 300 {
		t.Fatalf("len(out) = %d, want 300", len(out))
	}
	if out[0] != [3]float64{0, 0, 0} {
		t.Errorf("out[0] = %v, want [0 0 0]", out[0])
	}
	if out[len(out)-1] != [3]float64{999, 999, 999} {
		t.Errorf("out[-1] = %v, want [999 999 999]", out[len(out)-1])
	}

	noop := Downsample(pts[:5], 300)
	if diff := cmp.Diff(pts[:5], noop); diff != "" {
		t.Errorf("no-op downsample diff:\n%s", diff)
	}
}

// round6 must round half to even at 6 decimal places, not just at exact
// ties but near rounding boundaries in general: a naive
// scale-then-math.Round-then-divide implementation was tried first and was
// wrong on a meaningful fraction of realistic cost values, because the scale
// and divide steps are each their own lossy binary floating point operation.
// Every want value below was checked against an independent
// correctly-rounded decimal implementation rather than hand-computed, since
// a literal like 0.1234575 is not exactly representable in float64 and can
// resolve either side of an apparent .5 boundary depending on where the
// actual stored value falls.
func TestRound6HalfToEven(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{"half a millionth rounds down to zero", 0.0000005, 0},
		{"one and a half millionths rounds up", 0.0000015, 0.000002},
		{"banker's rounding: half rounds to the even digit below (6 stays 6)", 0.1234565, 0.123456},
		// Not 0.123458: 0.1234575 is not exactly representable in float64,
		// and the nearest double falls just under the true halfway point, so
		// round6 rounds it down, not up.
		{"apparent half that is not actually a tie in float64", 0.1234575, 0.123457},
		{"formula-derived value: (137/1e6)*1.05", (137.0 / 1e6) * 1.05, 0.000144},
		{"sonnet-5 golden request cost stays exact", 0.002525, 0.002525},
		{"haiku-4-5 golden request cost stays exact", 0.00011, 0.00011},
		{"sonnet-5 second golden request cost stays exact", 0.00006, 0.00006},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := round6(tc.in); got != tc.want {
				t.Errorf("round6(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Parse must return (nil, nil) for a transcript with no billable requests,
// as its interface contract states. The empty fixture below is a synthetic
// one-liner rather than a testdata fixture, since none exists for this
// shape.
func TestParseNoBillableRequestsReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Parse(path, nil)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if s != nil {
		t.Errorf("Parse(no billable requests) = %+v, want nil", s)
	}
}

// Cwds ranks by how many records carry each directory, which is the whole
// point: the launch directory is often the least-used one, so a ranking that
// followed first appearance would name every session after where it started.
func TestScanRanksCwdsByFrequency(t *testing.T) {
	lines := []string{
		`{"type":"user","cwd":"/w/acme"}`,
		`{"type":"assistant","cwd":"/w/acme/api"}`,
		`{"type":"summary","cwd":"/w/acme/api"}`,
		`{"type":"user","cwd":"/w/acme/api"}`,
		`{"type":"user","cwd":"/w/acme/terraform"}`,
		`{"type":"user","cwd":"/w/acme/terraform"}`,
		`{"type":"user"}`,
	}
	sc := mustScanLines(t, lines)
	want := []string{"/w/acme/api", "/w/acme/terraform", "/w/acme"}
	if diff := cmp.Diff(want, sc.Cwds); diff != "" {
		t.Errorf("Cwds mismatch (-want +got):\n%s", diff)
	}
}

// Equal counts break on the path, so two scans of an untouched transcript
// agree; ranking on map iteration order alone would let the name flip.
func TestScanCwdTiesBreakLexicographically(t *testing.T) {
	lines := []string{
		`{"type":"user","cwd":"/w/b"}`,
		`{"type":"user","cwd":"/w/a"}`,
		`{"type":"user","cwd":"/w/c"}`,
	}
	sc := mustScanLines(t, lines)
	want := []string{"/w/a", "/w/b", "/w/c"}
	if diff := cmp.Diff(want, sc.Cwds); diff != "" {
		t.Errorf("Cwds mismatch (-want +got):\n%s", diff)
	}
}

// mustScanLines writes lines to a temporary transcript and scans it. Scan
// rather than Parse, because these cases carry no billable request and Parse
// returns nil for those.
func mustScanLines(t *testing.T, lines []string) *Scanned {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cwds.jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sc, err := Scan(path, nil)
	if err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	return sc
}

// Cache refreshes must not look like compaction. Genuine reductions stay,
// while independent subagent prompts never become main-context samples.
func TestContextCacheRefreshCompactionAndSubagents(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "main.jsonl")
	nested := filepath.Join(dir, "agent.jsonl")
	lines := `{"type":"assistant","timestamp":"2026-09-01T10:00:00Z","message":{"id":"main1","model":"claude-sonnet-5","usage":{"input_tokens":20,"cache_read_input_tokens":100000,"cache_creation_input_tokens":500}}}
{"type":"assistant","timestamp":"2026-09-01T10:01:00Z","message":{"id":"main2","model":"claude-sonnet-5","usage":{"input_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":101000,"cache_creation":{"ephemeral_5m_input_tokens":61000,"ephemeral_1h_input_tokens":40000}}}}
{"type":"assistant","timestamp":"2026-09-01T10:03:00Z","message":{"id":"main3","model":"claude-sonnet-5","usage":{"input_tokens":20,"cache_creation_input_tokens":20000}}}
{"type":"assistant","isSidechain":true,"timestamp":"2026-09-01T10:04:00Z","message":{"id":"inline-agent","model":"claude-sonnet-5","usage":{"input_tokens":500}}}
`
	// A copied main request remains main; a new nested request is sidechain
	// even when it has no isSidechain flag.
	agent := `{"type":"assistant","timestamp":"2026-09-01T10:00:00Z","message":{"id":"main1","model":"claude-sonnet-5","usage":{"input_tokens":20,"cache_read_input_tokens":100000,"cache_creation_input_tokens":500}}}
{"type":"assistant","timestamp":"2026-09-01T10:02:00Z","message":{"id":"agent","model":"claude-sonnet-5","usage":{"input_tokens":10,"cache_creation_input_tokens":9000}}}
`
	if err := os.WriteFile(primary, []byte(lines), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte(agent), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Parse(primary, []string{nested})
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{100520, 101020, 0, 20020, 0}
	if len(s.Trajectory) != len(want) {
		t.Fatalf("trajectory: %+v", s.Trajectory)
	}
	for i, p := range s.Trajectory {
		if p[2] != want[i] {
			t.Errorf("context[%d] = %v, want %v", i, p[2], want[i])
		}
	}
	if s.Requests != 5 || s.Tokens.Input != 570 || s.Tokens.CacheCreation != 130500 || s.SidechainCostUSD <= 0 {
		t.Fatalf("subagent usage must remain in session totals: %+v", s)
	}
	if s.Trajectory[len(want)-1][1] != s.CostUSD {
		t.Fatal("cost trajectory lost subagent charges")
	}
}
