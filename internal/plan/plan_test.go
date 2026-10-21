package plan

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

var errQueryFailed = errors.New("query failed")

// ages is a realistic pair: the tap updates on every statusline render, so
// it is far fresher than the /usage cache.
var ages = map[string]float64{"cache": 24438, "statusline": 1563}

func valid(window, source string, value float64) Sample {
	return Sample{Window: window, Source: source, Value: value, Valid: true}
}

// Ported from "pickFreshest takes each window from the fresher source": the
// live case that produced "resetting now", where the cache still described a
// window that had ended, and its reset time was the larger of the two.
func TestPickFreshestFresherSourceWins(t *testing.T) {
	samples := []Sample{
		valid("five_hour", "cache", 1786444799),
		valid("five_hour", "statusline", 1786462800),
	}
	got := PickFreshest(samples, ages)
	if got["five_hour"].Value != 1786462800 {
		t.Errorf("five_hour = %v, want 1786462800", got["five_hour"].Value)
	}
}

// Ported from "pickFreshest prefers the fresher source even when its value
// is smaller": max() would answer 89 here; freshness answers 2, which is the
// point.
func TestPickFreshestPrefersFresherEvenIfSmaller(t *testing.T) {
	samples := []Sample{
		valid("five_hour", "cache", 89),
		valid("five_hour", "statusline", 2),
	}
	got := PickFreshest(samples, ages)
	if got["five_hour"].Value != 2 {
		t.Errorf("five_hour = %v, want 2", got["five_hour"].Value)
	}
}

// Ported from "pickFreshest keeps a window only one source reports":
// weekly_scoped is never in the statusline payload.
func TestPickFreshestKeepsSingleSourceWindow(t *testing.T) {
	samples := []Sample{
		valid("weekly_scoped", "cache", 3),
		valid("five_hour", "statusline", 97),
	}
	got := PickFreshest(samples, ages)
	if got["weekly_scoped"].Value != 3 {
		t.Errorf("weekly_scoped = %v, want 3", got["weekly_scoped"].Value)
	}
	if got["five_hour"].Value != 97 {
		t.Errorf("five_hour = %v, want 97", got["five_hour"].Value)
	}
}

// Ported from "pickFreshest distrusts a sample whose source has no age": an
// unlabelled sample is a leftover series from an older poller, not a fresh
// reading, so it loses to any source with a known age and is used only when
// nothing else reports that window.
func TestPickFreshestDistrustsUnknownAge(t *testing.T) {
	samples := []Sample{
		valid("five_hour", "", 1),
		valid("five_hour", "cache", 2),
		valid("seven_day", "", 3),
	}
	got := PickFreshest(samples, ages)
	if got["five_hour"].Value != 2 {
		t.Errorf("five_hour = %v, want 2", got["five_hour"].Value)
	}
	if got["seven_day"].Value != 3 {
		t.Errorf("seven_day = %v, want 3", got["seven_day"].Value)
	}
}

// Ported from "pickFreshest drops non-numeric values and is stable on ties".
func TestPickFreshestDropsInvalidAndBreaksTiesOnSourceName(t *testing.T) {
	tied := map[string]float64{"cache": 10, "statusline": 10}
	samples := []Sample{
		valid("five_hour", "statusline", 5),
		valid("five_hour", "cache", 6),
		{Window: "seven_day", Source: "cache", Valid: false},
	}
	got := PickFreshest(samples, tied)
	if got["five_hour"].Value != 6 {
		t.Errorf("five_hour = %v, want 6", got["five_hour"].Value)
	}
	if _, ok := got["seven_day"]; ok {
		t.Errorf("seven_day: want absent (dropped as invalid), got present")
	}
}

// PickFreshest must drop a sample whose value parsed to Infinity, matching
// plan.js's `if (!Number.isFinite(s.value)) continue`: an Inf value is just
// as unusable as one that never parsed, so the finite cache sample must win
// even though it is the only one with a known, non-infinite value.
func TestPickFreshestDropsInfiniteValue(t *testing.T) {
	samples := []Sample{
		{Window: "five_hour", Source: "statusline", Value: math.Inf(1), Valid: true},
		valid("five_hour", "cache", 42),
	}
	got := PickFreshest(samples, ages)
	if got["five_hour"].Value != 42 || got["five_hour"].Source != "cache" {
		t.Errorf("five_hour = %+v, want value 42 from cache", got["five_hour"])
	}
}

// sampleValue/seriesToSamples is the real parsing path a VM response goes
// through (the PickFreshest tests above all construct Sample directly and
// never exercise it). Drive it with the malformed strings VM's JSON can
// actually contain.
func TestSampleValueParsesOrRejects(t *testing.T) {
	cases := []struct {
		name    string
		value   []any
		wantOK  bool
		wantVal float64
	}{
		{"clean float string parses", []any{float64(0), "42.5"}, true, 42.5},
		{"NaN string is invalid", []any{float64(0), "NaN"}, false, 0},
		{"non-numeric string is invalid", []any{float64(0), "abc"}, false, 0},
		{"a value slice shorter than 2 elements is invalid", []any{float64(0)}, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := vm.Series{Metric: map[string]string{"window": "five_hour", "source": "cache"}, Value: c.value}
			v, ok := sampleValue(s)
			if ok != c.wantOK {
				t.Fatalf("sampleValue(%v) ok = %v, want %v", c.value, ok, c.wantOK)
			}
			if ok && v != c.wantVal {
				t.Errorf("sampleValue(%v) = %v, want %v", c.value, v, c.wantVal)
			}

			samples := seriesToSamples([]vm.Series{s})
			if len(samples) != 1 {
				t.Fatalf("seriesToSamples returned %d samples, want 1", len(samples))
			}
			if samples[0].Valid != c.wantOK {
				t.Errorf("seriesToSamples(%v).Valid = %v, want %v", c.value, samples[0].Valid, c.wantOK)
			}
			if c.wantOK && samples[0].Value != c.wantVal {
				t.Errorf("seriesToSamples(%v).Value = %v, want %v", c.value, samples[0].Value, c.wantVal)
			}
		})
	}
}

