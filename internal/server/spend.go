package server

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/history"
	"github.com/heapsgoodstudio/agent-observability/internal/overage"
	"github.com/heapsgoodstudio/agent-observability/internal/pricing"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// notIndexed is the project a session lands under when its id is not yet in
// the sessions table: usually a session still in progress, not yet swept by
// an indexing pass.
const notIndexed = "not yet indexed"

// allDays makes the "all" period a fixed 730-day chart/table window rather
// than a probe for the earliest sample. buildDailyCost instead trims the
// daily chart's own leading empty buckets once it has the data, which costs
// no extra query.
const allDays = 730

// measureNotApplied is shown when the measure toggle cannot be honoured.
// The daily-cost chart and the hour heatmap are always notional; when the
// caller asked for "billed", this is the only place that says so, since
// neither panel has a billed figure of its own to show instead.
const measureNotApplied = "the measure toggle does not apply to this chart, figures shown are always notional"

// periodShort renders a period for prose, used only in the model-usage note
// ("... across N models, 7 days").
var periodShort = map[string]string{
	"24h": "24 hours", "7d": "7 days", "30d": "30 days", "mtd": "month to date", "all": "all history",
}

var validSpendPeriods = map[string]bool{"24h": true, "7d": true, "30d": true, "mtd": true, "all": true}

// normalizePeriod falls back to "30d" for an unrecognized or absent period.
func normalizePeriod(period string) string {
	if validSpendPeriods[period] {
		return period
	}
	return "30d"
}

// normalizeMeasure falls back to "notional" for an unrecognized or absent
// measure.
func normalizeMeasure(measure string) string {
	if measure == "billed" {
		return "billed"
	}
	return "notional"
}

// identityCanon is the default canonicalizer for every attribution but
// model: the label's raw value, unchanged.
func identityCanon(v string) string { return v }

// profileScope is one selected profile and the account it is signed in to.
//
// Both halves are needed because the page mixes two kinds of figure. Cost,
// tokens and sessions belong to a PROFILE, so they filter on profile. The
// overage bands do not: being in overage is the ACCOUNT's state, shared by
// every profile signed in to it, so the bands filter on account. Masking one
// profile's spend against another account's bands would report money as
// billed that never was.
type profileScope struct {
	Profile string
	Account string
}

// match is the label filter for anything measured per profile. Empty for the
// unfiltered page, which is what keeps every query byte-identical to what it
// was before a selector existed.
func (p profileScope) match() map[string]string {
	if p.Profile == "" {
		return nil
	}
	return map[string]string{"profile": p.Profile}
}

// accountMatch is the label filter for anything measured per ACCOUNT: the
// plan windows, the billed figure, the overage curve and the bands. Empty
// when no profile is selected, and also empty when a profile is selected
// whose account nothing has reported, since an unnarrowed allowance figure is
// wrong in a way the reader can see and argue with, where another account's
// figure is not.
func (p profileScope) accountMatch() map[string]string {
	if p.Account == "" {
		return nil
	}
	return map[string]string{"account": p.Account}
}

// acct narrows a per-account metric name, the way match narrows a per-profile
// one.
func (p profileScope) acct(metric string) string {
	return history.Selector(metric, p.accountMatch())
}

// bandSelector is claude_in_overage narrowed to the selected profile's
// account. With no account resolved it stays unnarrowed rather than guessing
// one: an unfiltered band set overstates which spend was billed, which is
// visible and arguable, where a wrong account's bands are neither.
func (p profileScope) bandSelector() string {
	return p.acct("claude_in_overage")
}

// resolveProfileScope looks the selected profile's account up from the
// poller's info gauge. A failure or a miss leaves the account empty, which
// bandSelector handles; it is not worth failing a page over.
func resolveProfileScope(ctx context.Context, q *vm.Client, profile string) profileScope {
	sc := profileScope{Profile: profile}
	if profile == "" || q == nil {
		return sc
	}
	res, err := q.Q(ctx, history.Selector(accountInfoQuery, map[string]string{"profile": profile}))
	if err != nil {
		return sc
	}
	for _, s := range res {
		if a := s.Metric["account"]; a != "" {
			sc.Account = a
			break
		}
	}
	return sc
}

// periodWindows is periodBounds' return: three windows, because three kinds
// of panel need different edges. Chart anchors to a calendar-day grid, since
// its columns ARE calendar days; Table anchors to "now", so "30 days" means
// the 30 days ending now. Anchoring the tables to the chart's grid was tried
// and reverted: at 23:00 local it hid 17 hours of the heaviest day.
type periodWindows struct {
	Chart, Table           history.Window
	BilledStart, BilledEnd int64
}

