package payg

import "testing"

func TestMergeKeepsTheLargerReadingPerBucket(t *testing.T) {
	old := []Bucket{
		{Day: "2026-09-01", Provider: "baseten", Model: "glm-5.3", Requests: 10, CostUSD: 5},
		{Day: "2026-09-02", Provider: "baseten", Model: "glm-5.3", Requests: 4, CostUSD: 2},
	}
	// Day one's log was deleted; day two grew.
	fresh := []Bucket{
		{Day: "2026-09-02", Provider: "baseten", Model: "glm-5.3", Requests: 6, CostUSD: 3},
		{Day: "2026-09-03", Provider: "fireworks", Model: "glm-5.3-flash", Requests: 1, CostUSD: 0.01},
	}
	got := Merge(old, fresh)
	if len(got) != 3 {
		t.Fatalf("buckets = %d, want 3", len(got))
	}
	if got[0].Requests != 10 || got[1].Requests != 6 || got[2].Provider != "fireworks" {
		t.Fatalf("merged = %+v", got)
	}
}
