package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1/claudeobsv1connect"
	"github.com/heapsgoodstudio/agent-observability/internal/analysis"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vl"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// fakeVLHandler serves a canned NDJSON body for every request, or 500s when
// fail is set.
type fakeVLHandler struct {
	body string
	fail bool
}

func (h fakeVLHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.fail {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write([]byte(h.body))
}

// idleRunner is a Runner that reports Busy() == false without ever spawning
// anything, so tests that don't care about the runner's own state don't have
// it drag Healthy to false (runnerCheck reports "bad", the only state that
// counts against Healthy, for a nil Runner only).
func idleRunner(t *testing.T) *analysis.Runner {
	t.Helper()
	return analysis.New(analysis.Config{})
}

// busyRunnerFor starts one real (fake-`claude`-backed) analyse run against
// sid and returns the Runner with it genuinely in flight, so tests can
// exercise runnerCheck's warn path against real Busy() state instead of a
// fabricated bool. The fake script sleeps long enough to outlive the check;
// t.Cleanup waits for it to finish so no goroutine leaks past the test.
func busyRunnerFor(t *testing.T, sid string) *analysis.Runner {
	t.Helper()
	tmp := t.TempDir()
	profileDir := filepath.Join(tmp, "profile")
	proj := filepath.Join(profileDir, "projects", "-t-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, sid+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(tmp, "data")
	sessionsDir := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, sid+".json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(tmp, "fake-claude.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\necho done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := analysis.New(analysis.Config{
		ClaudeBin: script, ProfileDir: profileDir, DataDir: dataDir,
		VLURL:           "http://127.0.0.1:1",
		AnalysisTimeout: 30 * time.Second, EvidenceTimeout: 30 * time.Second,
	})
	if err := r.StartAnalysis(sid, "sonnet"); err != nil {
		t.Fatalf("StartAnalysis: %v", err)
	}
	t.Cleanup(r.Wait)
	return r
}

func healthyPollerInstant(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		pollerQuery: successBody(t, vmResult{Metric: map[string]string{"source": "cache"}, Value: []any{testNow, strconv.FormatInt(testNow-10, 10)}}),
		"count(count_over_time(claude_in_overage[1h]))":      successBody(t, vmResult{Metric: map[string]string{}, Value: []any{0, "1"}}),
		"count(count_over_time(claude_extra_usage_usd[1h]))": successBody(t, vmResult{Metric: map[string]string{}, Value: []any{0, "1"}}),
		"count(count_over_time(claude_plan_window_pct[1h]))": successBody(t, vmResult{Metric: map[string]string{}, Value: []any{0, "1"}}),
	}
}

func healthOptions(vmSrv, vlSrv *httptest.Server, dataDir, launchAgentsDir string, runner *analysis.Runner) Options {
	o := Options{
		Memo:            store.New(store.TTL),
		DataDir:         dataDir,
		LaunchAgentsDir: launchAgentsDir,
		Runner:          runner,
	}
	if vmSrv != nil {
		o.VM = &vm.Client{BaseURL: vmSrv.URL, HTTP: vmSrv.Client()}
	}
	if vlSrv != nil {
		o.VL = &vl.Client{BaseURL: vlSrv.URL, HTTP: vlSrv.Client()}
	}
	return o
}

func healthSource(profile, account, source string, at int64) vmResult {
	return vmResult{Metric: map[string]string{"profile": profile, "account": account, "source": source}, Value: []any{testNow, strconv.FormatInt(at, 10)}}
}

func profileHealthFixture(t *testing.T, fixture Options, instant map[string]string, fail string, now int64) []*claudeobsv1.HealthCheck {
	t.Helper()
	vmSrv := httptest.NewServer(fakeVMHandler{instant: instant, fail: fail})
	t.Cleanup(vmSrv.Close)
	vlSrv := httptest.NewServer(fakeVLHandler{})
	t.Cleanup(vlSrv.Close)
	o := healthOptions(vmSrv, vlSrv, t.TempDir(), t.TempDir(), idleRunner(t))
	o.Profiles, o.DB = fixture.Profiles, fixture.DB
	return buildHealth(context.Background(), o, now).Checks
}

func requirePlanCheck(t *testing.T, checks []*claudeobsv1.HealthCheck, profile profiles.Profile, source, state string) *claudeobsv1.HealthCheck {
	t.Helper()
	name := "plan data, " + source + " · " + profile.Name + " (" + profile.ID + ")"
	for _, check := range checks {
		if check.Name == name {
			if check.State != state {
				t.Fatalf("%s: got %s (%s), want %s", name, check.State, check.Detail, state)
			}
			return check
		}
	}
	t.Fatalf("missing %s in %v", name, checks)
	return nil
}

func TestHealthFreshCacheOnlyCoversItsOwnEnabledProfileAndAccount(t *testing.T) {
	fixture := settingsFixture(t)
	fresh := regressionProfile(t, fixture, profiles.Claude, "Fresh")
	stale := regressionProfile(t, fixture, profiles.Claude, "Stale")
	moved := regressionProfile(t, fixture, profiles.Claude, "Moved")
	missing := regressionProfile(t, fixture, profiles.Claude, "Missing")
	disabled := regressionProfile(t, fixture, profiles.Claude, "Disabled")
	disabled.Enabled = false
	if _, err := fixture.Profiles.Save(disabled); err != nil {
		t.Fatal(err)
	}
	instant := healthyPollerInstant(t)
	instant[healthAccountQuery] = successBody(t,
		healthSource(fresh.ID, "shared", "", testNow), healthSource(stale.ID, "shared", "", testNow),
		healthSource(moved.ID, "old", "", testNow-30), healthSource(moved.ID, "new", "", testNow),
		healthSource(missing.ID, "missing", "", testNow), healthSource(disabled.ID, "shared", "", testNow))
	var observed []vmResult
	for _, p := range []profiles.Profile{fresh, stale, disabled} {
		observed = append(observed, healthSource(p.ID, "shared", "cache", testNow-3600), healthSource(p.ID, "shared", "statusline", testNow-12000))
	}
	observed = append(observed, healthSource(moved.ID, "new", "cache", testNow-3600), healthSource(moved.ID, "new", "statusline", testNow-12000), healthSource(moved.ID, "old", "cache", testNow-5))
	instant[pollerQuery] = successBody(t, observed...)
	instant[confirmedQuery] = successBody(t, healthSource(fresh.ID, "shared", "cache", testNow-10), healthSource(stale.ID, "shared", "cache", testNow-900), healthSource(moved.ID, "old", "cache", testNow-5))
	checks := profileHealthFixture(t, fixture, instant, "", testNow)
	requirePlanCheck(t, checks, fresh, "cache", "ok")
	check := requirePlanCheck(t, checks, fresh, "statusline", "ok")
	if !strings.Contains(check.Detail, "Idle") || !strings.Contains(check.Detail, "confirmed 10s ago") {
		t.Fatalf("idle fallback not explained: %v", check)
	}
	for _, p := range []profiles.Profile{stale, moved} {
		requirePlanCheck(t, checks, p, "cache", "warn")
		requirePlanCheck(t, checks, p, "statusline", "warn")
	}
	requirePlanCheck(t, checks, missing, "cache", "unknown")
	for _, check := range checks {
		if strings.HasPrefix(check.Name, "plan data") && strings.Contains(check.Name, disabled.ID) {
			t.Fatalf("disabled profile produced a freshness warning: %v", check)
		}
	}
}

func TestHealthFreshnessAdvancesWhenPollerStops(t *testing.T) {
	fixture := settingsFixture(t)
	p := regressionProfile(t, fixture, profiles.Claude, "Stopped")
	instant := healthyPollerInstant(t)
	instant[healthAccountQuery] = successBody(t, healthSource(p.ID, "account", "", testNow))
	instant[pollerQuery] = successBody(t, healthSource(p.ID, "account", "cache", testNow-10), healthSource(p.ID, "account", "statusline", testNow-3600))
	instant[confirmedQuery] = successBody(t, healthSource(p.ID, "account", "cache", testNow-5))
	checks := profileHealthFixture(t, fixture, instant, "", testNow+600)
	requirePlanCheck(t, checks, p, "cache", "warn")
	requirePlanCheck(t, checks, p, "statusline", "warn")
}

func TestHealthExplicitUnavailableAllowanceIsProfileAndAccountScoped(t *testing.T) {
	fixture := settingsFixture(t)
	unavailable := regressionProfile(t, fixture, profiles.Claude, "Unavailable")
	available := regressionProfile(t, fixture, profiles.Claude, "Available")
	missing := regressionProfile(t, fixture, profiles.Claude, "Missing")
	instant := healthyPollerInstant(t)
	instant[healthAccountQuery] = successBody(t, healthSource(unavailable.ID, "shared", "", testNow), healthSource(available.ID, "shared", "", testNow), healthSource(missing.ID, "new-account", "", testNow))
	instant[pollerQuery] = successBody(t, healthSource(unavailable.ID, "shared", "cache", testNow-3600), healthSource(available.ID, "shared", "cache", testNow-5))
	instant[healthAvailabilityQuery] = successBody(t, healthSource(unavailable.ID, "shared", "", 0), healthSource(available.ID, "shared", "", 1), healthSource(missing.ID, "old-account", "", 0))
	checks := profileHealthFixture(t, fixture, instant, "", testNow)
	check := requirePlanCheck(t, checks, unavailable, "cache", "unknown")
	if !strings.Contains(check.Detail, "Provider does not report allowance") || !strings.Contains(check.Detail, "check Claude sign-in") {
		t.Fatalf("explicitly unavailable allowance left unexplained: %v", check)
	}
	requirePlanCheck(t, checks, available, "cache", "ok")
	check = requirePlanCheck(t, checks, missing, "cache", "unknown")
	if strings.Contains(check.Detail, "Provider does not report") {
		t.Fatalf("old account availability leaked into the current account: %v", check)
	}
}

func TestHealthMissingOrFailedSourcesCannotCoverIdleStatusline(t *testing.T) {
	for _, tc := range []struct {
		name, fail   string
		missingCache bool
	}{
		{name: "confirmation query failed", fail: confirmedQuery},
		{name: "cache missing", missingCache: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := settingsFixture(t)
			p := regressionProfile(t, fixture, profiles.Claude, "Work")
			instant := healthyPollerInstant(t)
			instant[healthAccountQuery] = successBody(t, healthSource(p.ID, "account", "", testNow))
			observed := []vmResult{healthSource(p.ID, "account", "statusline", testNow-3600)}
			if !tc.missingCache {
				observed = append(observed, healthSource(p.ID, "account", "cache", testNow-3600))
			}
			instant[pollerQuery] = successBody(t, observed...)
			instant[confirmedQuery] = successBody(t, healthSource(p.ID, "account", "cache", testNow-5))
			checks := profileHealthFixture(t, fixture, instant, tc.fail, testNow)
			requirePlanCheck(t, checks, p, "statusline", "warn")
			if tc.missingCache {
				requirePlanCheck(t, checks, p, "cache", "unknown")
			} else {
				found := false
				for _, check := range checks {
					if check.Name == "plan data, confirmations" && check.State == "unknown" {
						found = true
					}
				}
				if !found {
					t.Fatal("confirmation query failure was hidden")
				}
			}
		})
	}
}