// periodBounds resolves one period into its three windows.
// BilledStart/BilledEnd are the *raw*, unclamped billed window; billedTotals
// applies the maxBilledPoints-at-billedStepS clamp itself, against the bounds
// handed to it here.
func periodBounds(period string, now int64) periodWindows {
	switch period {
	case "24h":
		return periodWindows{
			Chart:       history.Window{Start: now - history.DAY, End: now, Step: history.HOUR},
			Table:       history.RollingWindow(24, now, history.HOUR),
			BilledStart: now - history.DAY, BilledEnd: now,
		}
	case "7d":
		return periodWindows{
			Chart:       history.DayWindow(7, now),
			Table:       history.RollingWindow(7, now, history.DAY),
			BilledStart: now - 7*history.DAY, BilledEnd: now,
		}
	case "30d":
		return periodWindows{
			Chart:       history.DayWindow(30, now),
			Table:       history.RollingWindow(30, now, history.DAY),
			BilledStart: now - 30*history.DAY, BilledEnd: now,
		}
	case "mtd":
		monthStart := monthStartLocal(now)
		days := int((now-monthStart)/history.DAY) + 1
		if days < 1 {
			days = 1
		}
		return periodWindows{
			Chart:       history.DayWindow(days, now),
			Table:       history.RollingWindow(days, now, history.DAY),
			BilledStart: monthStart, BilledEnd: now,
		}
	default: // "all"
		allChart := history.DayWindow(allDays, now)
		return periodWindows{
			Chart:       allChart,
			Table:       history.RollingWindow(allDays, now, history.DAY),
			BilledStart: allChart.Start, BilledEnd: now,
		}
	}
}

/* Billed masking --------------------------------------------------------- */

// billedStepS and maxBilledPoints bound one billed query:
// VictoriaMetrics rejects a query_range whose point count exceeds
// -search.maxPointsPerTimeseries (default 30000), so a window wider than the
// cap allows has to give up span at the end nearest "now" instead.
const (
	billedStepS     = 600
	maxBilledPoints = 30000
)

// billedResult is billedTotals' return: the overage bands, the per-key
// totals masked against them, and the window actually measured (which may be
// narrower than [rawStart, end] asked for).
type billedResult struct {
	Bands      [][2]float64
	Totals     map[string]float64
	Start, End int64
	Clamped    bool
}

// billedTotals masks the notional range series
// (10 minute step, independent of the page's own period step) against the
// claude_in_overage bands and sum what lands inside them per canonicalized
// label value. This is overage.MaskSeries's one consumer in this package.
func billedTotals(ctx context.Context, qr history.RangeQuerier, scope profileScope, label string, canon func(string) string, rawStart, end int64) (billedResult, error) {
	// query_range is inclusive of both ends, so a span of exactly
	// maxBilledPoints*billedStepS is one point over the cap; back off by one
	// step.
	start := rawStart
	if minStart := end - (maxBilledPoints-1)*billedStepS; minStart > start {
		start = minStart
	}
	clamped := start > rawStart

	seriesQuery := fmt.Sprintf("sum by (%s)(increase(%s[10m]))", label, history.Selector(costMetric, scope.match()))
	seriesRes, err := qr.QR(ctx, seriesQuery, start, end, strconv.Itoa(billedStepS))
	if err != nil {
		return billedResult{}, err
	}
	bandRes, err := qr.QR(ctx, "max("+scope.bandSelector()+") == 1", start, end, strconv.Itoa(billedStepS))
	if err != nil {
		return billedResult{}, err
	}

	bandPts := vm.Points(bandRes)
	bandTimes := make([]float64, len(bandPts))
	for i, p := range bandPts {
		bandTimes[i] = p.TS
	}
	// Bands are sampled every 10 minutes; a gap under twice that is still one
	// contiguous overage stretch. The same grouping is applied wherever
	// bands are drawn, so the three panels agree on where a stretch ends.
	bands := overage.GroupBands(bandTimes, billedStepS*2)

	totals := map[string]float64{}
	if len(bands) == 0 {
		return billedResult{Bands: bands, Totals: totals, Start: start, End: end, Clamped: clamped}, nil
	}
	for _, s := range seriesRes {
		v := s.Metric[label]
		if v == "" {
			v = "unlabelled"
		}
		key := canon(v)
		pts := vm.Points([]vm.Series{s})
		pairs := make([][2]float64, len(pts))
		for i, p := range pts {
			pairs[i] = [2]float64{p.TS, p.V}
		}
		masked := overage.MaskSeries(pairs, bands)
		var sum float64
		for _, p := range masked {
			sum += p[1]
		}
		if sum > 0 {
			totals[key] += sum
		}
	}
	return billedResult{Bands: bands, Totals: totals, Start: start, End: end, Clamped: clamped}, nil
}

