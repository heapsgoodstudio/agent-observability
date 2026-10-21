package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

func TestNeutralSnapshotCodexOnlyPresenceSharedAccountAndSameRole(t *testing.T) {
	o := settingsFixture(t)
	now := int64(1800000000)
	first := regressionProfile(t, o, profiles.Codex, "First")
	second := regressionProfile(t, o, profiles.Codex, "Second")
	allowed := false
	credits := int64(0)
	balance := "0.00"
	for i, p := range []profiles.Profile{first, second} {
		fetched := now - 10
		pollErr := ""
		pct := 0.0
		if i == 0 {
			fetched = now - 600
			pollErr = "failed poll"
			pct = 85
		}
		snap := monitor.QuotaSnapshot{Directory: p.Directory, UsageError: pollErr, Usage: codex.Usage{AccountID: "shared", PlanType: "plus", FetchedAt: time.Unix(fetched, 0), OrdinaryUsageAllowed: &allowed, ResetCreditsAvailable: &credits, Windows: []codex.Window{{LimitID: "codex", Kind: "primary", UsedPercent: pct, WindowMinutes: 10080, WindowMinutesKnown: true, ResetsAt: now + 600, ResetsAtKnown: true}}, Buckets: []codex.Bucket{{LimitID: "codex", Credits: &codex.Credits{Balance: &balance}}}}}
		if err := monitor.WriteJSON(o.DB, "codex.quota."+p.ID, snap); err != nil {
			t.Fatal(err)
		}
	}
	out, err := buildFilteredUsageSnapshot(context.Background(), o, &pb.GetUsageSnapshotRequest{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if out.SchemaVersion != 2 || len(out.Profiles) != 0 || len(out.Accounts) != 1 {
		t.Fatalf("Codex-only response failed: %v", out)
	}
	a := out.Accounts[0]
	if len(a.Profiles) != 2 || a.State != "fresh" || a.GetObservedAt() != float64(now-10) || a.Windows[0].UsedPercent == nil || *a.Windows[0].UsedPercent != 0 || a.IncludedUsageAllowed == nil || *a.IncludedUsageAllowed || a.ResetCreditsAvailable == nil || *a.ResetCreditsAvailable != 0 {
		t.Fatalf("presence/shared freshest broken: %v", a)
	}
	if a.CurrentWindowId == "" || a.CurrentWindowId != a.WeeklyWindowId || len(a.Windows) != 1 {
		t.Fatalf("primary weekly duplicated or mislabelled: %v", a)
	}
	if a.Billing.ReportedOverageUsd != nil || a.Billing.EstimatedOverageUsd != nil || a.Billing.State != "unavailable" || a.CodexBuckets[0].Credits.GetBalance() != "0.00" {
		t.Fatalf("billing invented or credits lost: %v", a)
	}
	before, _, err := db.ListAllowanceObservations(o.DB, "codex", a.Key, 0, now)
	if err != nil || len(before) != 0 {
		t.Fatalf("UI read wrote history: %v %v", before, err)
	}
}

func TestDashboardScopeRejectsUnknownAndCrossProvider(t *testing.T) {
	o := settingsFixture(t)
	p := regressionProfile(t, o, profiles.Codex, "Codex")
	now := int64(1800000000)
	if err := monitor.WriteJSON(o.DB, "codex.quota."+p.ID, monitor.QuotaSnapshot{Usage: codex.Usage{AccountID: "same", FetchedAt: time.Unix(now, 0)}}); err != nil {
		t.Fatal(err)
	}
	for _, req := range []*pb.GetUsageSnapshotRequest{{AccountKey: "codex:missing"}, {Provider: "claude", AccountKey: "codex:same"}, {Provider: "claude", Profile: p.ID}, {Profile: "missing"}, {Provider: "other"}} {
		if _, err := buildFilteredUsageSnapshot(context.Background(), o, req, now); err == nil {
			t.Fatalf("unknown scope widened: %v", req)
		}
	}
	scope, err := resolveDashboardScope(context.Background(), o, "codex", "codex:same", "", now)
	if err != nil || scope.All || len(scope.Profiles) != 1 || !strings.Contains(scope.ScopeNote, "profile history") {
		t.Fatalf("account historical provenance missing: %+v %v", scope, err)
	}
	service := &obsService{o: o}
	_, err = service.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{AccountKey: "codex:missing"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("scope error lost: %v", err)
	}
}

func TestClaudeNeutralFreshnessNeverUsesResponseAssemblyTime(t *testing.T) {
	now := int64(1800000000)
	instant := map[string]string{
		"max by (account,source)(timestamp(claude_plan_data_age_seconds) - claude_plan_data_age_seconds)":                     successBody(t, vmResult{Metric: map[string]string{"account": "a", "source": "cache"}, Value: []any{float64(now), "1799990000"}}),
		"max by (account,source)(timestamp(claude_plan_data_confirmed_age_seconds) - claude_plan_data_confirmed_age_seconds)": successBody(t, vmResult{Metric: map[string]string{"account": "a", "source": "cache"}, Value: []any{float64(now), "1799999900"}}),
	}
	srv := httptest.NewServer(fakeVMHandler{instant: instant})
	defer srv.Close()
	o := Options{VM: &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}}
	p := &pb.ProfileRow{Profile: "one", Provider: "claude", Account: "a"}
	l := &pb.UsageProfile{Profile: "one", Account: "a", ChangedAgeKnown: true, ChangedAgeS: 0, ConfirmedAgeKnown: true, ConfirmedAgeS: 0, Windows: []*pb.UsageWindow{{Window: "five_hour", Pct: 0, HasPct: true, Source: "cache"}}}
	a := buildDashboardAccounts(context.Background(), o, []*pb.ProfileRow{p}, []*pb.UsageProfile{l}, now)[0]
	if a.State != "fresh" || a.GetObservedAt() != 1799990000 || a.GetConfirmedAt() != 1799999900 || a.Billing.ReportedOverageUsd != nil {
		t.Fatalf("wrong timestamp/presence: %v", a)
	}
	for _, tc := range []struct {
		elapsed int64
		state   string
	}{{200, "fresh"}, {500, "fresh"}, {501, "stale"}} {
		later := buildDashboardAccounts(context.Background(), o, []*pb.ProfileRow{p}, []*pb.UsageProfile{l}, now+tc.elapsed)[0]
		if later.State != tc.state || later.GetConfirmedAt() != a.GetConfirmedAt() {
			t.Fatalf("incorrect confirmation age after %ds: %v", tc.elapsed, later)
		}
	}
}

func TestClaudeNeutralDedupChoosesFreshestSourceBeforeAccountMaximum(t *testing.T) {
	now := int64(1800000000)
	mk := func(profile, source, window string, value float64) vmResult {
		return vmResult{Metric: map[string]string{"account": "shared", "profile": profile, "source": source, "window": window}, Value: []any{float64(now), value}}
	}
	instant := map[string]string{
		"claude_plan_window_pct":                                                                     successBody(t, mk("old", "cache", "five_hour", 99), mk("new", "cache", "five_hour", 3)),
		"claude_plan_window_reset_timestamp_seconds":                                                 successBody(t, mk("old", "cache", "five_hour", float64(now-5)), mk("new", "cache", "five_hour", float64(now+500))),
		"timestamp(claude_plan_data_age_seconds) - claude_plan_data_age_seconds":                     successBody(t, mk("old", "cache", "", float64(now-1000)), mk("new", "cache", "", float64(now-10))),
		"timestamp(claude_plan_data_confirmed_age_seconds) - claude_plan_data_confirmed_age_seconds": successBody(t, mk("old", "cache", "", float64(now-900)), mk("new", "cache", "", float64(now-10))),
		"claude_extra_usage_usd":                                                                     successBody(t, mk("old", "", "", 9), mk("new", "", "", 0)),
	}
	srv := httptest.NewServer(fakeVMHandler{instant: instant})
	defer srv.Close()
	o := Options{VM: &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}}
	a := &pb.AccountSnapshot{Provider: "claude", Account: "shared", Billing: &pb.BillingSnapshot{}}
	applyClaudeObservations(context.Background(), o, map[string]*pb.AccountSnapshot{"claude:shared": a}, now)
	if len(a.Windows) != 1 || a.Windows[0].GetUsedPercent() != 3 || a.Windows[0].GetResetsAt() != float64(now+500) || a.State != "fresh" {
		t.Fatalf("stale account maximum replaced new period: %v", a)
	}
	if a.Billing.ReportedOverageUsd == nil || a.Billing.GetReportedOverageUsd() != 0 || a.Billing.GetObservedAt() != float64(now-10) {
		t.Fatalf("shared bill summed/maxed or zero lost: %v", a.Billing)
	}
}

