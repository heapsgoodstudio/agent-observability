package pricing

import (
	"math"
	"testing"
)

func TestNormModel(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"strips context suffix", "claude-opus-5[1m]", "claude-opus-5"},
		{"strips date suffix", "claude-sonnet-5-20250929", "claude-sonnet-5"},
		{"keeps claude prefix", "claude-haiku-4-5", "claude-haiku-4-5"},
		{"no change needed", "claude-sonnet-5", "claude-sonnet-5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormModel(tc.input)
			if got != tc.expect {
				t.Errorf("NormModel(%q) = %q, want %q", tc.input, got, tc.expect)
			}
		})
	}
}

func TestPrices(t *testing.T) {
	tests := []struct {
		model  string
		speed  string
		expIn  float64
		expOut float64
		expOk  bool
	}{
		{"claude-sonnet-5", "", 2.0, 10.0, true},
		{"claude-haiku-4-5", "", 1.0, 5.0, true},
		{"claude-opus-5", "", 5.0, 25.0, true},
		{"claude-opus-5", "fast", 10.0, 50.0, true},
		{"claude-opus-4-8", "fast", 10.0, 50.0, true},
		{"claude-fable-5", "", 10.0, 50.0, true},
		{"claude-mythos-5", "", 10.0, 50.0, true},
		{"claude-mythos-preview", "", 10.0, 50.0, true},
		{"claude-opus-4-7", "", 5.0, 25.0, true},
		{"claude-opus-4-6", "", 5.0, 25.0, true},
		{"claude-opus-4-5", "", 5.0, 25.0, true},
		{"claude-opus-4-1", "", 5.0, 25.0, true},
		{"claude-opus-4-0", "", 5.0, 25.0, true},
		{"claude-sonnet-4-6", "", 3.0, 15.0, true},
		{"claude-sonnet-4-5", "", 3.0, 15.0, true},
		{"unknown-model", "", 0.0, 0.0, false},
	}
	for _, tc := range tests {
		t.Run(tc.model+":"+tc.speed, func(t *testing.T) {
			in, out, ok := Prices(tc.model, tc.speed)
			if ok != tc.expOk {
				t.Errorf("Prices(%q, %q) ok = %v, want %v", tc.model, tc.speed, ok, tc.expOk)
			}
			if ok && (in != tc.expIn || out != tc.expOut) {
				t.Errorf("Prices(%q, %q) = (%v, %v), want (%v, %v)", tc.model, tc.speed, in, out, tc.expIn, tc.expOut)
			}
		})
	}
}

func TestRequestCost(t *testing.T) {
	const epsilon = 1e-9

	tests := []struct {
		name   string
		model  string
		tin    int64
		tout   int64
		cr     int64
		c5     int64
		c1     int64
		ws     int64
		speed  string
		tier   string
		expect float64
	}{
		{"golden 1", "claude-sonnet-5", 100, 200, 1000, 50, 0, 0, "", "", 0.002525},
		{"golden 2", "claude-haiku-4-5", 10, 20, 0, 0, 0, 0, "", "", 0.00011},
		{"golden 3", "claude-sonnet-5", 5, 5, 0, 0, 0, 0, "", "", 0.00006},
		{"with web search", "claude-sonnet-5", 100, 200, 1000, 50, 0, 1, "", "", 0.002525 + 0.01},
		{"batch tier halving", "claude-sonnet-5", 100, 200, 1000, 50, 0, 0, "", "batch", 0.002525 * 0.5},
		{"batch + web search", "claude-sonnet-5", 100, 200, 1000, 50, 0, 2, "", "batch", (0.002525 + 0.02) * 0.5},
		{"fast speed opus-5", "claude-opus-5", 100, 100, 0, 0, 0, 0, "fast", "", (100*10.0 + 100*50.0) / 1e6},
		{"unpriced with ws", "unknown-model", 0, 0, 0, 0, 0, 5, "", "", 0.05},
		{"unpriced batch with ws", "unknown-model", 0, 0, 0, 0, 0, 5, "", "batch", 0.025},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cost, priced := RequestCost(tc.model, tc.tin, tc.tout, tc.cr, tc.c5, tc.c1, tc.ws, tc.speed, tc.tier)
			if math.Abs(cost-tc.expect) > epsilon {
				t.Errorf("RequestCost(%q, %d, %d, %d, %d, %d, %d, %q, %q) = %.9f, want %.9f",
					tc.model, tc.tin, tc.tout, tc.cr, tc.c5, tc.c1, tc.ws, tc.speed, tc.tier,
					cost, tc.expect)
			}
			// Check priced flag: unpriced models have "unknown-model" in the name
			isUnpriced := tc.model == "unknown-model"
			if isUnpriced && priced {
				t.Errorf("RequestCost for unpriced model should return priced=false, got true")
			}
			if !isUnpriced && !priced {
				t.Errorf("RequestCost for priced model should return priced=true, got false")
			}
		})
	}
}