/* Note text --------------------------------------------------------------- */

// noteWord pluralizes word when count != 1.
func noteWord(word string, count int) string {
	if count == 1 {
		return word
	}
	return word + "s"
}

// fmtDayGB renders t day-first with a short month, e.g. "15 Aug".
func fmtDayGB(t time.Time) string {
	return fmt.Sprintf("%d %s", t.Day(), t.Month().String()[:3])
}

// billedNote builds the estimate disclaimer, plus, only
// when billedTotals actually clamped the window, the real span that was
// measured.
func billedNote(total float64, count int, word string, res billedResult) string {
	base := fmt.Sprintf("%s across %d %s, estimated from the overage bands, not an exact billed figure", usd(total), count, word)
	if !res.Clamped {
		return base
	}
	days := int64(math.Round(float64(res.End-res.Start) / float64(history.DAY)))
	return fmt.Sprintf("%s, and limited to the most recent %d days (from %s) "+
		"because VictoriaMetrics caps how many points a single query may request",
		base, days, fmtDayGB(time.Unix(res.Start, 0)))
}

// compactInt abbreviates integers for panels where the exact figure would
// not fit ("2,669,988,408" -> "2.67B").
// Used only by the model-usage note's token count.
func compactInt(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e9:
		return fmt.Sprintf("%.2fB", v/1e9)
	case a >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case a >= 1e5:
		return fmt.Sprintf("%dk", int64(math.Round(v/1e3)))
	default:
		return groupInt(int64(math.Round(v)))
	}
}

// groupInt comma-groups an integer's digits, matching fmtInt's en-AU
// thousands separators (shared shape with usd()'s own grouping, kept
// separate per this codebase's convention of small formatters living local
// to their one package rather than being shared).
func groupInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	digits := strconv.FormatInt(n, 10)
	var grouped strings.Builder
	ln := len(digits)
	for i, r := range digits {
		if i > 0 && (ln-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(r)
	}
	if neg {
		return "-" + grouped.String()
	}
	return grouped.String()
}

// attrRowsFrom builds one attribution's rows: positive totals only, sorted
// descending (ties broken on key for determinism, as computeAttribution
// already does for Overview). Unlike Overview's computeAttribution, this
// keeps every row: Spend's attribution tables are the full breakdown, not a
// top-N with an "other" bucket (that shape is only for the daily chart).
func attrRowsFrom(totals map[string]float64) (rows []*claudeobsv1.AttrRow, total float64, count int) {
	all := make([]kv, 0, len(totals))
	for k, v := range totals {
		if v > 0 {
			all = append(all, kv{k, v})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Value != all[j].Value {
			return all[i].Value > all[j].Value
		}
		return all[i].Key < all[j].Key
	})
	rows = make([]*claudeobsv1.AttrRow, len(all))
	for i, e := range all {
		rows[i] = &claudeobsv1.AttrRow{Key: e.Key, Value: e.Value}
		total += e.Value
	}
	count = len(all)
	return rows, total, count
}

/* Attributions ------------------------------------------------------------- */

// attributionSpec names one of the six labels notional/billed both share a
// shape for: model canonicalizes through pricing.ShortModel, the rest are
// used as-is. "project" is handled separately (buildProjectAttribution)
// since it needs the session index join.
type attributionSpec struct {
	Label string
	Canon func(string) string
}

var simpleAttributions = []attributionSpec{
	{"model", pricing.ShortModel},
	// profile is first among the identity labels because it is the widest
	// cut: everything below it splits one profile's spend further. It merges
	// with history, unlike its neighbours, because the backfill writes the
	// label (see history.historical), so this row is the full picture rather
	// than only what the live counters have seen.
	{"profile", identityCanon},
	{"query_source", identityCanon},
	{"agent.name", identityCanon},
	{"skill.name", identityCanon},
	{"effort", identityCanon},
	{"mcp_server.name", identityCanon},
}

