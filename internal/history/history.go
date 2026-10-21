// Package history merges backfilled transcript history with the live OTel
// counters. Query construction and merge arithmetic only, so all of it is
// testable without a store.
//
// The live counters only exist from the day the collector was installed;
// `claude-obs backfill` covers everything before that by re-parsing the
// transcripts. Reading the live counter alone silently reports a window
// as long as the collector has been running, no matter what period the
// caller asked for: measured against the live store, "by model, 7 days"
// totalled about a thirteenth of what the daily chart beside it read for the
// same week.
//
// The merge rule: bucket both sides, let history fill first, and let the
// live series fill only the buckets history left empty. That is what makes
// re-running the backfill (it re-parses transcripts the live counters also
// counted) unable to double count.
//
// Each figure is recorded twice by the backfill: once at the request's own
// timestamp, once rolled up to local noon. A daily-step window reads the
// noon rollups, so a day's whole figure lands in exactly one bucket; an
// hourly window reads the per-request series, the only pair with the
// resolution an hour needs. Cost and tokens always come from the same pair,
// since a rate column dividing one against the other spans one window:
// mixing them was measured at 6.4% off on a 7 day total.
package history

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

const (
	// DAY and HOUR are the two step grids the dashboard queries in.
	DAY  = 86400
	HOUR = 3600
)

// Live counter metric names. These are the names Claude Code's OTel exporter
// writes, also spelled as COST_METRIC/TOKEN_METRIC in config.env for the
// shell scripts that query them.
const (
	costMetric  = "claude_code.cost.usage"
	tokenMetric = "claude_code.token.usage"
)

// seriesSpec names one kind's live counter and its two backfilled
// counterparts. A kind with no Daily series (pullRequests, commits) has no
// history half at all: the backfill computes cost and tokens from a
// transcript's own numbers, and a transcript does not say whether the
// command it ran opened a pull request. Those kinds are whatever the live
// collector has counted since it started, and callers are expected to say
// so (see LiveOnlyNote).
type seriesSpec struct {
	Live       string
	PerRequest string
	Daily      string
	// Cumulative marks a counter whose series is a running total for one
	// session rather than a flow to be accumulated over time. Cost and
	// tokens are flows: what matters is how much arrived inside the window,
	// so increase() summed across buckets is right. Commits and pull
	// requests are not: the series is that session's tally, so the answer is
	// its final value, and increase() gets it wrong in both directions.
	//
	// Measured on real data before this existed. A session whose 50 commits
	// all landed before the window opened exports a flat series, and
	// increase() over flat is 0, so the dashboard read "0 commits" for a
	// session that made 50. Another session, series running 2 to 30, came
	// back as 48 because increase() extrapolates at the window edges: 60%
	// too high. Both are fixed by reading the counter's maximum instead.
	Cumulative bool
}

var seriesTable = map[string]seriesSpec{
	"cost":         {Live: costMetric, PerRequest: "claude_hist_cost_usd", Daily: "claude_hist_daily_cost_usd"},
	"tokens":       {Live: tokenMetric, PerRequest: "claude_hist_tokens", Daily: "claude_hist_daily_tokens"},
	"pullRequests": {Live: "claude_code.pull_request.count", Cumulative: true},
	"commits":      {Live: "claude_code.commit.count", Cumulative: true},
}

// historical is the label set the backfill records. A breakdown by anything
// else (query_source, agent.name, skill.name, effort, mcp_server.name)
// cannot be merged: the history series do not carry those labels, so every
// historical dollar would arrive under an empty label and land in one giant
// unlabelled row that reads as though it were real. Those breakdowns stay
// live-only and say so.
//
// "workspace" earns its place here only because the backfill writes it. It
// was added after "project", and adding a label changes a series' identity,
// so the samples written before it exist under the old identity and are not
// replaced by a plain re-run. `claude-obs backfill --reset-cost-series`
// deletes the two cost series first for exactly that reason; without it the
// old and new series both answer the same query and every historical dollar
// is counted twice.
var historical = map[string]bool{"model": true, "project": true, "workspace": true, "sidechain": true, "type": true, "profile": true}

// HasHistory reports whether label is one the backfill records.
func HasHistory(label string) bool { return historical[label] }

// CanMerge reports whether a query for kind grouped by the labels in by can
// be merged at all: the kind needs a backfilled counterpart, and every
// grouping label needs to survive into it. One predicate, used both to
// decide the query (MergedTotals) and to word the note admitting what a
// live-only panel is missing (LiveOnlyNote), so the two can never disagree.
func CanMerge(kind string, by []string) bool {
	if seriesTable[kind].Daily == "" {
		return false
	}
	for _, l := range by {
		if !historical[l] {
			return false
		}
	}
	return true
}