func TestRequestCostSum(t *testing.T) {
	const epsilon = 1e-9

	// Verify the fixture's three requests sum to 0.002695
	cost1, _ := RequestCost("claude-sonnet-5", 100, 200, 1000, 50, 0, 0, "", "")
	cost2, _ := RequestCost("claude-haiku-4-5", 10, 20, 0, 0, 0, 0, "", "")
	cost3, _ := RequestCost("claude-sonnet-5", 5, 5, 0, 0, 0, 0, "", "")

	total := cost1 + cost2 + cost3
	expected := 0.002695

	if math.Abs(total-expected) > epsilon {
		t.Errorf("Sum of three golden costs = %.9f, want %.9f", total, expected)
	}
}

// Fable 5.1 is priced at Fable 5's rates for everything but cache reads,
// which bill at $0.25 per million rather than a tenth of input. The two
// models are otherwise interchangeable, so the pair is checked together:
// a change that moves one and not the other is the likely mistake.
func TestFable51IsPricedLikeFable5ExceptCacheReads(t *testing.T) {
	const epsilon = 1e-9

	in, out, ok := Prices("claude-fable-5-1", "")
	if !ok {
		t.Fatal("claude-fable-5-1 is not in the price table")
	}
	if in != 10.0 || out != 50.0 {
		t.Errorf("Prices(claude-fable-5-1) = (%v, %v), want (10, 50)", in, out)
	}

	// Fast mode is an Opus 5 / Opus 4.8 research preview and does not exist
	// on this model, so asking for it must fall through to the list rate
	// rather than find an override.
	fin, fout, _ := Prices("claude-fable-5-1", "fast")
	if fin != in || fout != out {
		t.Errorf("Prices(claude-fable-5-1, fast) = (%v, %v), want the list rate (%v, %v)", fin, fout, in, out)
	}

	// One million cache-read tokens and nothing else, so the figure IS the
	// per-million cache-read rate.
	got, _ := RequestCost("claude-fable-5-1", 0, 0, 1_000_000, 0, 0, 0, "", "")
	if math.Abs(got-0.25) > epsilon {
		t.Errorf("1M cache reads on claude-fable-5-1 = $%.6f, want $0.25", got)
	}
	// The control: the same million tokens on Fable 5, which reads at the
	// ordinary tenth of input. Without this the test above would pass just
	// as well against a table where every model had moved.
	base, _ := RequestCost("claude-fable-5", 0, 0, 1_000_000, 0, 0, 0, "", "")
	if math.Abs(base-1.00) > epsilon {
		t.Errorf("1M cache reads on claude-fable-5 = $%.6f, want $1.00", base)
	}

	// Cache WRITES are unchanged at 1.25x and 2.0x input, so only the read
	// multiplier moved.
	w5, _ := RequestCost("claude-fable-5-1", 0, 0, 0, 1_000_000, 0, 0, "", "")
	if math.Abs(w5-12.50) > epsilon {
		t.Errorf("1M 5m cache writes on claude-fable-5-1 = $%.6f, want $12.50", w5)
	}
	w1, _ := RequestCost("claude-fable-5-1", 0, 0, 0, 0, 1_000_000, 0, "", "")
	if math.Abs(w1-20.00) > epsilon {
		t.Errorf("1M 1h cache writes on claude-fable-5-1 = $%.6f, want $20.00", w1)
	}
}

// The context-suffixed and date-suffixed spellings both have to reach the
// same row, since the OTel counter reports the first and a transcript can
// report the second.
func TestNormModelReachesFable51(t *testing.T) {
	for _, raw := range []string{"claude-fable-5-1", "claude-fable-5-1[1m]", "claude-fable-5-1-20260401"} {
		if got := NormModel(raw); got != "claude-fable-5-1" {
			t.Errorf("NormModel(%q) = %q, want claude-fable-5-1", raw, got)
		}
		if _, _, ok := Prices(NormModel(raw), ""); !ok {
			t.Errorf("NormModel(%q) does not resolve to a priced model", raw)
		}
	}
	// Fable 5.1 must not collapse onto Fable 5: they are different models
	// with different cache-read rates.
	if NormModel("claude-fable-5-1") == NormModel("claude-fable-5") {
		t.Error("NormModel collapses claude-fable-5-1 onto claude-fable-5")
	}
}
