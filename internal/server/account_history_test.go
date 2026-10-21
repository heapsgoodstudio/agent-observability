package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

func TestAllowanceHistorySeparatesResetAndMissingIntervals(t *testing.T) {
	rows := []db.AllowanceObservation{
		{WindowID: "primary", ObservedAt: 1000, ResetAt: 1500, UsedPercent: 90},
		{WindowID: "primary", ObservedAt: 1060, ResetAt: 1500, UsedPercent: 91},
		{WindowID: "primary", ObservedAt: 1060, ResetAt: 1500, UsedPercent: 91},
		{WindowID: "primary", ObservedAt: 1520, ResetAt: 2000, UsedPercent: 2},
		{WindowID: "primary", ObservedAt: 1800, ResetAt: 2000, UsedPercent: 20},
		{WindowID: "unknown", ObservedAt: 1000, UsedPercent: 90},
		{WindowID: "unknown", ObservedAt: 1060, UsedPercent: 5},
	}
	series := historySeries(rows, 900, 1900, 180)
	if len(series) != 5 {
		t.Fatalf("joined gaps, resets, or unknown-reset decline: %+v", series)
	}
	total := 0
	for _, s := range series {
		for _, p := range s.Points {
			total++
			if p.Ts == 900 || p.Ts == 1500 {
				t.Fatal("invented window start/reset point")
			}
		}
	}
	if total != 6 {
		t.Fatalf("duplicates not removed: %d", total)
	}
}

func TestLegacyBillingRequiresExplicitExactAccountAndKeepsProvenance(t *testing.T) {
	now := int64(1800000000)
	mk := func(account, profile, source string, value float64, at int64) vmResult {
		return vmResult{Metric: map[string]string{"account": account, "profile": profile, "source": source}, Values: [][2]any{{float64(at), value}}}
	}
	key := `{account="",profile=""}`
	at := now - 900
	ranged := map[string]string{
		"claude_extra_usage_usd" + key: successBody(t,
			mk("", "", "", 4321.09, at),
			mk("other", "", "", 99999, at),
			mk("", "unresolved-profile", "", 88888, at)),
		"claude_plan_data_age_seconds" + key:                 successBody(t, mk("", "", "cache", 0, at)),
		"timestamp(claude_plan_data_age_seconds" + key + ")": successBody(t, mk("", "", "cache", float64(at), at)),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged})
	defer srv.Close()
	client := &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}
	for _, mapping := range []string{"", "claude:other", "codex:wanted", "wanted"} {
		out := &pb.GetAccountHistoryResponse{AccountKey: "claude:wanted"}
		loadLegacyClaudeBilling(context.Background(), Options{VM: client, PrimaryProfile: "wanted", LegacyClaudeBillingAccount: mapping}, now-3600, now, out)
		if len(out.Billing) != 0 {
			t.Fatalf("legacy billing leaked under mapping %q: %v", mapping, out.Billing)
		}
	}
	out := &pb.GetAccountHistoryResponse{AccountKey: "claude:wanted"}
	loadLegacyClaudeBilling(context.Background(), Options{VM: client, LegacyClaudeBillingAccount: out.AccountKey}, now-3600, now, out)
	if len(out.Billing) != 1 || len(out.Billing[0].Points) != 1 || out.Billing[0].Points[0].Value != 4321.09 {
		t.Fatalf("confirmed legacy history missing or includes another identity: %v", out)
	}
	if out.Billing[0].Source != "Claude usage cache · legacy collector, manually assigned" {
		t.Fatalf("lost attribution provenance: %v", out.Billing[0])
	}
	// A single earlier labelled reading must not suppress later legacy data.
	out.Billing = []*pb.BillingHistorySeries{{Points: []*pb.Point{{Ts: float64(at - 60), Value: 12}}}}
	loadLegacyClaudeBilling(context.Background(), Options{VM: client, LegacyClaudeBillingAccount: out.AccountKey}, now-3600, now, out)
	if len(out.Billing) != 2 || out.Billing[0].Points[0].Value != 4321.09 {
		t.Fatalf("sparse labelled history hid later legacy readings: %v", out.Billing)
	}
	// Account attribution takes precedence for the same source observation.
	out.Billing = []*pb.BillingHistorySeries{{Points: []*pb.Point{{Ts: float64(at), Value: 12}}}}
	loadLegacyClaudeBilling(context.Background(), Options{VM: client, LegacyClaudeBillingAccount: out.AccountKey}, now-3600, now, out)
	if len(out.Billing) != 1 || out.Billing[0].Points[0].Value != 12 {
		t.Fatalf("legacy reading displaced or duplicated attributed history: %v", out.Billing)
	}
}