// Window is one query window: samples from Start to End inclusive, stepped
// by Step seconds.
type Window struct {
	Start, End, Step int64
}

// DayWindow anchors to the grid the daily chart draws on: 03:00 UTC, so that
// exactly one local-noon rollup sample lands in each column. now is assumed
// to be a Unix timestamp in seconds (always non-negative), so integer
// division below is equivalent to the JS Math.floor.
func DayWindow(days int, now int64) Window {
	end := (now/DAY)*DAY + 3*3600
	if end > now {
		end -= DAY
	}
	return Window{Start: end - int64(days-1)*DAY, End: end, Step: DAY}
}

// RollingWindow anchors at "now" instead, the grid every table uses. A table
// has no calendar columns to line up, and anchoring it to the chart's 03:00
// boundary instead ends its window there too: measured at 23:00 local, that
// hid 17 hours of the heaviest day, $650 of the month, from a table headed
// "30 days". The oldest bucket can therefore reach back before the period's
// literal start, and a table's total and the chart's differ by the edges
// each includes; that is expected, not a bug.
func RollingWindow(days int, now int64, step int64) Window {
	return Window{Start: now - int64(days-1)*step, End: now, Step: step}
}

// RangeQuerier runs one range PromQL query. *vm.Client satisfies it.
type RangeQuerier interface {
	QR(ctx context.Context, query string, start, end int64, step string) ([]vm.Series, error)
}

// MergedParams configures MergedTotals.
//
//   - By groups the history query; LiveBy groups the live query when it must
//     differ (nil defaults to By). Only "by project" differs in practice:
//     history carries the project name outright, while the live counter only
//     carries a session id that must be joined to a project through KeyOf.
//   - KeyOf turns one side's labels into the shared merge key, and is told
//     which source it is looking at so a join can differ per source. Returning
//     ok=false drops the row, matching JS's key === null/undefined check. A
//     nil KeyOf defaults to reading labels[By[0]], matching history.js's
//     default `(labels) => labels[by[0]]`.
//   - History can only turn merging OFF. nil defers to CanMerge(Kind, By); a
//     pointer to false forces merging off outright (used when a caller must
//     query a kind/by combination that would otherwise merge, but wants the
//     live-only figure anyway); a pointer to true does NOT force merging on,
//     since asking for a label the backfill never recorded would return every
//     historical dollar under one giant unlabelled row that reads as real.
type MergedParams struct {
	Kind             string
	By               []string
	LiveBy           []string
	KeyOf            func(labels map[string]string, source string) (string, bool)
	History          *bool
	// Match narrows both halves to a label value, the way By widens them.
	// It goes through CanMerge for the same reason By does: filtering on a
	// label the backfill never recorded matches no historical series at all,
	// so the answer would silently become the live counter alone. Reading the
	// live counter alone once made one panel report a thirteenth of what the
	// chart beside it read for the same week.
	Match            map[string]string
	Start, End, Step int64
}

// Selector renders a metric name with an optional label filter:
// `metric{a="b",c="d"}`, keys sorted so one filter always produces one query
// string. VictoriaMetrics caches instant-query results by query string, so a
// selector whose label order wandered would miss its own cache every time.
//
// Values are not escaped, deliberately: every value that reaches here is a
// profile name, which ProfileName derives from a directory basename with the
// .claude prefixes stripped. A value carrying a quote would break the query
// rather than change its meaning, and there is no path by which one arrives.
func Selector(metric string, match map[string]string) string {
	if len(match) == 0 {
		return metric
	}
	keys := make([]string, 0, len(match))
	for k := range match {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, match[k])
	}
	return metric + "{" + strings.Join(parts, ",") + "}"
}

// groupClause builds PromQL's `sum by (a,b)(...)` when labels is non-empty,
// or bare `sum(...)` when it is not.
func groupClause(labels []string) string {
	if len(labels) == 0 {
		return "sum"
	}
	return "sum by (" + strings.Join(labels, ",") + ")"
}

// stepString formats a step (seconds) as the plain decimal string VM's
// query_range accepts for its "step" parameter (e.g. "86400"). history.js
// passes step as a number of seconds to query_range; this Go port's
// RangeQuerier.QR takes step as a string instead, so every call site here
// formats the integer seconds as plain decimal, which VM parses as seconds.
func stepString(step int64) string {
	return strconv.FormatInt(step, 10)
}