func TestNeutralAllowanceAndBillingFailuresRemainSeparate(t *testing.T) {
	srv := httptest.NewServer(fakeVMHandler{fail: "claude_extra_usage_usd"})
	defer srv.Close()
	a := &pb.AccountSnapshot{Provider: "claude", Account: "a", State: "fresh", Billing: &pb.BillingSnapshot{State: "fresh", ReportedOverageUsd: number(0)}, Windows: []*pb.AllowanceWindow{{Id: "five_hour", State: "fresh", UsedPercent: number(0)}}}
	applyClaudeObservations(context.Background(), Options{VM: &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}}, map[string]*pb.AccountSnapshot{"claude:a": a}, 1800000000)
	if a.State != "fresh" || a.Windows[0].State != "fresh" || a.Billing.State != "stale" || a.Billing.ReportedOverageUsd == nil || !strings.Contains(a.Billing.Note, "unavailable") {
		t.Fatalf("billing failure hid quota or old bill: %v", a)
	}
}

func TestDashboardProfileAccountSwitchUsesSourceTimestamp(t *testing.T) {
	now := int64(1800000000)
	for _, tie := range []bool{false, true} {
		latest := float64(now)
		previous := latest - 300
		if tie {
			previous = latest
		}
		body := successBody(t, vmResult{Metric: map[string]string{"profile": "one", "account": "old"}, Value: []any{float64(now), 1}}, vmResult{Metric: map[string]string{"profile": "one", "account": "new"}, Value: []any{float64(now), 1}})
		timestamps := successBody(t, vmResult{Metric: map[string]string{"profile": "one", "account": "old"}, Value: []any{float64(now), previous}}, vmResult{Metric: map[string]string{"profile": "one", "account": "new"}, Value: []any{float64(now), latest}})
		srv := httptest.NewServer(fakeVMHandler{instant: map[string]string{accountInfoQuery: body, "timestamp(claude_profile_account_info)": timestamps}})
		o := Options{VM: &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}}
		roster, err := dashboardProfileRoster(context.Background(), o, now)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := "new"
		if tie {
			want = ""
		}
		if len(roster.Profiles) != 1 || roster.Profiles[0].Account != want {
			t.Fatalf("profile switch misattributed (tie %v): %v", tie, roster)
		}
	}
}