func TestBillingSeriesKeepsMonthBoundariesMissingTimestampsAndZeroDistinct(t *testing.T) {
	boundary := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC).Unix()
	var samples []historySample
	ages, stamps := map[string]float64{}, map[string]float64{}
	for i, value := range []float64{25000, 25001, 0, 5, 99999} {
		at := float64(boundary - 60 + int64(i)*60)
		sample := historySample{labels: map[string]string{}, ts: at, value: value}
		samples = append(samples, sample)
		if i < 4 { // Last sample has no trustworthy source timestamp.
			ages[sampleKey(sample, "cache")] = 0
			stamps[sampleKey(sample, "cache")] = at
		}
	}
	series := claudeBillingSeries(samples, ages, stamps, boundary-3600, boundary+3600, 600, "Claude usage cache")
	if len(series) != 3 || series[0].Period != "Month to date · Aug 2026" || series[1].Period != "Month to date · Sep 2026" {
		t.Fatalf("monthly boundary or downward reading joined: %v", series)
	}
	if len(series[2].Points) != 2 || series[2].Points[0].Value != 0 || series[2].Points[1].Value != 5 {
		t.Fatalf("measured zero lost, cumulative values summed, or unverified reading admitted: %v", series)
	}
}

func TestClaudeHistoryUsesExactAccountAndSourceTimeAndKeepsBillingCumulative(t *testing.T) {
	now := int64(1800000000)
	from := now - 3600
	times := []float64{float64(now - 900), float64(now - 600), float64(now - 300), float64(now)}
	mk := func(account, source, window string, values []float64) vmResult {
		r := vmResult{Metric: map[string]string{"account": account, "profile": "one", "source": source, "window": window}}
		for i, v := range values {
			r.Values = append(r.Values, [2]any{times[i], v})
		}
		return r
	}
	key := `{account="wanted"}`
	ranged := map[string]string{
		"claude_plan_window_pct" + key:                       successBody(t, mk("wanted", "cache", "five_hour", []float64{70, 80, 1, 2}), mk("other", "cache", "five_hour", []float64{99, 99, 99, 99})),
		"claude_plan_window_reset_timestamp_seconds" + key:   successBody(t, mk("wanted", "cache", "five_hour", []float64{float64(now - 500), float64(now - 500), float64(now + 500), float64(now + 500)})),
		"claude_plan_data_age_seconds" + key:                 successBody(t, mk("wanted", "cache", "", []float64{0, 0, 0, 300})),
		"timestamp(claude_plan_data_age_seconds" + key + ")": successBody(t, mk("wanted", "cache", "", times)),
		"claude_extra_usage_usd" + key:                       successBody(t, mk("wanted", "", "", []float64{5, 6, 1, 1})),
	}
	srv := httptest.NewServer(fakeVMHandler{ranged: ranged})
	defer srv.Close()
	out := &pb.GetAccountHistoryResponse{}
	loadClaudeHistory(context.Background(), Options{VM: &vm.Client{BaseURL: srv.URL, HTTP: srv.Client()}}, "wanted", from, now, out)
	if len(out.Windows) != 2 {
		t.Fatalf("reset periods not separated: %v", out.Windows)
	}
	points := 0
	for _, s := range out.Windows {
		for _, p := range s.Points {
			points++
			if p.Value == 99 || p.Ts == float64(now) {
				t.Fatalf("other account or stale repeated source stamped fresh: %v", p)
			}
		}
	}
	if points != 3 {
		t.Fatalf("duplicate observed timestamp retained: %d", points)
	}
	if len(out.Billing) != 2 || out.Billing[0].Points[0].Value != 5 || out.Billing[0].Points[1].Value != 6 || out.Billing[1].Points[0].Value != 1 {
		t.Fatalf("cumulative values/reset lost: %v", out.Billing)
	}
}