// buildBreakdownAttribution builds one of the six plain-label breakdowns. A query failure lands in the Attribution's own Error
// field, not the page-level SectionError list: one attribution failing must
// not blank the other six.
func buildBreakdownAttribution(ctx context.Context, qr history.RangeQuerier, scope profileScope, label string, canon func(string) string, measure string, table, billedWindow periodWindows) *claudeobsv1.Attribution {
	a := &claudeobsv1.Attribution{Label: label}
	if measure == "billed" {
		res, err := billedTotals(ctx, qr, scope, label, canon, billedWindow.BilledStart, billedWindow.BilledEnd)
		if err != nil {
			a.Error = err.Error()
			return a
		}
		if len(res.Bands) == 0 {
			return a
		}
		rows, total, count := attrRowsFrom(res.Totals)
		if count == 0 {
			return a
		}
		a.Rows = rows
		a.Note = billedNote(total, count, noteWord("value", count), res)
		return a
	}

	totals, err := history.MergedTotals(ctx, qr, history.MergedParams{
		Kind:  "cost",
		By:    []string{label},
		Match: scope.match(),
		KeyOf: func(labels map[string]string, _ string) (string, bool) {
			v := labels[label]
			if v == "" {
				v = "unlabelled"
			}
			return canon(v), true
		},
		Start: table.Table.Start, End: table.Table.End, Step: table.Table.Step,
	})
	if err != nil {
		a.Error = err.Error()
		return a
	}
	rows, total, count := attrRowsFrom(totals)
	if count == 0 {
		return a
	}
	a.Rows = rows
	liveNote, err := history.LiveOnlyNote(ctx, qr, label, total, table.Table)
	if err != nil {
		a.Error = err.Error()
		return a
	}
	a.Note = fmt.Sprintf("%s across %d %s%s", usd(total), count, noteWord("value", count), liveNote)
	return a
}

// buildProjectAttribution builds the one breakdown that needs a join, since
// the live cost metric carries no project label at all, only session.id. A
// session the sessions table does not know about (still in progress, not yet
// indexed) groups into notIndexed rather than being dropped. projectFilter,
// when non-empty, keeps only that one row, matched on the same workspace name
// the rows are keyed by.
//
// Both halves key on the workspace name: the backfill writes a workspace
// label of its own, and the live half joins through workspacesFromSessions,
// which derives the identical string. They have to agree, or one project
// splits into two rows with half its money in each. If the history half
// arrives unlabelled here, the cause is an install whose backfill predates
// the workspace label, and the fix is
// `claude-obs backfill --reset-cost-series`, not a change at this call site.
func buildProjectAttribution(ctx context.Context, qr history.RangeQuerier, d *sql.DB, scope profileScope, measure string, win, billedWindow periodWindows, projectFilter string) *claudeobsv1.Attribution {
	a := &claudeobsv1.Attribution{Label: "project"}
	sessionRows, err := listIndexedSessions(d, scope.Profile)
	if err != nil {
		a.Error = err.Error()
		return a
	}
	idToProject := workspacesFromSessions(sessionRows)

	billed := measure == "billed"
	var totals map[string]float64
	var bres billedResult
	if billed {
		res, err := billedTotals(ctx, qr, scope, "session.id", identityCanon, billedWindow.BilledStart, billedWindow.BilledEnd)
		if err != nil {
			a.Error = err.Error()
			return a
		}
		bres = res
		if len(res.Bands) == 0 {
			return a
		}
		totals = map[string]float64{}
		for id, v := range res.Totals {
			name, ok := idToProject[id]
			if !ok {
				name = notIndexed
			}
			totals[name] += v
		}
	} else {
		totals, err = history.MergedTotals(ctx, qr, history.MergedParams{
			Kind:   "cost",
			By:     []string{"workspace"},
			LiveBy: []string{"session.id"},
			Match:  scope.match(),
			KeyOf: func(labels map[string]string, source string) (string, bool) {
				if source == "history" {
					v := labels["workspace"]
					if v == "" {
						v = "unlabelled"
					}
					return v, true
				}
				name, ok := idToProject[labels["session.id"]]
				if !ok {
					name = notIndexed
				}
				return name, true
			},
			Start: win.Table.Start, End: win.Table.End, Step: win.Table.Step,
		})
		if err != nil {
			a.Error = err.Error()
			return a
		}
	}

	if projectFilter != "" {
		totals = map[string]float64{projectFilter: totals[projectFilter]}
	}

	rows, total, count := attrRowsFrom(totals)
	if count == 0 {
		return a
	}
	// No shortening pass here any more: workspace names are already the
	// display form, and running ShortProject over them would only be a no-op
	// waiting to mangle a name that happens to start with "-Users-".
	a.Rows = rows
	if billed {
		a.Note = billedNote(total, count, noteWord("project", count), bres)
	} else {
		a.Note = fmt.Sprintf("%s across %d %s", usd(total), count, noteWord("project", count))
	}
	return a
}