// defaultKeyOf matches history.js's default keyOf, `(labels) => labels[by[0]]`:
// an empty by has no key at all (ok=false, matching JS's undefined index),
// and looking up by[0] reports ok=false if that label was never populated for
// this series (labels only carries the names collect() was told to read).
func defaultKeyOf(by []string) func(map[string]string, string) (string, bool) {
	return func(labels map[string]string, _ string) (string, bool) {
		if len(by) == 0 {
			return "", false
		}
		v, ok := labels[by[0]]
		return v, ok
	}
}

// toFloat reads a JSON-decoded VM value, which arrives as either a float64
// (a timestamp) or a string (a sample value formatted by VM).
func toFloat(v any) (float64, bool) {
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

// MergedTotals returns Map(key -> total) for one kind, merging backfilled
// history with the live counter over [Start, End] stepped by Step. Kind must
// be a known key of seriesTable ("cost", "tokens", "pullRequests",
// "commits"); an unknown kind errors instead of silently building a query
// against an empty metric name and reading as $0, which is what the JS's
// SERIES[kind] would do too, just as a TypeError on the next property
// access rather than a returned error.
//
// Buckets fill in two passes: history first, then live. A bucket history
// already covered belongs to history; the live counter only fills the gaps
// history left. This is the whole double-count prevention rule: re-running
// the backfill (it re-parses transcripts the live counters also counted)
// cannot inflate a total, because any bucket it fills is a bucket the live
// pass will then skip.
func MergedTotals(ctx context.Context, qr RangeQuerier, p MergedParams) (map[string]float64, error) {
	s, ok := seriesTable[p.Kind]
	if !ok {
		return nil, fmt.Errorf("history: unknown kind %q", p.Kind)
	}
	win := "1d"
	hist := s.Daily
	if p.Step == HOUR {
		win = "1h"
		hist = s.PerRequest
	}

	liveBy := p.LiveBy
	if liveBy == nil {
		liveBy = p.By
	}
	keyOf := p.KeyOf
	if keyOf == nil {
		keyOf = defaultKeyOf(p.By)
	}

	// Merging is decided here, not remembered at each call site: `History`
	// can only turn it off, never on. Asking for a label the backfill never
	// recorded returns every historical dollar under one giant unlabelled
	// row that reads as though it were real, and asking for a kind with no
	// history series at all would query an empty metric name.
	merge := CanMerge(p.Kind, p.By)
	// A filter label the backfill never recorded is the same problem as a
	// grouping label it never recorded, and has to turn merging off the same
	// way: the history selector would match nothing and the total would
	// quietly become live-only.
	for l := range p.Match {
		if !historical[l] {
			merge = false
		}
	}
	if p.History != nil && !*p.History {
		merge = false
	}

	stepStr := stepString(p.Step)

	var histRes []vm.Series
	if merge {
		q := fmt.Sprintf("%s(sum_over_time(%s[%s]))", groupClause(p.By), Selector(hist, p.Match), win)
		res, err := qr.QR(ctx, q, p.Start, p.End, stepStr)
		if err != nil {
			return nil, err
		}
		histRes = res
	}

	// A cumulative counter is read as its running maximum, over a lookbehind
	// spanning the whole request rather than one bucket. The wide window is
	// what keeps a session that produced more than one series correct: when
	// Claude Code restarts mid-session the counter restarts too, under the
	// same session.id but a new service.version, and only a window holding
	// both lets `sum by` add the two tallies. A per-bucket window would see
	// one at a time and the max across buckets would report the larger
	// instead of the sum.
	liveWin, liveFn := win, "increase"
	if s.Cumulative {
		liveWin, liveFn = stepString(p.End-p.Start+p.Step), "max_over_time"
	}
	liveQuery := fmt.Sprintf("%s(%s(%s[%s]))", groupClause(liveBy), liveFn, Selector(s.Live, p.Match), liveWin)
	liveRes, err := qr.QR(ctx, liveQuery, p.Start, p.End, stepStr)
	if err != nil {
		return nil, err
	}

	buckets := int(math.Round(float64(p.End-p.Start)/float64(p.Step))) + 1
	totals := map[string]float64{}
	filled := map[int]bool{}

	collect := func(result []vm.Series, source string, labelNames []string) {
		for _, ser := range result {
			labels := make(map[string]string, len(labelNames))
			for _, l := range labelNames {
				labels[l] = ser.Metric[l]
			}
			key, keyOK := keyOf(labels, source)
			for _, pt := range ser.Values {
				t, tOK := toFloat(pt[0])
				v, vOK := toFloat(pt[1])
				if !tOK {
					continue
				}
				i := int(math.Round((t - float64(p.Start)) / float64(p.Step)))
				if i < 0 || i >= buckets || !vOK || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
					continue
				}
				// A bucket history covered is history's; the live counter
				// only fills the gaps it left.
				if source == "live" && filled[i] {
					continue
				}
				if !keyOK {
					continue
				}
				// Buckets of a flow add up; buckets of a running total do
				// not. Summing a cumulative counter's per-bucket maxima
				// would multiply one session's tally by the number of
				// buckets it appears in.
				if s.Cumulative {
					if v > totals[key] {
						totals[key] = v
					}
				} else {
					totals[key] += v
				}
				if source == "history" {
					filled[i] = true
				}
			}
		}
	}
	collect(histRes, "history", p.By)
	collect(liveRes, "live", liveBy)
	return totals, nil
}

