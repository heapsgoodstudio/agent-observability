package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

func TestMonitoringNeverPricesIncludedOrUnclassifiedHistory(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(map[bool]string{true: "included available", false: "included unavailable"}[allowed], func(t *testing.T) {
			o := settingsFixture(t)
			p := regressionProfile(t, o, profiles.Codex, "Default")
			if err := db.UpsertSession(o.DB, db.SessionRow{ID: codex.SessionID(p.ID, "old"), Provider: "codex", Profile: p.ID, Requests: 23, CostUSD: 9876, CostKnown: true, UsageKnown: true}); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertSession(o.DB, db.SessionRow{ID: codex.SessionID(p.ID, "missing"), Provider: "codex", Profile: p.ID}); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if err := monitor.WriteJSON(o.DB, "codex.snapshot."+p.ID, monitor.Snapshot{CollectedAt: now.Unix(), Usage: codex.Usage{FetchedAt: now, OrdinaryUsageAllowed: &allowed, Windows: []codex.Window{{LimitID: "codex", Kind: "primary", UsedPercent: 100, ResetsAt: now.Add(time.Hour).Unix(), WindowMinutes: 300}}}}); err != nil {
				t.Fatal(err)
			}
			resp, err := (&obsService{o: o}).GetMonitoring(context.Background(), connect.NewRequest(&pb.GetMonitoringRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			r := resp.Msg
			if r.EstimatedCostUsd != 0 || r.CostComplete || len(r.Models) != 0 {
				t.Fatal("whole-history monetary valuation leaked into monitoring", r)
			}
			if r.Billing == nil || r.Billing.ReportedOverageUsd != nil || r.Billing.EstimatedOverageUsd != nil {
				t.Fatal("quota state invented a reported or estimated bill", r.Billing)
			}
			if r.Billing.UnclassifiedRequests != 23 || r.Billing.UnclassifiedSessions != 2 || r.Billing.IncludedRequests != 0 || r.Billing.OutsideCapRequests != 0 || r.UsageComplete {
				t.Fatal("incomplete historical usage was misclassified", r)
			}
		})
	}
}

func TestMonitoringUsageGapsAreSpecificFilteredAndNotDuplicated(t *testing.T) {
	o := settingsFixture(t)
	p := regressionProfile(t, o, profiles.Codex, "Work")
	other := regressionProfile(t, o, profiles.Codex, "Personal")
	id := codex.SessionID(p.ID, "interrupted")
	for _, row := range []db.SessionRow{
		{ID: id, Provider: "codex", Profile: p.ID, Start: 123},
		{ID: codex.SessionID(p.ID, "rejected"), Provider: "codex", Profile: p.ID, UsageKnown: true},
		{ID: codex.SessionID(other.ID, "missing"), Provider: "codex", Profile: other.ID},
	} {
		if err := db.UpsertSession(o.DB, row); err != nil {
			t.Fatal(err)
		}
	}
	snap := monitor.Snapshot{Warnings: []string{"Token detail is unavailable for one or more threads; totals are partial", "Another collection problem"}, UsageGaps: map[string]string{id: "interrupted"}, Usage: codex.Usage{FetchedAt: time.Now(), Windows: []codex.Window{{UsedPercent: 25}}}}
	if err := monitor.WriteJSON(o.DB, "codex.snapshot."+p.ID, snap); err != nil {
		t.Fatal(err)
	}
	resp, err := (&obsService{o: o}).GetMonitoring(context.Background(), connect.NewRequest(&pb.GetMonitoringRequest{Profile: p.ID}))
	if err != nil {
		t.Fatal(err)
	}
	r := resp.Msg
	if r.UsageComplete || r.Sessions != 2 || len(r.UsageGaps) != 1 || r.UsageGaps[0].SessionId != id || r.UsageGaps[0].ProfileName != "Work" || r.UsageGaps[0].StartedAt != 123 || !strings.Contains(r.UsageGaps[0].Reason, "Interrupted") {
		t.Fatal("wrong usage coverage", r)
	}
	if len(r.Errors) != 1 || r.Errors[0].Error != "Another collection problem" {
		t.Fatal("duplicate token banner or lost unrelated diagnostic", r.Errors)
	}
	if len(r.Windows) != 1 || r.Windows[0].UsedPercent != 25 || r.Windows[0].Stale {
		t.Fatal("historical usage gap affected live headroom", r.Windows)
	}
}

func TestMonitoringPreservesZeroFalseAndUnknownCreditControls(t *testing.T) {
	o := settingsFixture(t)
	p := regressionProfile(t, o, profiles.Codex, "Work")
	now := time.Now()
	allowed, reached, balance, resets := false, false, "0", int64(0)
	u := codex.Usage{FetchedAt: now, OrdinaryUsageAllowed: &allowed, ResetCreditsAvailable: &resets, Buckets: []codex.Bucket{
		{LimitID: "known", Credits: &codex.Credits{Balance: &balance}, SpendControlReached: &reached},
		{LimitID: "unknown", Credits: &codex.Credits{HasCredits: true}},
	}}
	if err := monitor.WriteJSON(o.DB, "codex.snapshot."+p.ID, monitor.Snapshot{Usage: u}); err != nil {
		t.Fatal(err)
	}
	resp, err := (&obsService{o: o}).GetMonitoring(context.Background(), connect.NewRequest(&pb.GetMonitoringRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Accounts) != 1 || len(resp.Msg.Windows) != 0 {
		t.Fatal("credit-only account disappeared", resp.Msg)
	}
	a := resp.Msg.Accounts[0]
	if a.Stale || a.OrdinaryUsageAllowed == nil || *a.OrdinaryUsageAllowed || a.ResetCreditsAvailable == nil || *a.ResetCreditsAvailable != 0 {
		t.Fatal("explicit false/zero controls lost", a)
	}
	if a.Buckets[0].Credits.Balance == nil || *a.Buckets[0].Credits.Balance != "0" || a.Buckets[0].SpendControlReached == nil || *a.Buckets[0].SpendControlReached {
		t.Fatal("known zero balance or false control became unknown", a)
	}
	if a.Buckets[1].Credits.Balance != nil || a.Buckets[1].SpendControlReached != nil {
		t.Fatal("unknown balance/control became known", a)
	}
}

func TestMonitoringRetainedAllowanceIsStaleAfterFiveMinutes(t *testing.T) {
	p := profiles.Profile{ID: "p", Name: "Work"}
	now := time.Now()
	allowed := true
	for _, tc := range []struct {
		age   time.Duration
		fail  string
		stale bool
	}{
		{time.Minute, "", false}, {3 * time.Minute, "", false}, {5 * time.Minute, "", false}, {5*time.Minute + time.Second, "", true}, {time.Second, "authentication unavailable", true}, {-2 * time.Minute, "", true},
	} {
		a := codexAccountProto(p, monitor.Snapshot{UsageError: tc.fail, Usage: codex.Usage{FetchedAt: now.Add(-tc.age), OrdinaryUsageAllowed: &allowed}}, now)
		if a.Stale != tc.stale {
			t.Fatalf("age %v / error %q: stale=%v", tc.age, tc.fail, a.Stale)
		}
	}
}

func TestMonitoringMovedProfileNeverShowsPreviousAccountsAllowance(t *testing.T) {
	o := settingsFixture(t)
	p := regressionProfile(t, o, profiles.Codex, "Moved")
	allowed := true
	snap := monitor.Snapshot{UsageDirectory: "/previous/home", Usage: codex.Usage{AccountID: "previous-account", FetchedAt: time.Now(), OrdinaryUsageAllowed: &allowed, Windows: []codex.Window{{UsedPercent: 0, WindowMinutes: 300}}}}
	if err := monitor.WriteJSON(o.DB, "codex.snapshot."+p.ID, snap); err != nil {
		t.Fatal(err)
	}
	resp, err := (&obsService{o: o}).GetMonitoring(context.Background(), connect.NewRequest(&pb.GetMonitoringRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Windows) != 0 || len(resp.Msg.Accounts) != 1 || resp.Msg.Accounts[0].Account != "" || !resp.Msg.Accounts[0].Stale || resp.Msg.Accounts[0].OrdinaryUsageAllowed != nil {
		t.Fatal("moved profile exposed another account's remaining allowance", resp.Msg)
	}
}

func TestMonitoringFreshCreditOnlySnapshotReplacesOlderAccountWindows(t *testing.T) {
	o := settingsFixture(t)
	first := regressionProfile(t, o, profiles.Codex, "First")
	second := regressionProfile(t, o, profiles.Codex, "Second")
	now := time.Now()
	old := monitor.Snapshot{Usage: codex.Usage{AccountID: "shared", FetchedAt: now.Add(-time.Second), Windows: []codex.Window{{LimitID: "codex", UsedPercent: 100}}}}
	newer := monitor.QuotaSnapshot{Directory: second.Directory, Usage: codex.Usage{AccountID: "shared", FetchedAt: now, Buckets: []codex.Bucket{{LimitID: "codex", Credits: &codex.Credits{HasCredits: true}}}}}
	if err := monitor.WriteJSON(o.DB, "codex.snapshot."+first.ID, old); err != nil {
		t.Fatal(err)
	}
	if err := monitor.WriteJSON(o.DB, "codex.quota."+second.ID, newer); err != nil {
		t.Fatal(err)
	}
	resp, err := (&obsService{o: o}).GetMonitoring(context.Background(), connect.NewRequest(&pb.GetMonitoringRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Accounts) != 1 || len(resp.Msg.Windows) != 0 || resp.Msg.Accounts[0].Profile != second.ID {
		t.Fatal("older allowance survived a newer whole-account observation", resp.Msg)
	}
}
