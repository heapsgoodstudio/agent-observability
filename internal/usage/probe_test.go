package usage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A control_response with the SHAPE of a real one, captured from a live probe
// on 2026-09-21 with the numbers replaced.
//
// The shape is the whole point and an earlier, tidier fixture is what let a
// real bug through. rate_limits is heterogeneous: on the live response it
// carried 25 keys, of which 5 were objects, 17 null, 2 arrays and one a bool.
// A fixture holding only the two window objects passed happily against a
// parser that decoded the blob as map[string]struct{...}, which fails on the
// first array and takes the entire response down with it. Every one of those
// value kinds is represented below for that reason.
const signedIn = `{"type":"control_response","response":{"subtype":"success","request_id":"probe","response":{"session":{"total_cost_usd":0},"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":70,"resets_at":"2026-09-21T19:30:00.208280+00:00"},"seven_day":{"utilization":41,"resets_at":"2026-09-28T04:00:00.208305+00:00"},"seven_day_opus":null,"cinder_cove":null,"member_dashboard_available":false,"model_scoped":[{"display_name":"Fable","utilization":36,"resets_at":"2026-09-28T04:00:00.208378+00:00"}],"extra_usage":{"utilization":4.4242,"resets_at":null},"spend":{"utilization":null},"limits":[{"kind":"session","group":"session","percent":70,"resets_at":"2026-09-21T19:30:00.208280+00:00","severity":"normal","is_active":true,"scope":null},{"kind":"weekly_all","group":"weekly","percent":41,"resets_at":"2026-09-28T04:00:00.208305+00:00","severity":"normal","is_active":false,"scope":null},{"kind":"weekly_scoped","group":"weekly","percent":36,"resets_at":"2026-09-28T04:00:00.208378+00:00","severity":"normal","is_active":false,"scope":{"model":{"display_name":"Fable","id":null}}}]}}}}`

// The same call against a profile whose sign-in has lapsed. This is a
// SUCCESSFUL probe with a real answer, not a failure.
const signedOut = `{"type":"control_response","response":{"subtype":"success","request_id":"probe","response":{"session":{"total_cost_usd":0},"subscription_type":null,"rate_limits_available":false,"rate_limits":null}}}`

func TestParseProbeReadsWindowsAndResets(t *testing.T) {
	res, ok := parseProbe([]byte(signedIn + "\n"))
	if !ok {
		t.Fatal("a real control_response did not parse")
	}
	if !res.Available || res.Subscription != "max" {
		t.Errorf("available=%v subscription=%q, want true/max", res.Available, res.Subscription)
	}
	// Three windows, from limits[], including weekly_scoped which the sibling
	// five_hour/seven_day keys do not carry.
	if len(res.Windows) != 3 {
		t.Errorf("windows = %v, want three from limits[]", res.Windows)
	}
	if got := res.Windows["weekly_scoped"].Pct; got != 36 {
		t.Errorf("weekly_scoped = %v, want 36", got)
	}
	// extra_usage and spend are objects in rate_limits but are NOT plan
	// windows, and a parser reading every object would report them as two
	// more windows with no reset time.
	for _, name := range []string{"extra_usage", "spend", "model_scoped", "limits"} {
		if _, ok := res.Windows[name]; ok {
			t.Errorf("%q was reported as a plan window", name)
		}
	}
	if got := res.Windows["five_hour"].Pct; got != 70 {
		t.Errorf("five_hour = %v, want 70", got)
	}
	if got := res.Windows["seven_day"].Pct; got != 41 {
		t.Errorf("seven_day = %v, want 41", got)
	}
	// The reset arrives as RFC 3339 and leaves as epoch seconds, which is the
	// whole point: the caller this replaces parsed "Resets Sep 14 at 5am
	// (Europe/Warsaw)" with a regex, a month-name table and its own year
	// inference.
	w := res.Windows["five_hour"]
	if !w.HasReset {
		t.Fatal("five_hour reset did not parse")
	}
	if got := time.Unix(w.ResetTS, 0).UTC().Format(time.RFC3339); got != "2026-09-21T19:30:00Z" {
		t.Errorf("five_hour reset = %s, want 2026-09-21T19:30:00Z", got)
	}
}

// A signed-out profile must come back ok with Available false. Reporting it as
// a parse failure would make the caller retry a probe that can never succeed,
// and reporting it as zero usage would make it look like the emptiest account
// on the machine, which is exactly where a router would send work.
func TestParseProbeTreatsSignedOutAsAnAnswer(t *testing.T) {
	res, ok := parseProbe([]byte(signedOut + "\n"))
	if !ok {
		t.Fatal("a signed-out response read as a parse failure")
	}
	if res.Available {
		t.Error("a signed-out profile reported usage as available")
	}
	if len(res.Windows) != 0 {
		t.Errorf("windows = %v, want none", res.Windows)
	}
}

