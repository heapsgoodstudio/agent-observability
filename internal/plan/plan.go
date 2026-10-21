// Package plan resolves plan-window freshness between the two sources that
// report the same Claude Code plan windows. PickFreshest is pure: no HTTP, no
// module state. LoadWindows is the one impure entry point, running the three
// queries and feeding their results through PickFreshest.
package plan

import (
	"context"
	"math"
	"sort"
	"strconv"
	"sync"

	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// unknownAge is the rank given to a sample whose source has no reported age.
// It is not trusted over any source with a known age, however old: an
// unlabelled sample is a leftover series from an older poller, not a fresh
// reading.
var unknownAge = math.Inf(1)

// Sample is one window/source/value reading, as written by the plan-window
// poller under two sources: "statusline" and "cache". Valid false models a
// value that failed to parse as a number (JS NaN); PickFreshest drops it
// rather than comparing it.
type Sample struct {
	Window string
	Source string
	Value  float64
	Valid  bool
}

// Picked is the winning sample for one window: its value and which source it
// came from.
type Picked struct {
	Value  float64
	Source string
}

// PickFreshest chooses, per window, the value from whichever source has the
// lowest reported age in ages (seconds since that source last updated).
//
// The plan-window poller writes each window twice. The statusline tap
// updates on every statusline render, so it is within a minute or so of the
// truth. The /usage cache only changes when Claude Code refetches it, which
// can be hours stale and can still describe a window that has already
// ended. Collapsing the pair with max() answers "which number is larger",
// not "which is true": it looked right for percentages (a stale window
// usually reads lower) and was wrong for reset times, where the larger value
// was whichever source last saw a boundary, so an ended window kept reading
// "resetting now" until the cache caught up. Freshness is the only
// defensible tie-break, applied per window since not every window appears in
// every source (weekly_scoped exists only in the cache). Ties break on the
// lexically smaller source name, purely for determinism.
func PickFreshest(samples []Sample, ages map[string]float64) map[string]Picked {
	type ranked struct {
		Picked
		rank float64
	}
	best := map[string]ranked{}
	for _, s := range samples {
		// Matches plan.js's `if (!Number.isFinite(s.value)) continue`: a
		// value that parsed to Inf is just as unusable as one that never
		// parsed at all, so it is dropped here rather than allowed to win a
		// window on rank alone.
		if !s.Valid || math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
			continue
		}
		rank := unknownAge
		if age, ok := ages[s.Source]; ok && !math.IsNaN(age) && !math.IsInf(age, 0) {
			rank = age
		}
		cur, ok := best[s.Window]
		if !ok || rank < cur.rank || (rank == cur.rank && s.Source < cur.Source) {
			best[s.Window] = ranked{Picked{Value: s.Value, Source: s.Source}, rank}
		}
	}
	out := make(map[string]Picked, len(best))
	for w, b := range best {
		out[w] = b.Picked
	}
	return out
}

// Window is one resolved plan window, combining the freshest percentage and
// reset-timestamp samples that name it. The two need not come from the same
// source.
type Window struct {
	Name        string
	Pct         float64
	HasPct      bool
	ResetTS     float64
	HasReset    bool
	PctSource   string
	ResetSource string
}

// Querier runs one instant PromQL query. *vm.Client satisfies it.
type Querier interface {
	Q(ctx context.Context, query string) ([]vm.Series, error)
}

// The three queries LoadWindows issues, verbatim.
const (
	pctQuery   = `max by (window,source)(claude_plan_window_pct)`
	resetQuery = `max by (window,source)(claude_plan_window_reset_timestamp_seconds)`
	ageQuery   = `min by (source)(claude_plan_data_age_seconds)`
)

// windowOrder fixes the three well-known windows first; any other window
// name sorts alphabetically after them.
var windowOrder = map[string]int{"five_hour": 0, "seven_day": 1, "weekly_scoped": 2}