func TestHealthUnknownCurrentIdentityRemainsVisible(t *testing.T) {
	for _, account := range []string{"", "unknown"} {
		fixture := settingsFixture(t)
		p := regressionProfile(t, fixture, profiles.Claude, "Unresolved")
		instant := healthyPollerInstant(t)
		instant[healthAccountQuery] = successBody(t, healthSource(p.ID, account, "", testNow))
		instant[pollerQuery] = successBody(t, healthSource(p.ID, account, "cache", testNow-5))
		checks := profileHealthFixture(t, fixture, instant, "", testNow)
		found := false
		for _, check := range checks {
			if strings.HasPrefix(check.Name, "plan data") && strings.Contains(check.Name, p.ID) {
				found = true
				if check.State != "unknown" || !strings.Contains(check.Detail, "identity") {
					t.Fatalf("unresolved account was treated as healthy: %v", check)
				}
			}
		}
		if !found {
			t.Fatal("unresolved enabled profile was omitted")
		}
	}
}

// Store reachability: a healthy VM and VL fake must report "ok".
func TestBuildHealthStoresReachable(t *testing.T) {
	vmSrv := httptest.NewServer(fakeVMHandler{instant: healthyPollerInstant(t)})
	defer vmSrv.Close()
	vlSrv := httptest.NewServer(fakeVLHandler{body: `{"event.name":"api_request","n":"5"}` + "\n"})
	defer vlSrv.Close()

	o := healthOptions(vmSrv, vlSrv, t.TempDir(), t.TempDir(), idleRunner(t))
	resp := buildHealth(context.Background(), o, 1755300000)

	states := map[string]string{}
	for _, c := range resp.Checks {
		states[c.Name] = c.State
	}
	if states["VictoriaMetrics"] != "ok" {
		t.Errorf("VictoriaMetrics state = %q, want ok", states["VictoriaMetrics"])
	}
	if states["VictoriaLogs"] != "ok" {
		t.Errorf("VictoriaLogs state = %q, want ok", states["VictoriaLogs"])
	}
	if len(resp.LogVolume_24H) != 1 || resp.LogVolume_24H[0].Key != "api_request" || resp.LogVolume_24H[0].Value != 5 {
		t.Errorf("LogVolume_24H = %+v, want one api_request row of 5", resp.LogVolume_24H)
	}
}

