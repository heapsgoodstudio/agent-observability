package server

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

func writeCodexQuota(t *testing.T, o Options, p profiles.Profile, account string, fetched time.Time, buckets ...codex.Bucket) {
	t.Helper()
	snap := monitor.QuotaSnapshot{Directory: p.Directory, Usage: codex.Usage{AccountID: account, PlanType: "self_serve_business_prolite", FetchedAt: fetched, Buckets: buckets}}
	if err := monitor.WriteJSON(o.DB, "codex.quota."+p.ID, snap); err != nil {
		t.Fatal(err)
	}
}

func codexAccount(t *testing.T, o Options, now int64) *pb.AccountSnapshot {
	t.Helper()
	out, err := buildFilteredUsageSnapshot(context.Background(), o, &pb.GetUsageSnapshotRequest{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("want one account: %v", out.Accounts)
	}
	return out.Accounts[0]
}

func TestCodexAccountReportsCreditOverageAndItsEstimate(t *testing.T) {
	o := settingsFixture(t)
	now := int64(1800000000)
	p := regressionProfile(t, o, profiles.Codex, "Codex")
	// The codex bucket is the account's own meter; another bucket's limit
	// must not replace it or be added to it.
	writeCodexQuota(t, o, p, "acct", time.Unix(now-10, 0),
		codex.Bucket{LimitID: "other", IndividualLimit: &codex.SpendControlLimit{Limit: "1000", Used: "7", ResetsAt: now + 1}},
		codex.Bucket{LimitID: "codex", IndividualLimit: &codex.SpendControlLimit{Limit: "1000", Used: "212.4375", RemainingPercent: 99, ResetsAt: now + 86400}},
	)

	o.CodexCreditUSD = 0.04
	b := codexAccount(t, o, now).Billing
	if b.GetReportedOverageCredits() != 212.4375 || b.GetPeriodResetsAt() != float64(now+86400) || b.ReportedOverageUsd != nil || b.State != "fresh" {
		t.Fatalf("credit overage wrong: %v", b)
	}
	if b.EstimatedOverageUsd == nil || math.Abs(b.GetEstimatedOverageUsd()-8.4975) > 1e-9 || b.GetCreditPriceUsd() != 0.04 || b.Currency != "USD" || !strings.Contains(b.Note, "$0.04") {
		t.Fatalf("dollar estimate wrong: %v", b)
	}

	o.CodexCreditUSD = 0
	b = codexAccount(t, o, now).Billing
	if b.GetReportedOverageCredits() != 212.4375 || b.EstimatedOverageUsd != nil || b.CreditPriceUsd != nil || !strings.Contains(b.Note, "CODEX_CREDIT_USD") {
		t.Fatalf("no price must leave credits only: %v", b)
	}

	writeCodexQuota(t, o, p, "acct", time.Unix(now-10, 0), codex.Bucket{LimitID: "codex", IndividualLimit: &codex.SpendControlLimit{Used: "unknown"}})
	o.CodexCreditUSD = 0.04
	b = codexAccount(t, o, now).Billing
	if b.ReportedOverageCredits != nil || b.EstimatedOverageUsd != nil || b.State != "unavailable" {
		t.Fatalf("an unreadable amount became a figure: %v", b)
	}
}

func TestMonitoringCountsEachAccountsCreditOverageOnce(t *testing.T) {
	o := settingsFixture(t)
	o.CodexCreditUSD = 0.04
	now := time.Now()
	limit := func(used string) codex.Bucket {
		return codex.Bucket{LimitID: "codex", IndividualLimit: &codex.SpendControlLimit{Limit: "1000", Used: used}}
	}
	// Two profiles signed in to one account report the same limit once.
	writeCodexQuota(t, o, regressionProfile(t, o, profiles.Codex, "First"), "shared", now, limit("10"))
	writeCodexQuota(t, o, regressionProfile(t, o, profiles.Codex, "Second"), "shared", now, limit("10"))
	writeCodexQuota(t, o, regressionProfile(t, o, profiles.Codex, "Other"), "other", now, limit("5"))
	resp, err := (&obsService{o: o}).GetMonitoring(context.Background(), connect.NewRequest(&pb.GetMonitoringRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	s := resp.Msg.Billing
	if s.GetReportedOverageCredits() != 15 || math.Abs(s.GetEstimatedOverageUsd()-0.6) > 1e-9 || s.GetCreditPriceUsd() != 0.04 || s.ReportedOverageUsd != nil || !strings.Contains(s.Note, "$0.04") {
		t.Fatalf("credit summary wrong: %v", s)
	}
}
