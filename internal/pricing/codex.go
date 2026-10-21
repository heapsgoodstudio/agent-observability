package pricing

// CodexEstimate returns the current public API-equivalent token estimate, not
// actual billed spend or subscription usage. Missing service-tier information
// uses Standard rates; discounts, tool fees, and regional uplifts are excluded.
// Rates verified 2026-09-22: https://developers.openai.com/api/docs/pricing
// Long-context threshold: https://developers.openai.com/api/docs/models/gpt-6-astra
// Unknown models (including internal auto-review models) are never free usage.
// Input encompasses cached input and cache writes. Output encompasses reasoning.
func CodexEstimate(model string, input, output, cached, write int64) (float64, bool) {
	// All figures are dollars per million tokens, in/cached/write/out order.
	var rate [4]float64
	long := false
	switch model {
	case "gpt-6-astra":
		rate = [4]float64{10, 1, 12.5, 50}
		long = true
	case "gpt-5.6-sol":
		rate = [4]float64{4, .4, 5, 20}
		long = true
	case "gpt-5.6-terra":
		rate = [4]float64{2, .2, 2.5, 12}
		long = true
	case "gpt-5.6-luna":
		rate = [4]float64{.2, .02, .25, 1.2}
		long = true
	case "gpt-5.3-codex":
		rate = [4]float64{1.75, .175, 0, 14}
	case "gpt-5-codex":
		rate = [4]float64{1.25, .125, 0, 10}
	default:
		return 0, false
	}
	if input < 0 || output < 0 || cached < 0 || write < 0 || cached > input || write > input-cached {
		return 0, false
	}
	if write > 0 && rate[2] == 0 {
		return 0, false
	}
	if long && input > 272000 {
		rate[0] *= 2
		rate[1] *= 2
		rate[2] *= 2
		rate[3] *= 1.5
	}
	return (float64(input-cached-write)*rate[0] + float64(cached)*rate[1] + float64(write)*rate[2] + float64(output)*rate[3]) / 1e6, true
}