// A VM 500 must report VictoriaMetrics as bad.
func TestBuildHealthVMUnreachable(t *testing.T) {
	vmSrv := httptest.NewServer(fakeVMHandler{fail: pollerQuery})
	defer vmSrv.Close()
	vlSrv := httptest.NewServer(fakeVLHandler{body: ""})
	defer vlSrv.Close()

	o := healthOptions(vmSrv, vlSrv, t.TempDir(), t.TempDir(), idleRunner(t))
	resp := buildHealth(context.Background(), o, 1755300000)

	var vmState string
	for _, c := range resp.Checks {
		if c.Name == "VictoriaMetrics" {
			vmState = c.State
		}
	}
	if vmState != "bad" {
		t.Errorf("VictoriaMetrics state = %q, want bad", vmState)
	}
	if resp.Healthy {
		t.Error("Healthy = true, want false when VictoriaMetrics is unreachable")
	}
}

// The five expected series: three present, two absent, asserting warn on the
// absent ones and ok on the present ones.
func TestBuildHealthExpectedSeries(t *testing.T) {
	instant := healthyPollerInstant(t)
	// COST_METRIC and TOKEN_METRIC queries are deliberately left unmapped,
	// falling back to an empty result (no sample).
	vmSrv := httptest.NewServer(fakeVMHandler{instant: instant})
	defer vmSrv.Close()
	vlSrv := httptest.NewServer(fakeVLHandler{body: ""})
	defer vlSrv.Close()

	o := healthOptions(vmSrv, vlSrv, t.TempDir(), t.TempDir(), idleRunner(t))
	resp := buildHealth(context.Background(), o, 1755300000)

	states := map[string]string{}
	for _, c := range resp.Checks {
		states[c.Name] = c.State
	}
	for _, name := range []string{"claude_in_overage", "claude_extra_usage_usd", "claude_plan_window_pct"} {
		if states[name] != "ok" {
			t.Errorf("%s state = %q, want ok", name, states[name])
		}
	}
	for _, name := range []string{costMetric, tokenMetric} {
		if states[name] != "warn" {
			t.Errorf("%s state = %q, want warn", name, states[name])
		}
	}
	// Exporter-only series missing must not fail the aggregate.
	if !resp.Healthy {
		t.Errorf("Healthy = false, want true (exporter-only series missing should not count against health)")
	}
}