// buildAttributions runs all eight attributions (seven plain labels plus
// project) in the response order the controller fixed: model, profile,
// query_source, agent.name, skill.name, effort, mcp_server.name, project.
func buildAttributions(ctx context.Context, qr history.RangeQuerier, d *sql.DB, scope profileScope, measure string, win periodWindows, projectFilter string) []*claudeobsv1.Attribution {
	out := make([]*claudeobsv1.Attribution, 0, len(simpleAttributions)+1)
	for _, spec := range simpleAttributions {
		// The profile breakdown under a profile filter would be one row
		// restating the filter, so it is skipped rather than rendered as a
		// tautology. Every other breakdown still splits the selected
		// profile's spend, which is the point of selecting one.
		if spec.Label == "profile" && scope.Profile != "" {
			continue
		}
		out = append(out, buildBreakdownAttribution(ctx, qr, scope, spec.Label, spec.Canon, measure, win, win))
	}
	out = append(out, buildProjectAttribution(ctx, qr, d, scope, measure, win, win, projectFilter))
	return out
}

/* Daily cost chart ---------------------------------------------------------- */

// topNWithOther returns the top n entries by value
// descending (ties broken on key for determinism), plus an "other" bucket
// summing everything past n when that remainder is positive. Returns the
// ordered keys (for ChartSeries order) and the tail's own keys (so the
// caller can re-sum "other" per bucket).
func topNWithOther(totals map[string]float64, n int) (keys []string, tailNames []string) {
	all := make([]kv, 0, len(totals))
	for k, v := range totals {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Value != all[j].Value {
			return all[i].Value > all[j].Value
		}
		return all[i].Key < all[j].Key
	})
	head := all
	if len(head) > n {
		head = head[:n]
	}
	var tail []kv
	if len(all) > n {
		tail = all[n:]
	}
	var rest float64
	for _, t := range tail {
		rest += t.Value
	}
	keys = make([]string, len(head))
	for i, h := range head {
		keys[i] = h.Key
	}
	if rest > 0 {
		keys = append(keys, "other")
	}
	tailNames = make([]string, len(tail))
	for i, t := range tail {
		tailNames[i] = t.Key
	}
	return keys, tailNames
}

