package server

import (
	"context"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

func regressionProfile(t *testing.T, o Options, provider profiles.Provider, name string) profiles.Profile {
	t.Helper()
	p, err := o.Profiles.Save(profiles.Profile{Provider: provider, Name: name, Directory: t.TempDir(), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMonitoringSharedAccountUsesNewestSuccessfulWindow(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		firstAge, secondAge     int64
		firstError, secondError string
		account                 string
		wantSecond              bool
		wantCount               int
	}{
		{name: "newer second profile replaces retained stale value", firstAge: 1200, secondAge: 10, firstError: "account refresh failed", account: "shared-account", wantSecond: true, wantCount: 1},
		{name: "newer first profile survives older second value", firstAge: 10, secondAge: 1200, secondError: "account refresh failed", account: "shared-account", wantCount: 1},
		{name: "same timestamp prefers successful second profile", firstAge: 10, secondAge: 10, firstError: "account refresh failed", account: "shared-account", wantSecond: true, wantCount: 1},
		{name: "same timestamp preserves successful first profile", firstAge: 10, secondAge: 10, secondError: "account refresh failed", account: "shared-account", wantCount: 1},
		{name: "unknown accounts remain separate", firstAge: 10, secondAge: 10, wantCount: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := settingsFixture(t)
			first := regressionProfile(t, o, profiles.Codex, "First")
			second := regressionProfile(t, o, profiles.Codex, "Second")
			now := time.Now().Unix()
			for i, p := range []profiles.Profile{first, second} {
				age, usageErr, percent := tc.firstAge, tc.firstError, 80.0
				if i == 1 {
					age, usageErr, percent = tc.secondAge, tc.secondError, 20.0
				}
				snap := monitor.Snapshot{CollectedAt: now, UsageError: usageErr, Usage: codex.Usage{
					AccountID: tc.account, FetchedAt: time.Unix(now-age, 0),
					Windows: []codex.Window{{LimitID: "codex", Kind: "primary", Name: "Codex", UsedPercent: percent, WindowMinutes: 300, ResetsAt: now + 600}},
				}}
				if err := monitor.WriteJSON(o.DB, "codex.snapshot."+p.ID, snap); err != nil {
					t.Fatal(err)
				}
			}
			resp, err := (&obsService{o: o}).GetMonitoring(context.Background(), connect.NewRequest(&pb.GetMonitoringRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.Msg.Windows) != tc.wantCount {
				t.Fatalf("got %d windows, want %d: %v", len(resp.Msg.Windows), tc.wantCount, resp.Msg.Windows)
			}
			if tc.wantCount == 2 {
				if resp.Msg.Windows[0].Profile == resp.Msg.Windows[1].Profile {
					t.Fatal("unidentified accounts were attributed to the same profile")
				}
				return
			}
			wantID, wantPercent := first.ID, 80.0
			if tc.wantSecond {
				wantID, wantPercent = second.ID, 20
			}
			window := resp.Msg.Windows[0]
			if window.Profile != wantID || window.UsedPercent != wantPercent || window.Stale {
				t.Fatalf("picked wrong shared allowance: %v; want profile %s at %.0f%%, fresh", window, wantID, wantPercent)
			}
		})
	}
}

func TestConfiguredHealthKeepsIdleClaudeWarningsInformational(t *testing.T) {
	for _, missingPoller := range []bool{false, true} {
		name := "idle Claude stays healthy"
		if missingPoller {
			name = "missing scheduled poller still degrades health"
		}
		t.Run(name, func(t *testing.T) {
			fixture := settingsFixture(t)
			regressionProfile(t, fixture, profiles.Claude, "Claude work")
			instant := healthyPollerInstant(t)
			if missingPoller {
				delete(instant, "count(count_over_time(claude_in_overage[1h]))")
			}
			vmSrv := httptest.NewServer(fakeVMHandler{instant: instant})
			defer vmSrv.Close()
			vlSrv := httptest.NewServer(fakeVLHandler{body: ""})
			defer vlSrv.Close()
			o := healthOptions(vmSrv, vlSrv, t.TempDir(), t.TempDir(), idleRunner(t))
			o.Profiles, o.DB = fixture.Profiles, fixture.DB
			if err := db.MetaSet(o.DB, sessionsGeneratedAtKey, strconv.FormatInt(testNow, 10)); err != nil {
				t.Fatal(err)
			}
			resp := buildConfiguredHealth(context.Background(), o, testNow)
			states := map[string]string{}
			for _, check := range resp.Checks {
				states[check.Name] = check.State
			}
			for _, metric := range []string{costMetric, tokenMetric} {
				if states[metric] != "warn" {
					t.Errorf("%s = %q; fixture must have no live samples", metric, states[metric])
				}
			}
			if states["Claude work (claude)"] != "ok" {
				t.Fatalf("configured profile is not healthy: %v", resp.Checks)
			}
			if resp.Healthy == missingPoller {
				t.Errorf("healthy = %v, missing scheduled poller = %v", resp.Healthy, missingPoller)
			}
		})
	}
}

func TestCodexSessionsFreshnessIgnoresRecentClaudeIndex(t *testing.T) {
	for _, tc := range []struct {
		name      string
		snapshot  *monitor.Snapshot
		wantStale bool
		wantAge   int64
	}{
		{name: "missing snapshot", wantStale: true},
		{name: "failed recent collection", snapshot: &monitor.Snapshot{CollectedAt: testNow - 20, Error: "history unreadable"}, wantStale: true, wantAge: 20},
		{name: "old successful collection", snapshot: &monitor.Snapshot{CollectedAt: testNow - indexStaleS - 1}, wantStale: true, wantAge: indexStaleS + 1},
		{name: "fresh successful collection", snapshot: &monitor.Snapshot{CollectedAt: testNow - 20}, wantAge: 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := settingsFixture(t)
			p := regressionProfile(t, o, profiles.Codex, "Codex work")
			if err := db.MetaSet(o.DB, sessionsGeneratedAtKey, strconv.FormatInt(testNow, 10)); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertSession(o.DB, db.SessionRow{ID: codex.SessionID(p.ID, "thread"), Provider: "codex", Profile: p.ID, NativeID: "thread", CostKnown: true, UsageKnown: true}); err != nil {
				t.Fatal(err)
			}
			if tc.snapshot != nil {
				if err := monitor.WriteJSON(o.DB, "codex.snapshot."+p.ID, tc.snapshot); err != nil {
					t.Fatal(err)
				}
			}
			// Both the explicit provider filter and selecting a Codex profile
			// without a provider parameter must use the Codex snapshot.
			for _, selection := range []struct{ profile, provider string }{{"", "codex"}, {p.ID, ""}} {
				resp, err := buildProviderSessions(context.Background(), o, selection.profile, selection.provider, testNow)
				if err != nil {
					t.Fatal(err)
				}
				if len(resp.Sessions) != 1 || resp.IndexStale != tc.wantStale {
					t.Fatalf("selection %+v: rows=%d, stale=%v; want 1 row, stale=%v", selection, len(resp.Sessions), resp.IndexStale, tc.wantStale)
				}
				wantGenerated := ""
				if tc.snapshot != nil {
					wantGenerated = time.Unix(testNow-tc.wantAge, 0).UTC().Format(time.RFC3339)
				}
				if resp.GeneratedAt != wantGenerated {
					t.Errorf("generatedAt = %q, want Codex snapshot %q", resp.GeneratedAt, wantGenerated)
				}
			}
		})
	}
}
