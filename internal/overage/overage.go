// Package overage implements overage band arithmetic for tracking when plan
// windows were exhausted and usage was billed at API rates.
package overage

import (
	"context"
	"math"
	"strconv"

	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// GroupBands groups sorted timestamps into contiguous bands, breaking
// whenever the gap exceeds maxGap seconds. Input must be sorted ascending.
func GroupBands(times []float64, maxGap float64) [][2]float64 {
	if len(times) == 0 {
		return [][2]float64{}
	}

	out := make([][2]float64, 0, len(times))
	for _, t := range times {
		if len(out) > 0 && t-out[len(out)-1][1] <= maxGap {
			out[len(out)-1][1] = t
		} else {
			out = append(out, [2]float64{t, t})
		}
	}
	return out
}

// SpanOverlapsBands returns true if the span [start, end] overlaps with any
// band. Band edges are inclusive.
func SpanOverlapsBands(start, end float64, bands [][2]float64) bool {
	for _, band := range bands {
		if start <= band[1] && end >= band[0] {
			return true
		}
	}
	return false
}

// CostInBands calculates the sum of cost deltas for a trajectory that landed
// inside bands. Trajectory is [epoch, cumulative_cost_usd, context_tokens] rows.
// Each row's cost is its rise since the previous row, charged to its own
// timestamp; the first row is charged in full.
func CostInBands(trajectory [][3]float64, bands [][2]float64) float64 {
	var sum float64
	var prev float64
	for _, row := range trajectory {
		t := row[0]
		cum := row[1]
		delta := cum - prev
		prev = cum
		if delta > 0 && SpanOverlapsBands(t, t, bands) {
			sum += delta
		}
	}
	return sum
}

// MaskSeries filters points to only those that fall within any band.
// Band edges are inclusive.
func MaskSeries(points [][2]float64, bands [][2]float64) [][2]float64 {
	if len(bands) == 0 {
		return [][2]float64{}
	}

	result := make([][2]float64, 0, len(points))
	for _, point := range points {
		t := point[0]
		if SpanOverlapsBands(t, t, bands) {
			result = append(result, point)
		}
	}
	return result
}

// Querier is a minimal interface for instant queries.
type Querier interface {
	Q(ctx context.Context, query string) ([]vm.Series, error)
}

// RangeQuerier is a minimal interface for range queries.
type RangeQuerier interface {
	QR(ctx context.Context, query string, start, end int64, step string) ([]vm.Series, error)
}

// Load queries VictoriaMetrics for overage bands over the trailing 45 days
// and returns the bands along with the series' true start timestamp.
// It issues two queries:
// - Range query: max(claude_in_overage) == 1 over 45d step 10m
// - Instant query: min_over_time(timestamp(claude_in_overage)[45d:10m])
func Load(ctx context.Context, q Querier, qr RangeQuerier, now int64) (bands [][2]float64, seriesStart float64, hasStart bool, err error) {
	const (
		dayS        = 86400
		overageStepS = 600
	)

	start := now - 45*dayS

	// Range query for overage samples
	res, err := qr.QR(ctx, "max(claude_in_overage) == 1", start, now, "10m")
	if err != nil {
		return nil, 0, false, err
	}

	// Extract timestamps from the range query results
	times := make([]float64, 0)
	if len(res) > 0 {
		for _, v := range res[0].Values {
			ts, ok := toFloat(v[0])
			if ok && !math.IsNaN(ts) {
				times = append(times, ts)
			}
		}
	}

	// Group into bands with maxGap = 1200 (2 * OVERAGE_STEP_S)
	bands = GroupBands(times, overageStepS*2)

	// Instant query for series start
	firstRes, err := q.Q(ctx, "min_over_time(timestamp(claude_in_overage)[45d:10m])")
	if err != nil {
		return bands, 0, false, err
	}

	if len(firstRes) > 0 && len(firstRes[0].Value) >= 2 {
		val, ok := toFloat(firstRes[0].Value[1])
		if ok && !math.IsNaN(val) {
			return bands, val, true, nil
		}
	}

	return bands, 0, false, nil
}

// toFloat converts a VM value (which arrives as float64 or string) to float64.
func toFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}