// A poller-scheduled series with no sample must fail the healthy aggregate;
// an exporter-only series missing must not.
func TestBuildHealthyAggregation(t *testing.T) {
	t.Run("poller-scheduled series missing fails health", func(t *testing.T) {
		instant := healthyPollerInstant(t)
		instant["count(count_over_time(claude_in_overage[1h]))"] = successBody(t) // no sample
		vmSrv := httptest.NewServer(fakeVMHandler{instant: instant})
		defer vmSrv.Close()
		vlSrv := httptest.NewServer(fakeVLHandler{body: ""})
		defer vlSrv.Close()

		o := healthOptions(vmSrv, vlSrv, t.TempDir(), t.TempDir(), idleRunner(t))
		resp := buildHealth(context.Background(), o, 1755300000)
		if resp.Healthy {
			t.Error("Healthy = true, want false when a poller-scheduled series has no sample")
		}
	})

	t.Run("unconfigured runner fails health", func(t *testing.T) {
		vmSrv := httptest.NewServer(fakeVMHandler{instant: healthyPollerInstant(t)})
		defer vmSrv.Close()
		vlSrv := httptest.NewServer(fakeVLHandler{body: ""})
		defer vlSrv.Close()

		// A nil Runner is the in-process analogue of the old HTTP probe's
		// connection-refused case: no analysis capability is wired up at all.
		o := healthOptions(vmSrv, vlSrv, t.TempDir(), t.TempDir(), nil)
		resp := buildHealth(context.Background(), o, 1755300000)
		if resp.Healthy {
			t.Error("Healthy = true, want false when the runner is not configured")
		}
	})

	t.Run("all healthy", func(t *testing.T) {
		vmSrv := httptest.NewServer(fakeVMHandler{instant: healthyPollerInstant(t)})
		defer vmSrv.Close()
		vlSrv := httptest.NewServer(fakeVLHandler{body: ""})
		defer vlSrv.Close()

		o := healthOptions(vmSrv, vlSrv, t.TempDir(), t.TempDir(), idleRunner(t))
		resp := buildHealth(context.Background(), o, 1755300000)
		if !resp.Healthy {
			t.Errorf("Healthy = false, want true; checks=%+v", resp.Checks)
		}
	})
}