// buildDailyCost builds the stacked daily chart: hist and live queried over
// the chart window (hist wins any bucket it has data for; live only fills
// the buckets hist left empty), stacked by model with the top 4 plus an
// "other" bucket. For "all", leading all-empty buckets are trimmed rather
// than probed for separately (see allDays' comment). Returns (nil, 0, nil)
// when there is no cost at all: that is a legitimate empty state, not an
// error.
func buildDailyCost(ctx context.Context, qr history.RangeQuerier, scope profileScope, period string, chart history.Window) ([]*claudeobsv1.ChartSeries, float64, error) {
	isHourly := chart.Step == history.HOUR
	histQuery := "sum by (model)(sum_over_time(" + history.Selector("claude_hist_daily_cost_usd", scope.match()) + "[1d]))"
	win := "1d"
	if isHourly {
		histQuery = "sum by (model)(sum_over_time(" + history.Selector("claude_hist_cost_usd", scope.match()) + "[1h]))"
		win = "1h"
	}
	liveQuery := fmt.Sprintf("sum by (model)(increase(%s[%s]))", history.Selector(costMetric, scope.match()), win)

	stepStr := strconv.FormatInt(chart.Step, 10)
	histRes, err := qr.QR(ctx, histQuery, chart.Start, chart.End, stepStr)
	if err != nil {
		return nil, 0, err
	}
	liveRes, err := qr.QR(ctx, liveQuery, chart.Start, chart.End, stepStr)
	if err != nil {
		return nil, 0, err
	}

	n := int(math.Round(float64(chart.End-chart.Start)/float64(chart.Step))) + 1
	xs := make([]int64, n)
	for i := range xs {
		xs[i] = chart.Start + int64(i)*chart.Step
	}
	byBucket := make([]map[string]float64, n)
	for i := range byBucket {
		byBucket[i] = map[string]float64{}
	}
	histBuckets := map[int]bool{}
	indexOf := func(t float64) int {
		return int(math.Round((t - float64(chart.Start)) / float64(chart.Step)))
	}

	collect := func(series []vm.Series, skipHistBuckets bool) {
		for _, s := range series {
			model := s.Metric["model"]
			if model == "" {
				model = "unknown"
			}
			model = pricing.ShortModel(model)
			for _, p := range vm.Points([]vm.Series{s}) {
				i := indexOf(p.TS)
				// Guards the same shapes history.MergedTotals does:
				// vm.Points already drops
				// values that fail to parse, but a value that parses to
				// NaN or +/-Inf (e.g. a division-by-zero result upstream)
				// would otherwise still pass v <= 0 (NaN compares false to
				// everything) and corrupt a bucket's sum.
				if i < 0 || i >= n || p.V <= 0 || math.IsNaN(p.V) || math.IsInf(p.V, 0) {
					continue
				}
				if skipHistBuckets && histBuckets[i] {
					continue
				}
				byBucket[i][model] += p.V
				if !skipHistBuckets {
					histBuckets[i] = true
				}
			}
		}
	}
	collect(histRes, false)
	// Live series only fill buckets the backfill left empty, so re-running
	// the backfill can never double count against the OTel counters.
	collect(liveRes, true)

	totals := make([]float64, n)
	for i, m := range byBucket {
		for _, v := range m {
			totals[i] += v
		}
	}

	xsUsed, byBucketUsed, totalsUsed := xs, byBucket, totals
	if period == "all" {
		first := -1
		for i, t := range totals {
			if t > 0 {
				first = i
				break
			}
		}
		if first > 0 {
			xsUsed = xs[first:]
			byBucketUsed = byBucket[first:]
			totalsUsed = totals[first:]
		}
	}

	var grand float64
	for _, t := range totalsUsed {
		grand += t
	}
	if grand <= 0 {
		return nil, 0, nil
	}

	modelTotals := map[string]float64{}
	for _, m := range byBucketUsed {
		for k, v := range m {
			modelTotals[k] += v
		}
	}
	keys, tailNames := topNWithOther(modelTotals, 4)

	seriesOut := make([]*claudeobsv1.ChartSeries, len(keys))
	for ki, k := range keys {
		pts := make([]*claudeobsv1.Point, len(xsUsed))
		for i, x := range xsUsed {
			var v float64
			if k == "other" {
				for _, m := range tailNames {
					v += byBucketUsed[i][m]
				}
			} else {
				v = byBucketUsed[i][k]
			}
			pts[i] = &claudeobsv1.Point{Ts: float64(x), Value: v}
		}
		seriesOut[ki] = &claudeobsv1.ChartSeries{Label: k, Points: pts}
	}
	return seriesOut, grand, nil
}

/* Heatmap --------------------------------------------------------------- */

// heatmapPointsToProto applies the heatmap's bucket shift:
// `sum_over_time(claude_hist_cost_usd[1h])` sampled at
// timestamp t covers the hour ENDING at t, so the sample belongs to the
// bucket starting at t - 3600 (weekday/hour-of-day are derived from that
// shifted timestamp). Without the shift, every cell in the heatmap would be
// bucketed one hour later than the activity it represents.
func heatmapPointsToProto(pts []vm.Point) []*claudeobsv1.Point {
	if len(pts) == 0 {
		return nil
	}
	out := make([]*claudeobsv1.Point, len(pts))
	for i, p := range pts {
		out[i] = &claudeobsv1.Point{Ts: p.TS - 3600, Value: p.V}
	}
	return out
}

/* Model usage --------------------------------------------------------------- */