// LoadWindows runs the pct, reset and age queries concurrently, then
// resolves each window through PickFreshest. It fails as a whole only if the
// pct query fails: without a percentage there is nothing to show for any
// window. The reset and age queries degrade to empty results instead,
// since a missing reset time or age is less informative, not fatal.
func LoadWindows(ctx context.Context, q Querier) ([]Window, map[string]float64, error) {
	var pctRes, resetRes, ageRes []vm.Series
	var pctErr, resetErr, ageErr error

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); pctRes, pctErr = q.Q(ctx, pctQuery) }()
	go func() { defer wg.Done(); resetRes, resetErr = q.Q(ctx, resetQuery) }()
	go func() { defer wg.Done(); ageRes, ageErr = q.Q(ctx, ageQuery) }()
	wg.Wait()

	if pctErr != nil {
		return nil, nil, pctErr
	}
	if resetErr != nil {
		resetRes = nil
	}
	if ageErr != nil {
		ageRes = nil
	}

	ages := map[string]float64{}
	for _, s := range ageRes {
		v, ok := sampleValue(s)
		if !ok {
			continue
		}
		ages[s.Metric["source"]] = v
	}

	pctPicked := PickFreshest(seriesToSamples(pctRes), ages)
	resetPicked := PickFreshest(seriesToSamples(resetRes), ages)

	return assemble(pctPicked, resetPicked), ages, nil
}

// assemble turns two picked maps into the sorted Window slice both loaders
// return. Split out so the per-account loader below produces windows that are
// shaped and ordered identically to the flat one's.
func assemble(pctPicked, resetPicked map[string]Picked) []Window {
	names := map[string]struct{}{}
	for w := range pctPicked {
		names[w] = struct{}{}
	}
	for w := range resetPicked {
		names[w] = struct{}{}
	}
	windows := make([]Window, 0, len(names))
	for name := range names {
		w := Window{Name: name}
		if p, ok := pctPicked[name]; ok {
			w.Pct, w.HasPct, w.PctSource = p.Value, true, p.Source
		}
		if r, ok := resetPicked[name]; ok {
			w.ResetTS, w.HasReset, w.ResetSource = r.Value, true, r.Source
		}
		windows = append(windows, w)
	}
	sort.Slice(windows, func(i, j int) bool {
		oi, oki := windowOrder[windows[i].Name]
		oj, okj := windowOrder[windows[j].Name]
		switch {
		case oki && okj:
			return oi < oj
		case oki:
			return true
		case okj:
			return false
		default:
			return windows[i].Name < windows[j].Name
		}
	})
	return windows
}

// AccountWindows is one Claude account's resolved plan windows.
//
// The account, not the profile, is the unit here. An allowance belongs to an
// account, and several profile directories can be signed in to one, so
// grouping by profile would draw one meter per profile and read as that many
// separate allowances.
type AccountWindows struct {
	Account string
	Windows []Window
	// Ages is how long since each source's figures last CHANGED, keyed by
	// source. It is what PickFreshest resolves a window on, and it is the
	// right input for that: between two sources, the one whose value moved
	// more recently is the one to believe.
	Ages map[string]float64
	// ConfirmedAgeS is how long since a live fetch last confirmed the cache
	// source, and HasConfirmedAge says whether anything ever has. It is a
	// different question from Ages and must not be substituted into
	// PickFreshest: an account whose percentage has genuinely not moved in an
	// hour has an old Ages["cache"] and a young ConfirmedAgeS, and that is
	// the correct reading of both.
	ConfirmedAgeS   float64
	HasConfirmedAge bool
}

// Per-account forms of the three queries. The flat ones above reduce with
// max by (window,source), which across several accounts answers "which
// account is highest" rather than "what is this account at". Keeping account
// in the grouping is what stops one account's 95% being reported as every
// account's.
const (
	pctByAccountQuery   = `max by (window,source,account)(claude_plan_window_pct)`
	resetByAccountQuery = `max by (window,source,account)(claude_plan_window_reset_timestamp_seconds)`
	ageByAccountQuery   = `min by (source,account)(claude_plan_data_age_seconds)`
	// Reduced with min rather than max, like its sibling above: with two
	// profiles on one account, the account has been confirmed as recently as
	// whichever of them last confirmed it.
	confirmedByAccountQuery = `min by (account)(claude_plan_data_confirmed_age_seconds)`
)