func TestClaudeAccountConfirmationDescribesEveryWindow(t *testing.T) {
	now := int64(1800000000)
	for _, missing := range []bool{false, true} {
		mk := func(source, window string, value float64) vmResult {
			return vmResult{Metric: map[string]string{"account": "a", "profile": "one", "source": source, "window": window}, Value: []any{float64(now), value}}
		}
		confirmation := successBody(t, mk("cache", "", float64(now-660)))
		if missing {
			confirmation = successBody(t)
		}
		instant := map[string]string{
			"claude_plan_window_pct": successBody(t, mk("statusline", "five_hour", 12), mk("cache", "weekly_scoped", 5)),
			"timestamp(claude_plan_data_age_seconds) - claude_plan_data_age_seconds":                     successBody(t, mk("statusline", "", float64(now-60)), mk("cache", "", float64(now-2000))),
			"timestamp(claude_plan_data_confirmed_age_seconds) - claude_plan_data_confirmed_age_seconds": confirmation,
		}
		srv := httptest.NewServer(fakeVMHandler{instant: instant})
		a := &pb.AccountSnapshot{Provider: "claude", Account: "a", Billing: &pb.BillingSnapshot{}}
		applyClaudeObservations(context.Background(), Options{VM: &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}}, map[string]*pb.AccountSnapshot{"claude:a": a}, now)
		srv.Close()
		if a.State != "stale" || a.GetObservedAt() != float64(now-60) {
			t.Fatalf("aggregate state or latest change altered: %v", a)
		}
		if missing {
			if a.ConfirmedAt != nil {
				t.Fatalf("unconfirmed model borrowed current confirmation: %v", a)
			}
		} else if a.GetConfirmedAt() != float64(now-660) {
			t.Fatalf("stale account labelled with newest partial confirmation: %v", a)
		}
	}
}