// plural returns "s" unless n == 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// buildModelUsage builds the per-model table. It carries no display-only
// derived column (share, $/M out, cache %, write:read): ModelUsageRow carries
// the raw cost and token figures the frontend computes those from, plus the
// note (which does need computing here, since
// its gap warning depends on historyBuckets this handler alone queries).
func buildModelUsage(ctx context.Context, qr history.RangeQuerier, scope profileScope, period string, table history.Window) ([]*claudeobsv1.ModelUsageRow, string, error) {
	cost, err := history.MergedTotals(ctx, qr, history.MergedParams{
		Kind: "cost", By: []string{"model"}, Match: scope.match(),
		KeyOf: func(labels map[string]string, _ string) (string, bool) {
			model := labels["model"]
			if model == "" {
				model = "unknown"
			}
			return pricing.ShortModel(model), true
		},
		Start: table.Start, End: table.End, Step: table.Step,
	})
	if err != nil {
		return nil, "", err
	}
	tokens, err := history.MergedTotals(ctx, qr, history.MergedParams{
		Kind: "tokens", By: []string{"model", "type"}, Match: scope.match(),
		KeyOf: func(labels map[string]string, _ string) (string, bool) {
			model := labels["model"]
			if model == "" {
				model = "unknown"
			}
			return labels["type"] + ":" + pricing.ShortModel(model), true
		},
		Start: table.Start, End: table.End, Step: table.Step,
	})
	if err != nil {
		return nil, "", err
	}
	costDays, err := history.HistoryBuckets(ctx, qr, "cost", table)
	if err != nil {
		return nil, "", err
	}
	tokenDays, err := history.HistoryBuckets(ctx, qr, "tokens", table)
	if err != nil {
		return nil, "", err
	}

	ranked := make([]kv, 0, len(cost))
	for k, v := range cost {
		if v > 0 {
			ranked = append(ranked, kv{k, v})
		}
	}
	if len(ranked) == 0 {
		return nil, "", nil
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Value != ranked[j].Value {
			return ranked[i].Value > ranked[j].Value
		}
		return ranked[i].Key < ranked[j].Key
	})

	var grand float64
	for _, r := range ranked {
		grand += r.Value
	}

	var allTokens float64
	rows := make([]*claudeobsv1.ModelUsageRow, len(ranked))
	for i, r := range ranked {
		tok := func(t string) float64 { return tokens[t+":"+r.Key] }
		input := tok("input")
		output := tok("output")
		read := tok("cacheRead")
		write := tok("cacheCreation")
		allTokens += input + output + read + write
		rows[i] = &claudeobsv1.ModelUsageRow{
			Model: r.Key, CostUsd: r.Value, Output: output, CacheRead: read, CacheCreation: write, Input: input,
		}
	}

	models := fmt.Sprintf("%d model%s", len(rows), plural(len(rows)))
	note := fmt.Sprintf("%s and %s tokens across %s, %s", usd(grand), compactInt(allTokens), models, periodShort[period])
	gap := 0
	for i := range costDays {
		if !tokenDays[i] {
			gap++
		}
	}
	if gap > 0 {
		unit := "day"
		if table.Step == history.HOUR {
			unit = "hour"
		}
		note += fmt.Sprintf(". %d %s%s of backfilled history carry cost but no token detail,"+
			" so the rate columns read high for them: re-run claude-obs backfill to record it",
			gap, unit, plural(gap))
	}
	return rows, note, nil
}

/* Delivery ------------------------------------------------------------------ */

