package pricing

import (
	"strings"
)

// WebSearchCost is the cost per web search request.
const WebSearchCost = 0.01

// CacheReadX is the multiplier for cache read input tokens (0.1x input rate),
// and the default for every model CACHE_READ_X does not override.
const CacheReadX = 0.1

// CACHE_READ_X overrides CacheReadX per model.
//
// The multiplier is a property of the model, not a constant: claude-fable-5-1
// reads cached input at $0.25 per million against a $10 input rate, which is
// 0.025x rather than the 0.1x every other model charges. Applying the global
// constant to it overprices cache reads fourfold, and cache reads are the
// largest token category in an agentic run by a wide margin (one measured
// analysis run on this stack read 5.57M cached tokens against 92k output), so
// this is not a rounding difference.
//
// Cache WRITES are unaffected: Fable 5.1 charges the same 1.25x and 2.0x as
// everything else, so CacheWrite5mX and CacheWrite1hX stay global constants.
var CACHE_READ_X = map[string]float64{
	"claude-fable-5-1": 0.025,
}

// CacheReadXOf returns the cache read multiplier for a model, falling back to
// CacheReadX. The model is expected to be normalized (NormModel) already,
// which is what every caller of RequestCost passes.
func CacheReadXOf(model string) float64 {
	if x, ok := CACHE_READ_X[model]; ok {
		return x
	}
	return CacheReadX
}

// CacheWrite5mX is the multiplier for 5-minute cache write input tokens (1.25x input rate).
const CacheWrite5mX = 1.25

// CacheWrite1hX is the multiplier for 1-hour cache write input tokens (2.0x input rate).
const CacheWrite1hX = 2.0

// PRICES is the per-model-token pricing table (input, output) in USD per million tokens.
var PRICES = map[string][2]float64{
	// Fable 5.1 lists at Fable 5's input and output rates. It differs only in
	// cache reads, which CACHE_READ_X carries, and it has no fast tier: fast
	// mode is an Opus 5 / Opus 4.8 research preview, so there is deliberately
	// no FAST_PRICES entry below.
	"claude-fable-5-1":      {10.0, 50.0},
	"claude-fable-5":        {10.0, 50.0},
	"claude-mythos-5":       {10.0, 50.0},
	"claude-mythos-preview": {10.0, 50.0},
	"claude-opus-5":         {5.0, 25.0},
	"claude-opus-4-8":       {5.0, 25.0},
	"claude-opus-4-7":       {5.0, 25.0},
	"claude-opus-4-6":       {5.0, 25.0},
	"claude-opus-4-5":       {5.0, 25.0},
	"claude-opus-4-1":       {5.0, 25.0},
	"claude-opus-4-0":       {5.0, 25.0},
	"claude-sonnet-5":       {2.0, 10.0},
	"claude-sonnet-4-6":     {3.0, 15.0},
	"claude-sonnet-4-5":     {3.0, 15.0},
	"claude-haiku-4-5":      {1.0, 5.0},
}

// FAST_PRICES overrides PRICES for specific models when speed == "fast".
var FAST_PRICES = map[string][2]float64{
	"claude-opus-5":   {10.0, 50.0},
	"claude-opus-4-8": {10.0, 50.0},
}

// NormModel returns a normalized model name by stripping the trailing [1m]
// context suffix and the trailing YYYYMMDD date, but keeping the "claude-" prefix.
// Examples: "claude-opus-5[1m]" -> "claude-opus-5", "claude-sonnet-5-20250929" -> "claude-sonnet-5".
func NormModel(name string) string {
	m := name
	// Strip [1m] context suffix if present
	if strings.HasSuffix(m, "[1m]") {
		m = m[:len(m)-4]
	}
	// Strip trailing -YYYYMMDD date
	parts := strings.Split(m, "-")
	if len(parts) >= 2 {
		last := parts[len(parts)-1]
		if len(last) == 8 && isAllDigits(last) {
			m = strings.Join(parts[:len(parts)-1], "-")
		}
	}
	return m
}

// isAllDigits checks if a string contains only digit characters.
func isAllDigits(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Prices returns the input and output token prices (in USD per million tokens) for a given model and speed.
// If the model is not recognized, it returns (0, 0, false).
// If speed is "fast" and the model has a FAST_PRICES override, that is used instead.
func Prices(model, speed string) (in, out float64, ok bool) {
	var prices [2]float64
	var found bool

	// Check for fast-speed override first.
	if speed == "fast" {
		prices, found = FAST_PRICES[model]
		if found {
			return prices[0], prices[1], true
		}
	}

	// Fall back to regular prices.
	prices, found = PRICES[model]
	if !found {
		return 0.0, 0.0, false
	}
	return prices[0], prices[1], true
}

// RequestCost calculates the cost of an API request.
// Parameters:
//   - model: the normalized model name (e.g., "claude-sonnet-5"); the cache
//     read multiplier is looked up from it, so an un-normalized name would
//     also miss its own rate
//   - tin: input tokens
//   - tout: output tokens
//   - cr: cache read input tokens
//   - c5: 5-minute ephemeral cache write input tokens
//   - c1: 1-hour ephemeral cache write input tokens
//   - ws: web search requests
//   - speed: speed tier (e.g., "fast")
//   - tier: service tier (e.g., "batch")
//
// Cost formula: (tin*rin + cr*crx*rin + c5*1.25*rin + c1*2.0*rin)/1e6 + tout*rout/1e6 + ws*0.01,
// where crx is CacheReadXOf(model): 0.1 for every model but claude-fable-5-1.
// If tier == "batch", the total cost is halved.
// For unpriced models, rin and rout are 0 but ws cost is still included.
func RequestCost(model string, tin, tout, cr, c5, c1, ws int64, speed, tier string) (cost float64, priced bool) {
	rin, rout, ok := Prices(model, speed)

	// Calculate token costs in USD.
	// Input costs include: base input, cache reads, and cache writes.
	// Each cache category is weighted by its respective multiplier.
	inputCost := float64(tin)*rin + float64(cr)*CacheReadXOf(model)*rin + float64(c5)*CacheWrite5mX*rin + float64(c1)*CacheWrite1hX*rin
	inputCost /= 1e6

	// Output costs.
	outputCost := float64(tout) * rout / 1e6

	// Web search costs.
	searchCost := float64(ws) * WebSearchCost

	cost = inputCost + outputCost + searchCost

	// Apply batch tier discount (50% off).
	if tier == "batch" {
		cost *= 0.5
	}

	return cost, ok
}