// fakeQuerier answers each of the three verbatim queries LoadWindows issues
// with a canned response, keyed by the exact query string.
type fakeQuerier struct {
	responses map[string][]vm.Series
}

func (f fakeQuerier) Q(_ context.Context, query string) ([]vm.Series, error) {
	return f.responses[query], nil
}

func series(window, source, value string) vm.Series {
	return vm.Series{
		Metric: map[string]string{"window": window, "source": source},
		Value:  []any{float64(0), value},
	}
}

func ageSeries(source, value string) vm.Series {
	return vm.Series{
		Metric: map[string]string{"source": source},
		Value:  []any{float64(0), value},
	}
}

func TestLoadWindowsOrdersWindowsAndResolvesAges(t *testing.T) {
	q := fakeQuerier{responses: map[string][]vm.Series{
		pctQuery: {
			series("five_hour", "cache", "89"),
			series("five_hour", "statusline", "2"),
			series("seven_day", "cache", "50"),
			series("weekly_scoped", "cache", "3"),
			series("alpha_window", "cache", "1"),
		},
		resetQuery: {
			series("five_hour", "cache", "1786444799"),
			series("five_hour", "statusline", "1786462800"),
		},
		ageQuery: {
			ageSeries("cache", "24438"),
			ageSeries("statusline", "1563"),
		},
	}}

	windows, ages, err := LoadWindows(context.Background(), q)
	if err != nil {
		t.Fatalf("LoadWindows: %v", err)
	}
	if ages["cache"] != 24438 || ages["statusline"] != 1563 {
		t.Errorf("ages = %v, want cache=24438 statusline=1563", ages)
	}

	var names []string
	for _, w := range windows {
		names = append(names, w.Name)
	}
	want := []string{"five_hour", "seven_day", "weekly_scoped", "alpha_window"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("window order = %v, want %v", names, want)
	}

	byName := map[string]Window{}
	for _, w := range windows {
		byName[w.Name] = w
	}

	fh := byName["five_hour"]
	if !fh.HasPct || fh.Pct != 2 || fh.PctSource != "statusline" {
		t.Errorf("five_hour pct = %+v, want value 2 from statusline", fh)
	}
	if !fh.HasReset || fh.ResetTS != 1786462800 || fh.ResetSource != "statusline" {
		t.Errorf("five_hour reset = %+v, want value 1786462800 from statusline", fh)
	}

	ws := byName["weekly_scoped"]
	if !ws.HasPct || ws.Pct != 3 {
		t.Errorf("weekly_scoped pct = %+v, want value 3", ws)
	}
	if ws.HasReset {
		t.Errorf("weekly_scoped reset = %+v, want HasReset false", ws)
	}
}

// A pct query failure fails the whole call: without a percentage there is
// nothing left to show for any window.
func TestLoadWindowsFailsWhenPctQueryFails(t *testing.T) {
	q := failingQuerier{fail: pctQuery}
	if _, _, err := LoadWindows(context.Background(), q); err == nil {
		t.Fatal("want error when pct query fails, got nil")
	}
}

// A reset or age query failure degrades to empty rather than failing the
// call: a missing reset time or age is less informative, not fatal.
func TestLoadWindowsDegradesWhenResetOrAgeQueryFails(t *testing.T) {
	t.Run("reset query fails", func(t *testing.T) {
		q := failingQuerier{
			fail: resetQuery,
			ok: map[string][]vm.Series{
				pctQuery: {series("five_hour", "cache", "50")},
				ageQuery: {ageSeries("cache", "24438")},
			},
		}
		windows, ages, err := LoadWindows(context.Background(), q)
		if err != nil {
			t.Fatalf("LoadWindows: %v", err)
		}
		if len(windows) != 1 || windows[0].HasReset {
			t.Errorf("windows = %+v, want one window with no reset", windows)
		}
		if ages["cache"] != 24438 {
			t.Errorf("ages = %v, want cache=24438", ages)
		}
	})

	t.Run("age query fails", func(t *testing.T) {
		q := failingQuerier{
			fail: ageQuery,
			ok: map[string][]vm.Series{
				pctQuery:   {series("five_hour", "cache", "50")},
				resetQuery: {series("five_hour", "cache", "1786444799")},
			},
		}
		windows, ages, err := LoadWindows(context.Background(), q)
		if err != nil {
			t.Fatalf("LoadWindows: %v", err)
		}
		if len(windows) != 1 || !windows[0].HasPct || !windows[0].HasReset {
			t.Errorf("windows = %+v, want one window with pct and reset intact", windows)
		}
		if len(ages) != 0 {
			t.Errorf("ages = %v, want empty", ages)
		}
	})
}

type failingQuerier struct {
	fail string
	ok   map[string][]vm.Series
}

func (f failingQuerier) Q(_ context.Context, query string) ([]vm.Series, error) {
	if query == f.fail {
		return nil, errQueryFailed
	}
	return f.ok[query], nil
}
