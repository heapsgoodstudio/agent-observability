package server

import (
	"math"
	"testing"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/payg"
)

func TestBuildPaygGroupsByProviderAndListsFreeProvidersByTokens(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.Local)
	buckets := []payg.Bucket{
		{Day: "2026-09-01", Provider: "baseten", Model: "glm-5.3", Requests: 10, Input: 100, Output: 20, CacheRead: 1000, CostUSD: 6.49, CostKnown: true},
		{Day: "2026-09-02", Provider: "baseten", Model: "glm-5.3-flash", Requests: 30, Input: 300, Output: 10, CostUSD: 3.19, CostKnown: true},
		// Last month: outside month to date, inside the 30 day series.
		{Day: "2026-08-30", Provider: "baseten", Model: "glm-5.3", Requests: 1, CostUSD: 1, CostKnown: true},
		{Day: "2026-09-22", Provider: "fireworks", Model: "glm-5.3-flash", Requests: 1, Input: 23500, Output: 14, CostUSD: 0.003532, CostKnown: true},
		// A local model: requests but every one free. Listed, with no cost
		// recorded, so its tokens are its figure.
		{Day: "2026-09-10", Provider: "lm-studio", Model: "qwen3.8-27b", Requests: 5, Input: 5000, CostUSD: 0, CostKnown: true},
	}
	month, got := buildPayg(buckets, now)
	if month != "2026-09" {
		t.Fatalf("month = %q", month)
	}
	if len(got) != 3 {
		t.Fatalf("providers = %d, want baseten, fireworks and lm-studio", len(got))
	}
	b, f, l := got[0], got[1], got[2]
	if b.Provider != "baseten" || f.Provider != "fireworks" || l.Provider != "lm-studio" {
		t.Fatalf("order = %s, %s, %s; want the larger spend first, then by tokens", b.Provider, f.Provider, l.Provider)
	}
	if !b.CostRecorded || !f.CostRecorded || l.CostRecorded {
		t.Fatalf("cost recorded = %v %v %v; only the free provider has none", b.CostRecorded, f.CostRecorded, l.CostRecorded)
	}
	if l.MonthToDateTokens != 5000 || l.Last_30DaysTokens != 5000 || l.MonthToDateUsd != 0 || len(l.Models) != 1 {
		t.Fatalf("lm-studio = %+v", l)
	}
	if math.Abs(b.MonthToDateUsd-9.68) > 1e-9 || !b.MonthToDateComplete {
		t.Fatalf("baseten month to date = %v complete=%v", b.MonthToDateUsd, b.MonthToDateComplete)
	}
	if math.Abs(b.Last_30DaysUsd-10.68) > 1e-9 {
		t.Fatalf("baseten 30 days = %v, want August's dollar included", b.Last_30DaysUsd)
	}
	if len(b.Days) != paygDays || b.Days[0].Day != "2026-08-24" || b.Days[paygDays-1].Day != "2026-09-22" {
		t.Fatalf("days span %s..%s (%d)", b.Days[0].Day, b.Days[len(b.Days)-1].Day, len(b.Days))
	}
	if len(b.Models) != 2 || b.Models[0].Model != "glm-5.3" || b.Models[0].CostUsd != 6.49 {
		t.Fatalf("baseten models = %+v", b.Models)
	}
	if b.LastActiveDay != "2026-09-02" || f.LastActiveDay != "2026-09-22" {
		t.Fatalf("last active = %s, %s", b.LastActiveDay, f.LastActiveDay)
	}
}

func TestBuildPaygListsUnpricedUsageAndMarksItIncomplete(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.Local)
	_, got := buildPayg([]payg.Bucket{
		{Day: "2026-09-05", Provider: "fireworks", Model: "new-model", Requests: 3, CostUSD: 0, CostKnown: false},
	}, now)
	if len(got) != 1 {
		t.Fatalf("an unpriced request is pay-as-you-go usage of unknown cost; got %d providers", len(got))
	}
	if got[0].MonthToDateComplete || got[0].Models[0].CostComplete {
		t.Fatal("unpriced usage must not read as a complete $0")
	}
}
