package overage

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestGroupBands(t *testing.T) {
	tests := []struct {
		name   string
		times  []float64
		maxGap float64
		want   [][2]float64
	}{
		{
			name:   "joins samples inside the gap and splits beyond it",
			times:  []float64{100, 200, 300},
			maxGap: 150,
			want:   [][2]float64{{100, 300}},
		},
		{
			name:   "splits when gap exceeds maxGap",
			times:  []float64{100, 200, 900},
			maxGap: 150,
			want:   [][2]float64{{100, 200}, {900, 900}},
		},
		{
			name:   "empty input",
			times:  []float64{},
			maxGap: 150,
			want:   [][2]float64{},
		},
		{
			name:   "single sample",
			times:  []float64{42},
			maxGap: 150,
			want:   [][2]float64{{42, 42}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GroupBands(tt.times, tt.maxGap)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("GroupBands diff:\n%s", diff)
			}
		})
	}
}

func TestSpanOverlapsBands(t *testing.T) {
	tests := []struct {
		name  string
		start float64
		end   float64
		bands [][2]float64
		want  bool
	}{
		{
			name:  "inside",
			start: 150,
			end:   160,
			bands: [][2]float64{{100, 200}, {400, 500}},
			want:  true,
		},
		{
			name:  "overlaps the start",
			start: 50,
			end:   120,
			bands: [][2]float64{{100, 200}, {400, 500}},
			want:  true,
		},
		{
			name:  "overlaps the end",
			start: 480,
			end:   900,
			bands: [][2]float64{{100, 200}, {400, 500}},
			want:  true,
		},
		{
			name:  "in the gap",
			start: 250,
			end:   300,
			bands: [][2]float64{{100, 200}, {400, 500}},
			want:  false,
		},
		{
			name:  "no bands at all",
			start: 250,
			end:   300,
			bands: [][2]float64{},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SpanOverlapsBands(tt.start, tt.end, tt.bands)
			if got != tt.want {
				t.Errorf("SpanOverlapsBands(%v, %v, %v) = %v, want %v",
					tt.start, tt.end, tt.bands, got, tt.want)
			}
		})
	}
}

func TestCostInBands(t *testing.T) {
	tests := []struct {
		name       string
		trajectory [][3]float64
		bands      [][2]float64
		want       float64
	}{
		{
			name:       "charges each rise to the timestamp it landed on",
			trajectory: [][3]float64{{100, 1, 0}, {150, 3, 0}, {450, 9, 0}, {460, 10, 0}},
			bands:      [][2]float64{{400, 500}},
			want:       7,
		},
		{
			name:       "counts the first point's cost from zero",
			trajectory: [][3]float64{{450, 4, 0}},
			bands:      [][2]float64{{400, 500}},
			want:       4,
		},
		{
			name:       "ignores a falling trajectory",
			trajectory: [][3]float64{{450, 4, 0}, {460, 3, 0}},
			bands:      [][2]float64{{400, 500}},
			want:       4,
		},
		{
			name:       "empty input",
			trajectory: [][3]float64{},
			bands:      [][2]float64{{400, 500}},
			want:       0,
		},
		{
			name:       "empty bands",
			trajectory: [][3]float64{{450, 4, 0}},
			bands:      [][2]float64{},
			want:       0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CostInBands(tt.trajectory, tt.bands)
			if got != tt.want {
				t.Errorf("CostInBands(...) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMaskSeries(t *testing.T) {
	// Define a simple Point type for testing since we're testing before
	// the full interface is available
	type Point struct {
		TS float64
		V  float64
	}

	tests := []struct {
		name   string
		points [][2]float64
		bands  [][2]float64
		want   [][2]float64
	}{
		{
			name:   "keeps only samples that landed inside a band",
			points: [][2]float64{{100, 1}, {250, 2}, {450, 3}, {600, 4}},
			bands:  [][2]float64{{400, 500}},
			want:   [][2]float64{{450, 3}},
		},
		{
			name:   "includes all samples when band covers entire range",
			points: [][2]float64{{100, 1}, {250, 2}, {450, 3}, {600, 4}},
			bands:  [][2]float64{{0, 1000}},
			want:   [][2]float64{{100, 1}, {250, 2}, {450, 3}, {600, 4}},
		},
		{
			name:   "treats band edges as inclusive",
			points: [][2]float64{{400, 1}, {500, 2}, {501, 3}},
			bands:  [][2]float64{{400, 500}},
			want:   [][2]float64{{400, 1}, {500, 2}},
		},
		{
			name:   "returns nothing when there are no bands",
			points: [][2]float64{{100, 1}},
			bands:  [][2]float64{},
			want:   [][2]float64{},
		},
		{
			name:   "spans multiple bands",
			points: [][2]float64{{50, 1}, {150, 2}, {250, 3}, {450, 4}},
			bands:  [][2]float64{{100, 200}, {400, 500}},
			want:   [][2]float64{{150, 2}, {450, 4}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MaskSeries(tt.points, tt.bands)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("MaskSeries diff:\n%s", diff)
			}
		})
	}
}