// The runner check itself: a nil Runner (server built without one) must
// report bad; an idle Runner must report ok/"running, idle"; a Runner with a
// real analyse run in flight must report warn/"running, busy with an
// analysis run".
func TestRunnerCheck(t *testing.T) {
	t.Run("nil runner", func(t *testing.T) {
		check := runnerCheck(nil)
		if check.State != "bad" {
			t.Errorf("state = %q, want bad", check.State)
		}
	})

	t.Run("idle runner", func(t *testing.T) {
		check := runnerCheck(idleRunner(t))
		if check.State != "ok" || check.Detail != "running, idle" {
			t.Errorf("check = %+v, want ok/running, idle", check)
		}
	})

	t.Run("busy runner", func(t *testing.T) {
		r := busyRunnerFor(t, "22222222-2222-4222-8222-222222222222")
		check := runnerCheck(r)
		if check.State != "warn" || check.Detail != "running, busy with an analysis run" {
			t.Errorf("check = %+v, want warn/running, busy with an analysis run", check)
		}
	})
}

// Session indexer freshness: fresh, stale (age > INDEX_STALE_S = 900) and
// missing. The fixture seeds the db directly: the meta row RunIndex sets on
// every pass, plus n session rows.
func TestIndexerCheck(t *testing.T) {
	now := int64(1755300000)

	seedIndex := func(t *testing.T, d *sql.DB, generatedAt int64, n int) {
		t.Helper()
		if err := db.MetaSet(d, sessionsGeneratedAtKey, strconv.FormatInt(generatedAt, 10)); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			row := db.SessionRow{ID: fmt.Sprintf("11111111-1111-4111-8111-%012d", i), Project: "-p"}
			if err := db.UpsertSession(d, row); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("fresh", func(t *testing.T) {
		d := openTestDB(t)
		seedIndex(t, d, now-100, 3)
		check := indexerCheck(d, now)
		if check.State != "ok" {
			t.Errorf("state = %q, want ok; detail=%q", check.State, check.Detail)
		}
	})

	// A four-digit session count must comma-group. ageText's own tests cover
	// the "ago" phrasing, so only the count's grouping is asserted here.
	t.Run("comma-grouped count", func(t *testing.T) {
		d := openTestDB(t)
		seedIndex(t, d, now-100, 1234)
		check := indexerCheck(d, now)
		wantPrefix := "1,234 sessions, generated "
		if !strings.HasPrefix(check.Detail, wantPrefix) {
			t.Errorf("detail = %q, want prefix %q", check.Detail, wantPrefix)
		}
	})

	t.Run("stale", func(t *testing.T) {
		d := openTestDB(t)
		seedIndex(t, d, now-1000, 3)
		check := indexerCheck(d, now)
		if check.State != "warn" {
			t.Errorf("state = %q, want warn; detail=%q", check.State, check.Detail)
		}
	})

	// Missing: a freshly migrated db with no indexing pass ever recorded
	// (no sessions_generated_at meta row), the direct analogue of the old
	// "sessions/index.json never written" case.
	t.Run("missing", func(t *testing.T) {
		d := openTestDB(t)
		check := indexerCheck(d, now)
		if check.State != "bad" {
			t.Errorf("state = %q, want bad; detail=%q", check.State, check.Detail)
		}
	})

	// No database configured at all (Options.DB nil): the same "bad" state
	// as "missing", reached through a different guard.
	t.Run("no db configured", func(t *testing.T) {
		check := indexerCheck(nil, now)
		if check.State != "bad" {
			t.Errorf("state = %q, want bad; detail=%q", check.State, check.Detail)
		}
	})
}

// Retention: parsed from rendered plists when present, falling back to the
// config.js constants with observed=false when absent.
func TestLoadRetention(t *testing.T) {
	writePlist := func(t *testing.T, dir, name, flag string) {
		t.Helper()
		content := fmt.Sprintf(`<?xml version="1.0"?><plist><dict><key>ProgramArguments</key><array><string>%s</string></array></dict></plist>`, flag)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("parses rendered plists", func(t *testing.T) {
		dir := t.TempDir()
		writePlist(t, dir, "com.agent-observability.victoriametrics.plist", "-retentionPeriod=24")
		writePlist(t, dir, "com.agent-observability.victorialogs.plist", "-retentionPeriod=52w")

		vmR, vlR, observed := loadRetention(dir)
		if vmR != "24 months" || vlR != "52 weeks" || !observed {
			t.Errorf("loadRetention = (%q, %q, %v), want (24 months, 52 weeks, true)", vmR, vlR, observed)
		}
	})

	t.Run("falls back when plists are absent", func(t *testing.T) {
		dir := t.TempDir()
		vmR, vlR, observed := loadRetention(dir)
		if vmR != defaultVMRetention || vlR != defaultVLRetention || observed {
			t.Errorf("loadRetention = (%q, %q, %v), want (%q, %q, false)", vmR, vlR, observed, defaultVMRetention, defaultVLRetention)
		}
	})
}

// A smoke test that GetHealth is actually wired through Handler/Options,
// not just that buildHealth works in isolation.
func TestGetHealthRPC(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<title>t</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	vmSrv := httptest.NewServer(fakeVMHandler{instant: healthyPollerInstant(t)})
	defer vmSrv.Close()
	vlSrv := httptest.NewServer(fakeVLHandler{body: ""})
	defer vlSrv.Close()

	o := Options{
		StaticDir: dir, InstallDir: "/tmp", Version: "test",
		VM:              &vm.Client{BaseURL: vmSrv.URL, HTTP: vmSrv.Client()},
		VL:              &vl.Client{BaseURL: vlSrv.URL, HTTP: vlSrv.Client()},
		Memo:            store.New(store.TTL),
		DataDir:         t.TempDir(),
		Runner:          idleRunner(t),
		LaunchAgentsDir: t.TempDir(),
	}
	srv := httptest.NewServer(Handler(o))
	defer srv.Close()
	client := claudeobsv1connect.NewObsServiceClient(http.DefaultClient, srv.URL)

	resp, err := client.GetHealth(context.Background(), connect.NewRequest(&claudeobsv1.GetHealthRequest{}))
	if err != nil {
		t.Fatalf("GetHealth: %v", err)
	}
	if resp.Msg.Identity == nil {
		t.Error("Identity is nil")
	}
}