// HistoryBuckets reports which buckets the backfill covers for one kind. A
// panel dividing one kind by another (cost per token) needs this: an install
// that has not re-run the backfill since it learned to record daily token
// history has buckets carrying cost and no tokens, which would quietly
// inflate every rate it shows rather than leaving a visible hole. As in
// MergedTotals, an unknown kind errors rather than silently querying an
// empty metric name.
func HistoryBuckets(ctx context.Context, qr RangeQuerier, kind string, w Window) (map[int]bool, error) {
	s, ok := seriesTable[kind]
	if !ok {
		return nil, fmt.Errorf("history: unknown kind %q", kind)
	}
	win := "1d"
	hist := s.Daily
	if w.Step == HOUR {
		win = "1h"
		hist = s.PerRequest
	}
	q := fmt.Sprintf("sum(sum_over_time(%s[%s]))", hist, win)
	res, err := qr.QR(ctx, q, w.Start, w.End, stepString(w.Step))
	if err != nil {
		return nil, err
	}
	out := map[int]bool{}
	if len(res) == 0 {
		return out, nil
	}
	for _, pt := range res[0].Values {
		t, tOK := toFloat(pt[0])
		v, vOK := toFloat(pt[1])
		if !tOK || !vOK {
			continue
		}
		if v > 0 {
			i := int(math.Round((t - float64(w.Start)) / float64(w.Step)))
			out[i] = true
		}
	}
	return out, nil
}

// MergedPeriodCost is the period's whole cost, merged. Used to say how much
// of a period a live-only breakdown is leaving out, rather than letting its
// total quietly read as the period's.
func MergedPeriodCost(ctx context.Context, qr RangeQuerier, w Window) (float64, error) {
	totals, err := MergedTotals(ctx, qr, MergedParams{
		Kind:  "cost",
		By:    []string{},
		KeyOf: func(map[string]string, string) (string, bool) { return "all", true },
		Start: w.Start, End: w.End, Step: w.Step,
	})
	if err != nil {
		return 0, err
	}
	return totals["all"], nil
}

// usd formats v the way Intl.NumberFormat("en-US", {style: "currency",
// currency: "USD"}) does in history.js's money(): two decimal places,
// comma-grouped thousands. LiveOnlyNote is the only caller.
func usd(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	cents := int64(math.Round(v * 100))
	whole := cents / 100
	frac := cents % 100

	digits := strconv.FormatInt(whole, 10)
	var grouped strings.Builder
	n := len(digits)
	for i, r := range digits {
		if i > 0 && (n-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(r)
	}

	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s$%s.%02d", sign, grouped.String(), frac)
}

// LiveOnlyNote says what a live-only breakdown leaves out, in money, so its
// total cannot be read as the period's own. Empty when the label is
// mergeable, or when history holds nothing this label is missing (every
// install once the collector has run longer than the period). Shared by
// both pages that draw these tables: the sentence has to say the same thing
// in both places, and the condition for showing it is a property of the
// data, not of either page.
func LiveOnlyNote(ctx context.Context, qr RangeQuerier, label string, total float64, w Window) (string, error) {
	if CanMerge("cost", []string{label}) {
		return "", nil
	}
	periodTotal, err := MergedPeriodCost(ctx, qr, w)
	if err != nil {
		return "", err
	}
	missing := periodTotal - total
	if missing <= 0.005 {
		return "", nil
	}
	return fmt.Sprintf(
		". The transcript backfill records no %s, so this is the live collector only: %s of the period's %s is not broken down here",
		label, usd(missing), usd(periodTotal),
	), nil
}