// buildDelivery builds the delivery panel: the two live-only delivery
// counters (pull requests, commits), joined to sessions' whole cost and
// grouped by project through the sessions table. Both counters are live-only
// by nature (a transcript cannot say whether the command it ran opened a
// pull request), so this panel begins wherever the collector did.
func buildDelivery(ctx context.Context, qr history.RangeQuerier, d *sql.DB, scope profileScope, table history.Window) (prs, commits, sessionCost float64, rows []*claudeobsv1.DeliveryRow, err error) {
	prsMap, err := history.MergedTotals(ctx, qr, history.MergedParams{Kind: "pullRequests", By: []string{"session.id"}, Match: scope.match(), Start: table.Start, End: table.End, Step: table.Step})
	if err != nil {
		return 0, 0, 0, nil, err
	}
	commitsMap, err := history.MergedTotals(ctx, qr, history.MergedParams{Kind: "commits", By: []string{"session.id"}, Match: scope.match(), Start: table.Start, End: table.End, Step: table.Step})
	if err != nil {
		return 0, 0, 0, nil, err
	}

	var totalPrs, totalCommits float64
	for _, v := range prsMap {
		totalPrs += v
	}
	for _, v := range commitsMap {
		totalCommits += v
	}
	// Gates on the rounded totals, the same figures the tiles show: a total
	// that rounds to zero must not render as an empty panel with tiles.
	if math.Round(totalPrs) == 0 && math.Round(totalCommits) == 0 {
		return 0, 0, 0, nil, nil
	}

	costMap, err := history.MergedTotals(ctx, qr, history.MergedParams{Kind: "cost", By: []string{"session.id"}, Match: scope.match(), Start: table.Start, End: table.End, Step: table.Step})
	if err != nil {
		return 0, 0, 0, nil, err
	}

	sessionRows, ierr := listIndexedSessions(d, scope.Profile)
	if ierr != nil {
		return 0, 0, 0, nil, ierr
	}
	// Workspace names rather than project keys, which this panel can do and
	// the by-project cost attribution cannot: both counters here are
	// live-only, so every figure joins through session.id and there is no
	// backfilled half labelled the other way to disagree with.
	idToProject := workspacesFromSessions(sessionRows)

	type acc struct {
		prs, commits, cost float64
		sessions           int
	}
	byProject := map[string]*acc{}
	ids := map[string]bool{}
	for id := range prsMap {
		ids[id] = true
	}
	for id := range commitsMap {
		ids[id] = true
	}
	var deliveryCost float64
	for id := range ids {
		name, ok := idToProject[id]
		if !ok {
			name = notIndexed
		}
		row := byProject[name]
		if row == nil {
			row = &acc{}
			byProject[name] = row
		}
		row.prs += prsMap[id]
		row.commits += commitsMap[id]
		row.cost += costMap[id]
		row.sessions++
		deliveryCost += costMap[id]
	}

	rows = make([]*claudeobsv1.DeliveryRow, 0, len(byProject))
	for name, r := range byProject {
		rows = append(rows, &claudeobsv1.DeliveryRow{
			Project: name, Prs: r.prs, Commits: r.commits, Sessions: float64(r.sessions), SessionCostUsd: r.cost,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Prs != rows[j].Prs {
			return rows[i].Prs > rows[j].Prs
		}
		if rows[i].SessionCostUsd != rows[j].SessionCostUsd {
			return rows[i].SessionCostUsd > rows[j].SessionCostUsd
		}
		return rows[i].Project < rows[j].Project
	})

	return totalPrs, totalCommits, deliveryCost, rows, nil
}

/* Page ---------------------------------------------------------------------- */

// buildSpend is GetSpend's real body, taking now as a plain parameter for the
// same reason buildOverview does: deterministic tests. Four
// page-level sections (daily-cost, hour-heatmap, model-usage, delivery)
// degrade through SectionError, exactly like buildOverview's sections; the
// seven attributions degrade individually through their own Attribution.error
// field instead, so one broken breakdown never blanks the other six.
func buildSpend(ctx context.Context, o Options, period, measure, project, profile string, now int64) (*claudeobsv1.GetSpendResponse, error) {
	period = normalizePeriod(period)
	measure = normalizeMeasure(measure)
	win := periodBounds(period, now)

	resp := &claudeobsv1.GetSpendResponse{Identity: identity()}
	mvm := memoVM{client: o.VM, memo: o.Memo}
	// Resolved once, before any panel runs: every panel that narrows uses the
	// same profile and the same account, so two panels cannot end up scoped
	// differently within one response.
	scope := resolveProfileScope(ctx, o.VM, profile)
	var errs []*claudeobsv1.SectionError

	sections := []struct {
		name string
		f    func() error
	}{
		{"daily-cost", func() error {
			series, total, err := buildDailyCost(ctx, mvm, scope, period, win.Chart)
			if err != nil {
				return err
			}
			resp.DailyCost = series
			resp.DailyCostTotalUsd = total
			return nil
		}},
		{"hour-heatmap", func() error {
			res, err := mvm.QR(ctx, "sum(sum_over_time("+history.Selector("claude_hist_cost_usd", scope.match())+"[1h]))", win.Chart.Start, win.Chart.End, "3600")
			if err != nil {
				return err
			}
			resp.HeatmapHourly = heatmapPointsToProto(vm.Points(res))
			return nil
		}},
		{"model-usage", func() error {
			rows, note, err := buildModelUsage(ctx, mvm, scope, period, win.Table)
			if err != nil {
				return err
			}
			resp.ModelUsage = rows
			resp.ModelUsageNote = note
			return nil
		}},
		{"delivery", func() error {
			prs, commits, sessionCost, rows, err := buildDelivery(ctx, mvm, o.DB, scope, win.Table)
			if err != nil {
				return err
			}
			resp.DeliveryPrs = prs
			resp.DeliveryCommits = commits
			resp.DeliverySessionCostUsd = sessionCost
			resp.DeliveryRows = rows
			return nil
		}},
	}
	for _, s := range sections {
		section(s.name, &errs, s.f)
	}

	resp.Attributions = buildAttributions(ctx, mvm, o.DB, scope, measure, win, project)

	if measure == "billed" {
		resp.MeasureNote = measureNotApplied
	}

	resp.Errors = errs
	if len(errs) == len(sections) {
		return nil, fmt.Errorf("spend: all %d sections failed", len(sections))
	}
	return resp, nil
}

// cacheAge is how old the billed figure is, narrowed to the selected
// profile's account. The source label is already on the metric, so this
// cannot go through acct: it would produce two brace groups.
func (p profileScope) cacheAge() string {
	m := map[string]string{"source": "cache"}
	if p.Account != "" {
		m["account"] = p.Account
	}
	return "max(" + history.Selector("claude_plan_data_age_seconds", m) + ")"
}