func TestClaudeConfirmsOnlyMatchingSelectedReadingWithoutChangingItsSource(t *testing.T) {
	now := int64(1800000000)
	for _, tc := range []struct {
		name, account, window   string
		pct, reset, confirmed   float64
		missingReset, wantFresh bool
	}{
		{name: "matching confirmation", account: "a", window: "five_hour", pct: 42, reset: float64(now + 600), confirmed: float64(now - 60), wantFresh: true},
		{name: "different percentage", account: "a", window: "five_hour", pct: 41, reset: float64(now + 600), confirmed: float64(now - 60)},
		{name: "different reset period", account: "a", window: "five_hour", pct: 42, reset: float64(now + 900), confirmed: float64(now - 60)},
		{name: "different account", account: "b", window: "five_hour", pct: 42, reset: float64(now + 600), confirmed: float64(now - 60)},
		{name: "different window", account: "a", window: "seven_day", pct: 42, reset: float64(now + 600), confirmed: float64(now - 60)},
		{name: "missing reset evidence", account: "a", window: "five_hour", pct: 42, confirmed: float64(now - 60), missingReset: true},
		{name: "confirmation predates selected observation", account: "a", window: "five_hour", pct: 42, reset: float64(now + 600), confirmed: float64(now - 700)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mk := func(account, source, window string, value float64) vmResult {
				return vmResult{Metric: map[string]string{"account": account, "profile": "one", "source": source, "window": window}, Value: []any{float64(now), value}}
			}
			resets := []vmResult{mk("a", "statusline", "five_hour", float64(now+600))}
			if !tc.missingReset {
				resets = append(resets, mk(tc.account, "cache", tc.window, tc.reset))
			}
			instant := map[string]string{
				"claude_plan_window_pct":                                                                     successBody(t, mk("a", "statusline", "five_hour", 42), mk(tc.account, "cache", tc.window, tc.pct)),
				"claude_plan_window_reset_timestamp_seconds":                                                 successBody(t, resets...),
				"timestamp(claude_plan_data_age_seconds) - claude_plan_data_age_seconds":                     successBody(t, mk("a", "statusline", "", float64(now-660)), mk(tc.account, "cache", "", float64(now-900))),
				"timestamp(claude_plan_data_confirmed_age_seconds) - claude_plan_data_confirmed_age_seconds": successBody(t, mk(tc.account, "cache", "", tc.confirmed)),
			}
			srv := httptest.NewServer(fakeVMHandler{instant: instant})
			defer srv.Close()
			a := &pb.AccountSnapshot{Provider: "claude", Account: "a", Billing: &pb.BillingSnapshot{}}
			applyClaudeObservations(context.Background(), Options{VM: &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}}, map[string]*pb.AccountSnapshot{"claude:a": a}, now)
			var current *pb.AllowanceWindow
			for _, w := range a.Windows {
				if w.Id == "five_hour" {
					current = w
				}
			}
			if current == nil || current.Source != "statusline" || current.GetObservedAt() != float64(now-660) || current.GetUsedPercent() != 42 || current.GetResetsAt() != float64(now+600) {
				t.Fatalf("changed value/source selection: %v", current)
			}
			if (current.State == "fresh") != tc.wantFresh {
				t.Fatalf("wrong corroboration (want fresh %v): %v", tc.wantFresh, current)
			}
			want := float64(now - 660)
			if tc.wantFresh {
				want = tc.confirmed
			}
			if current.GetConfirmedAt() != want {
				t.Fatalf("invented confirmation: %v", current)
			}
		})
	}
}

func TestClaudeAccountConfirmationNormalizesRetainedFallback(t *testing.T) {
	newest, oldest := float64(1800000000-60), float64(1800000000-360)
	for _, missing := range []bool{false, true} {
		a := &pb.AccountSnapshot{Provider: "claude", State: "stale", ObservedAt: &newest, ConfirmedAt: &newest, Windows: []*pb.AllowanceWindow{{ConfirmedAt: &newest}, {ConfirmedAt: &oldest}}}
		if missing {
			a.Windows[1].ConfirmedAt = nil
		}
		applyClaudeObservations(context.Background(), Options{}, map[string]*pb.AccountSnapshot{"a": a}, 1800000000)
		if a.State != "stale" || a.GetObservedAt() != newest {
			t.Fatalf("changed fallback error/state policy: %v", a)
		}
		if missing {
			if a.ConfirmedAt != nil {
				t.Fatalf("borrowed confirmation for missing window: %v", a)
			}
		} else if a.GetConfirmedAt() != oldest {
			t.Fatalf("fallback kept misleading newest timestamp: %v", a)
		}
	}
	codex := &pb.AccountSnapshot{Provider: "codex", ConfirmedAt: &newest, Windows: []*pb.AllowanceWindow{{ConfirmedAt: &oldest}}}
	summarizeClaudeConfirmation(codex)
	if codex.GetConfirmedAt() != newest {
		t.Fatal("changed Codex snapshot confirmation policy")
	}
}
