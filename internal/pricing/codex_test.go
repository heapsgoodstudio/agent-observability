package pricing

import (
	"math"
	"testing"
)

func TestCodexEstimatePartitionsInput(t *testing.T) {
	cost, known := CodexEstimate("gpt-6-astra", 1000, 200, 500, 100)
	want := (400.0*10 + 500*1 + 100*12.5 + 200*50) / 1e6
	if !known || math.Abs(cost-want) > 1e-10 {
		t.Fatalf("got %v,%v want %v", cost, known, want)
	}
}
func TestCodexEstimateUnknownAndInvalid(t *testing.T) {
	for _, tc := range []struct {
		m          string
		i, o, c, w int64
	}{{"codex-auto-review", 10, 2, 0, 0}, {"gpt-6", 10, 2, 0, 0}, {"gpt-6-astra", 10, 2, 11, 0}, {"gpt-5-codex", 10, 2, 0, 1}} {
		if c, k := CodexEstimate(tc.m, tc.i, tc.o, tc.c, tc.w); k || c != 0 {
			t.Fatalf("unknown priced %+v", tc)
		}
	}
}
func TestCodexEstimateLongContext(t *testing.T) {
	c, k := CodexEstimate("gpt-6-astra", 272001, 100, 72001, 0)
	want := (200000.0*20 + 72001*2 + 100*75) / 1e6
	if !k || math.Abs(c-want) > 1e-9 {
		t.Fatalf("got %f want %f", c, want)
	}
}