// The stream carries other lines before the control response, and nothing
// says it comes first.
func TestParseProbeFindsTheResponseAmongOtherLines(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"x"}`,
		`{"type":"assistant","message":{"content":[]}}`,
		signedIn,
	}, "\n")
	if _, ok := parseProbe([]byte(stream)); !ok {
		t.Error("the control response was not found after other lines")
	}
}

// No control response at all is the one real failure: the probe ran and said
// nothing usable.
func TestParseProbeFailsWhenNoResponseArrives(t *testing.T) {
	for _, in := range []string{"", "not json\n", `{"type":"system"}` + "\n"} {
		if _, ok := parseProbe([]byte(in)); ok {
			t.Errorf("parsed a result from %q", in)
		}
	}
}

// Rounding rather than truncating, matching bin/limits-poller.sh. The same
// reset instant comes back either side of the whole second between refetches,
// and truncating made the epoch alternate by one, which the alert path's dedup
// read as a fresh reset period and re-fired every crossed threshold on.
func TestParseResetsAtRoundsTheSubSecond(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"2026-09-21T02:59:59.574463+00:00", "2026-09-21T03:00:00Z"},
		{"2026-09-21T03:00:00.410182+00:00", "2026-09-21T03:00:00Z"},
		{"2026-09-21T02:59:59.410182+00:00", "2026-09-21T02:59:59Z"},
	}
	for _, c := range cases {
		got, ok := parseResetsAt(c.in)
		if !ok {
			t.Errorf("%s did not parse", c.in)
			continue
		}
		if s := time.Unix(got, 0).UTC().Format(time.RFC3339); s != c.want {
			t.Errorf("parseResetsAt(%s) = %s, want %s", c.in, s, c.want)
		}
	}
	// Absent and unparseable both report not-known rather than epoch 0, which
	// would be 1970 and would read as a window that reset long ago.
	for _, in := range []string{"", "Resets Sep 14 at 5am (Europe/Warsaw)"} {
		if _, ok := parseResetsAt(in); ok {
			t.Errorf("parseResetsAt(%q) claimed a timestamp", in)
		}
	}
}

// The default profile is the one case where CLAUDE_CONFIG_DIR must be absent
// rather than set. Stripping matters even when the parent process has none of
// its own: launchd hands the server whatever it inherited, and an
// orchestrator's shell may well export one.
func TestSpawnEnvUnsetsConfigDirForTheDefaultProfile(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	base := []string{"PATH=/usr/bin", "CLAUDE_CONFIG_DIR=/somewhere/else"}

	def := spawnEnv(base, filepath.Join(home, ".claude"))
	for _, kv := range def {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			t.Fatalf("the default profile kept %q", kv)
		}
	}

	// The control: a named profile must SET it, or the probe reads the
	// default profile's config while claiming to be about another one.
	named := spawnEnv(base, "/Users/x/Claude/.claude-acme")
	found := ""
	for _, kv := range named {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			found = kv
		}
	}
	if found != "CLAUDE_CONFIG_DIR=/Users/x/Claude/.claude-acme" {
		t.Errorf("named profile env = %q", found)
	}
	// And exactly one: appending without stripping would leave the inherited
	// value behind it, and which one wins is not something to rely on.
	n := 0
	for _, kv := range named {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("CLAUDE_CONFIG_DIR appears %d times, want 1", n)
	}
}

// The single flight is not an optimisation. A launch wave asks about every
// profile at once while the poller probes on its own cadence, and without it
// one profile can have several probes in the air, each spawning a process and
// each racing to rewrite the same config file.
func TestProbeRunsOnceForConcurrentCallers(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude")
	counter := filepath.Join(dir, "runs")
	// Appends a line per invocation, then answers like a real probe. The
	// sleep is what gives the callers time to pile up behind one flight.
	body := "#!/bin/bash\ncat >/dev/null\necho run >> " + counter + "\nsleep 0.4\ncat <<'EOF'\n" + signedIn + "\nEOF\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	p := &Prober{ClaudeBin: script, Timeout: 10 * time.Second}
	var wg sync.WaitGroup
	results := make([]Result, 8)
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = p.Probe(context.Background(), "/Users/x/Claude/.claude-acme")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if results[i].Windows["five_hour"].Pct != 70 {
			t.Errorf("caller %d got %v, want every caller to share the one answer", i, results[i].Windows)
		}
	}
	runs, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(runs), "run"); n != 1 {
		t.Errorf("the binary ran %d times for 8 concurrent callers, want 1", n)
	}
}

// A later caller must get a fresh probe rather than a cached one: the flight
// is deduplication of concurrent work, not a cache, and serving an old answer
// to a caller that asked after the first finished would be the lag this whole
// endpoint exists to remove.
func TestProbeRunsAgainForALaterCaller(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude")
	counter := filepath.Join(dir, "runs")
	body := "#!/bin/bash\ncat >/dev/null\necho run >> " + counter + "\ncat <<'EOF'\n" + signedIn + "\nEOF\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Prober{ClaudeBin: script, Timeout: 10 * time.Second}
	for i := 0; i < 3; i++ {
		if _, err := p.Probe(context.Background(), "/Users/x/Claude/.claude-acme"); err != nil {
			t.Fatal(err)
		}
	}
	runs, _ := os.ReadFile(counter)
	if n := strings.Count(string(runs), "run"); n != 3 {
		t.Errorf("sequential calls ran the binary %d times, want 3", n)
	}
}

// A probe that cannot run at all is an error, not a Result{} that would read
// as a profile with no usage.
func TestProbeErrorsRatherThanReturningAnEmptyResult(t *testing.T) {
	p := &Prober{ClaudeBin: "/nonexistent/claude", Timeout: 2 * time.Second}
	if _, err := p.Probe(context.Background(), "/Users/x/Claude/.claude-acme"); err == nil {
		t.Error("a missing binary produced no error")
	}
	empty := &Prober{}
	if _, err := empty.Probe(context.Background(), "/x"); err == nil {
		t.Error("an unconfigured prober produced no error")
	}
}