// LoadAccountWindows resolves plan windows per account, applying the same
// freshness rule PickFreshest applies within one account. Accounts come back
// sorted by their highest window percentage descending, so whichever account
// is closest to throttling is first whatever the caller does with the list.
func LoadAccountWindows(ctx context.Context, q Querier) ([]AccountWindows, error) {
	var pctRes, resetRes, ageRes, confRes []vm.Series
	var pctErr, resetErr, ageErr, confErr error

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); pctRes, pctErr = q.Q(ctx, pctByAccountQuery) }()
	go func() { defer wg.Done(); resetRes, resetErr = q.Q(ctx, resetByAccountQuery) }()
	go func() { defer wg.Done(); ageRes, ageErr = q.Q(ctx, ageByAccountQuery) }()
	go func() { defer wg.Done(); confRes, confErr = q.Q(ctx, confirmedByAccountQuery) }()
	wg.Wait()

	if pctErr != nil {
		return nil, pctErr
	}
	if resetErr != nil {
		resetRes = nil
	}
	if ageErr != nil {
		ageRes = nil
	}
	if confErr != nil {
		confRes = nil
	}

	// Ages are per account per source, because one account's statusline tap
	// being cold says nothing about another's.
	ages := map[string]map[string]float64{}
	for _, s := range ageRes {
		v, ok := sampleValue(s)
		if !ok {
			continue
		}
		acct := s.Metric["account"]
		if ages[acct] == nil {
			ages[acct] = map[string]float64{}
		}
		ages[acct][s.Metric["source"]] = v
	}

	// Absent stays absent: an account nothing has ever confirmed reports no
	// confirmation rather than a zero, which would read as "confirmed just
	// now" and is the opposite of the truth.
	confirmed := map[string]float64{}
	for _, s := range confRes {
		v, ok := sampleValue(s)
		if !ok {
			continue
		}
		if acct := s.Metric["account"]; acct != "" {
			confirmed[acct] = v
		}
	}

	// A series with no account label is not an account, and must not become
	// one. Every upgrade leaves the previous poller's unlabelled samples
	// inside the next query's lookbehind, and grouping them produced a
	// nameless fifth "account" on a four account machine, carrying a copy of
	// the primary account's billed figure. The same dollars appeared twice
	// under two rows, one of which had no name to explain itself. It ages out
	// on its own within the lookbehind, which is exactly what makes it worth
	// dropping in code: a bug that disappears before it can be investigated
	// is one that gets rediscovered on every upgrade.
	byAcct := map[string][]vm.Series{}
	resetByAcct := map[string][]vm.Series{}
	for _, s := range pctRes {
		if acct := s.Metric["account"]; acct != "" {
			byAcct[acct] = append(byAcct[acct], s)
		}
	}
	for _, s := range resetRes {
		if acct := s.Metric["account"]; acct != "" {
			resetByAcct[acct] = append(resetByAcct[acct], s)
		}
	}

	out := make([]AccountWindows, 0, len(byAcct))
	for acct, pct := range byAcct {
		a := ages[acct]
		if a == nil {
			a = map[string]float64{}
		}
		conf, hasConf := confirmed[acct]
		out = append(out, AccountWindows{
			Account: acct,
			Windows: assemble(
				PickFreshest(seriesToSamples(pct), a),
				PickFreshest(seriesToSamples(resetByAcct[acct]), a),
			),
			Ages:            a,
			ConfirmedAgeS:   conf,
			HasConfirmedAge: hasConf,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		hi, hj := MaxPct(out[i].Windows), MaxPct(out[j].Windows)
		if hi != hj {
			return hi > hj
		}
		return out[i].Account < out[j].Account
	})
	return out, nil
}

// MaxPct is the highest percentage across a set of windows, and -1 when none
// of them has one. It is how an account is ranked and how a picker labels an
// account without the reader opening it.
func MaxPct(ws []Window) float64 {
	max := -1.0
	for _, w := range ws {
		if w.HasPct && w.Pct > max {
			max = w.Pct
		}
	}
	return max
}

// seriesToSamples turns raw VM series into Samples, defaulting a missing
// window label to "unknown" rather than dropping the sample.
func seriesToSamples(res []vm.Series) []Sample {
	samples := make([]Sample, 0, len(res))
	for _, s := range res {
		window := s.Metric["window"]
		if window == "" {
			window = "unknown"
		}
		v, ok := sampleValue(s)
		samples = append(samples, Sample{
			Window: window,
			Source: s.Metric["source"],
			Value:  v,
			Valid:  ok,
		})
	}
	return samples
}

// sampleValue reads a VM instant-query series' value, which arrives JSON
// decoded as [timestamp, "value string"]. A value that fails to parse as a
// float, or parses to NaN, is reported invalid rather than aborting the rest
// of the series.
func sampleValue(s vm.Series) (float64, bool) {
	if len(s.Value) < 2 {
		return 0, false
	}
	str, ok := s.Value[1].(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(str, 64)
	if err != nil || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}
