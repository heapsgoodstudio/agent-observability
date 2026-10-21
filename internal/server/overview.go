package server

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/history"
	"github.com/heapsgoodstudio/agent-observability/internal/overage"
	"github.com/heapsgoodstudio/agent-observability/internal/plan"
	"github.com/heapsgoodstudio/agent-observability/internal/pricing"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// Live counter metric names: what Claude Code's OTel exporter writes, also
// spelled as COST_METRIC/TOKEN_METRIC in config.env for the shell scripts.
// internal/history carries its own copy of the same two; health.go shares
// these.
const (
	costMetric  = "claude_code.cost.usage"
	tokenMetric = "claude_code.token.usage"
)

// cacheAgeQuery is read by both the "today" and "overage-curve" panels, which
// build independently; routing it through Memo means the two sections cost
// one round trip, not two.
const attributionTopN = 5
const attributionDays = 7

// memoVM adapts a *vm.Client to route every query through store.Memo: the
// same query (plus range params) within the TTL window is one request, not
// one per caller.
type memoVM struct {
	client *vm.Client
	memo   *store.Memo
}

func (m memoVM) Q(ctx context.Context, query string) ([]vm.Series, error) {
	v, err := m.memo.Do("q|"+query, func() (any, error) { return m.client.Q(ctx, query) })
	if err != nil {
		return nil, err
	}
	return v.([]vm.Series), nil
}

func (m memoVM) QR(ctx context.Context, query string, start, end int64, step string) ([]vm.Series, error) {
	key := fmt.Sprintf("qr|%s|%d|%d|%s", query, start, end, step)
	v, err := m.memo.Do(key, func() (any, error) { return m.client.QR(ctx, query, start, end, step) })
	if err != nil {
		return nil, err
	}
	return v.([]vm.Series), nil
}

// section runs f and, on failure, appends a SectionError rather than
// aborting: one panel's failure must not blank the rest of the page. The RPC
// itself only errors when every section failed (see buildOverview).
func section(name string, errs *[]*claudeobsv1.SectionError, f func() error) {
	if err := f(); err != nil {
		*errs = append(*errs, &claudeobsv1.SectionError{Section: name, Error: err.Error()})
	}
}

// monthStartLocal returns the first second of now's calendar month in the
// server's local timezone. Local, not UTC: "month to date" means the user's
// own calendar month. time.Unix returns a Time in Local, so Date()/Location()
// below already reflect that.
func monthStartLocal(now int64) int64 {
	t := time.Unix(now, 0)
	y, mo, _ := t.Date()
	return time.Date(y, mo, 1, 0, 0, 0, 0, t.Location()).Unix()
}

func pointsToProto(pts []vm.Point) []*claudeobsv1.Point {
	if len(pts) == 0 {
		return nil
	}
	out := make([]*claudeobsv1.Point, len(pts))
	for i, p := range pts {
		out[i] = &claudeobsv1.Point{Ts: p.TS, Value: p.V}
	}
	return out
}

func bandsToProto(bands [][2]float64) []*claudeobsv1.Band {
	if len(bands) == 0 {
		return nil
	}
	out := make([]*claudeobsv1.Band, len(bands))
	for i, b := range bands {
		out[i] = &claudeobsv1.Band{Start: b[0], End: b[1]}
	}
	return out
}

func windowsToProto(ws []plan.Window) []*claudeobsv1.PlanWindow {
	out := make([]*claudeobsv1.PlanWindow, len(ws))
	for i, w := range ws {
		out[i] = &claudeobsv1.PlanWindow{
			Window:      w.Name,
			Pct:         w.Pct,
			HasPct:      w.HasPct,
			ResetTs:     w.ResetTS,
			HasReset:    w.HasReset,
			PctSource:   w.PctSource,
			ResetSource: w.ResetSource,
		}
	}
	return out
}

// usd formats v with two decimal places and comma-grouped thousands. A
// deliberate duplicate of history.go's private usd(): small formatters stay
// local to each package rather than pulling packages together for three
// lines. Note internal/rules has a third, ungrouped one, on purpose.
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

// kv is one resolved attribution row before it is truncated to the top N and
// converted to an AttrRow.
type kv struct {
	Key   string
	Value float64
}

// computeAttribution merges cost by label over win, canonicalizing each key
// with canon, keeps only positive totals, sorts descending (ties broken on
// key, so the order is deterministic), and returns the top attributionTopN
// rows plus the note text rendered below them.
// shown is also returned so the by-model panel can compute its per-model
// usage line from the same top rows, without re-running the cost query.
func computeAttribution(ctx context.Context, qr history.RangeQuerier, scope profileScope, label string, canon func(string) string, win history.Window) (rows []*claudeobsv1.AttrRow, note string, shown []kv, err error) {
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
		Start: win.Start, End: win.End, Step: win.Step,
	})
	if err != nil {
		return nil, "", nil, err
	}

	all := make([]kv, 0, len(totals))
	for k, v := range totals {
		if v > 0 {
			all = append(all, kv{k, v})
		}
	}
	if len(all) == 0 {
		// Matches renderAttribution's early return: attrTable({rows: [],
		// empty: NO_COST}) carries no note, since there is nothing to
		// caveat, and no liveOnlyNote query is worth issuing for zero rows.
		return nil, "", nil, nil
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Value != all[j].Value {
			return all[i].Value > all[j].Value
		}
		return all[i].Key < all[j].Key
	})

	shown = all
	if len(shown) > attributionTopN {
		shown = shown[:attributionTopN]
	}

	rows = make([]*claudeobsv1.AttrRow, len(shown))
	for i, e := range shown {
		rows[i] = &claudeobsv1.AttrRow{Key: e.Key, Value: e.Value}
	}

	var total float64
	for _, e := range all {
		total += e.Value
	}

	var counted string
	switch {
	case len(shown) < len(all):
		counted = fmt.Sprintf("%s across %d values, top %d shown", usd(total), len(all), len(shown))
	case len(all) == 1:
		counted = fmt.Sprintf("%s across %d value", usd(total), len(all))
	default:
		counted = fmt.Sprintf("%s across %d values", usd(total), len(all))
	}

	liveNote, err := history.LiveOnlyNote(ctx, qr, label, total, win)
	if err != nil {
		return nil, "", nil, err
	}

	return rows, counted + liveNote, shown, nil
}

// computeModelUsage builds, for each of the by-model panel's shown rows, the
// token breakdown (output, cache read, cache creation, input) over the same
// window the cost row came from. A model with no token history at all is
// skipped rather than reported as 0%, which would read as a real measurement.
func computeModelUsage(ctx context.Context, qr history.RangeQuerier, scope profileScope, win history.Window, shown []kv) ([]*claudeobsv1.ModelUsageRow, error) {
	tokens, err := history.MergedTotals(ctx, qr, history.MergedParams{
		Kind:  "tokens",
		By:    []string{"model", "type"},
		Match: scope.match(),
		KeyOf: func(labels map[string]string, _ string) (string, bool) {
			model := labels["model"]
			if model == "" {
				model = "unknown"
			}
			return labels["type"] + ":" + pricing.ShortModel(model), true
		},
		Start: win.Start, End: win.End, Step: win.Step,
	})
	if err != nil {
		return nil, err
	}

	var rows []*claudeobsv1.ModelUsageRow
	for _, e := range shown {
		tok := func(t string) float64 { return tokens[t+":"+e.Key] }
		output := tok("output")
		read := tok("cacheRead")
		input := tok("input")
		creation := tok("cacheCreation")
		if output+read+input+creation == 0 {
			continue
		}
		rows = append(rows, &claudeobsv1.ModelUsageRow{
			Model:         e.Key,
			CostUsd:       e.Value,
			Output:        output,
			CacheRead:     read,
			CacheCreation: creation,
			Input:         input,
		})
	}
	return rows, nil
}

// buildOverview is GetOverview's real body, taking now as a plain parameter
// so tests can pin it without touching Options. Every section is independent:
// section() records a failure and moves on, and the whole call only errors
// when every section failed.
// profile, when non-empty, narrows the cost panels (burn, last 24 hours, the
// two attributions) to one profile. It deliberately does NOT narrow the
// allowance panels: plan windows, the billed figure and the overage curve
// belong to an ACCOUNT, and two profiles can share one. Those are resolved to
// the selected profile's account instead, which is what keeps one allowance
// from being drawn as two.
func buildOverview(ctx context.Context, o Options, profile string, now int64) (*claudeobsv1.GetOverviewResponse, error) {
	resp := &claudeobsv1.GetOverviewResponse{Identity: identity()}
	mvm := memoVM{client: o.VM, memo: o.Memo}
	// Resolved once, so every panel below narrows to the same profile and the
	// same account. See profileScope in spend.go.
	scope := resolveProfileScope(ctx, o.VM, profile)
	var errs []*claudeobsv1.SectionError

	sections := []struct {
		name string
		f    func() error
	}{
		{"billing", func() error {
			res, err := mvm.Q(ctx, "max("+scope.acct("claude_in_overage")+")")
			if err != nil {
				return err
			}
			v, ok := vm.Scalar(res)
			resp.OverageKnown = ok
			resp.InOverage = ok && v >= 1
			return nil
		}},
		{"freshness", func() error {
			res, err := mvm.Q(ctx, "min("+scope.acct("claude_plan_data_age_seconds")+")")
			if err != nil {
				return err
			}
			v, ok := vm.Scalar(res)
			resp.AgeKnown = ok
			resp.PlanDataAgeS = v

			// The confirmation age is fetched separately and its failure is
			// not this section's failure: it is the better freshness answer
			// but the older one still works, so a client falls back rather
			// than losing the panel. Absent stays absent, never zero, which
			// would read as "confirmed just now".
			confRes, confErr := mvm.Q(ctx, "min("+scope.acct("claude_plan_data_confirmed_age_seconds")+")")
			if confErr == nil {
				cv, cok := vm.Scalar(confRes)
				resp.ConfirmedAgeKnown = cok
				resp.PlanDataConfirmedAgeS = cv
			}
			return nil
		}},
		{"burn", func() error {
			perHourRes, err := mvm.Q(ctx, fmt.Sprintf("sum(rate(%s[15m]))*3600", history.Selector(costMetric, scope.match())))
			if err != nil {
				return err
			}
			trendRes, err := mvm.QR(ctx, fmt.Sprintf("sum(increase(%s[1d]))", history.Selector(costMetric, scope.match())), now-14*history.DAY, now, "1d")
			if err != nil {
				return err
			}
			v, ok := vm.Scalar(perHourRes)
			resp.BurnRecent = ok
			resp.BurnPerHourUsd = v
			resp.BurnDaily = pointsToProto(vm.Points(trendRes))
			return nil
		}},
		{"today", func() error {
			realRes, err := mvm.Q(ctx, "max(clamp_min(delta("+scope.acct("claude_extra_usage_usd")+"[24h]),0))")
			if err != nil {
				return err
			}
			notionalRes, err := mvm.Q(ctx, fmt.Sprintf("sum(clamp_min(increase(%s[24h]),0))", history.Selector(costMetric, scope.match())))
			if err != nil {
				return err
			}
			cacheAgeRes, err := mvm.Q(ctx, scope.cacheAge())
			if err != nil {
				return err
			}
			trendRes, err := mvm.QR(ctx, "max(clamp_min(delta("+scope.acct("claude_extra_usage_usd")+"[24h]),0))", now-14*history.DAY, now, "1d")
			if err != nil {
				return err
			}
			real, realOK := vm.Scalar(realRes)
			notional, notionalOK := vm.Scalar(notionalRes)
			cacheAge, cacheOK := vm.Scalar(cacheAgeRes)
			resp.TodayRealUsd = real
			resp.TodayRealKnown = realOK
			resp.TodayNotionalUsd = notional
			resp.TodayNotionalKnown = notionalOK
			resp.BilledCacheAgeS = cacheAge
			resp.BilledAgeKnown = cacheOK
			resp.TodayRealDaily = pointsToProto(vm.Points(trendRes))
			return nil
		}},
		{"meters", func() error {
			windows, ages, err := plan.LoadWindows(ctx, mvm)
			if err != nil {
				return err
			}
			resp.Windows = windowsToProto(windows)
			resp.SourceAges = ages
			return nil
		}},
		{"accounts", func() error {
			// Its own section, not folded into "meters": an install whose
			// poller predates the account labels answers these queries with
			// nothing, and an empty account list beside working flat fields
			// is the correct reading of that, not a failure.
			accounts, primary, err := loadAccounts(ctx, o.VM, o.PrimaryProfile)
			if err != nil {
				return err
			}
			resp.Accounts = accounts
			resp.PrimaryAccount = primary
			return nil
		}},
		{"overage-curve", func() error {
			if scope.Profile != "" && scope.Account == "" {
				return fmt.Errorf("the selected profile's billing account is unavailable")
			}
			monthStart := monthStartLocal(now)
			curveQuery := "max(" + scope.acct("claude_extra_usage_usd") + ")"
			if scope.Profile == "" {
				// Multiple profiles can share one bill. Sum distinct labelled
				// accounts, not profiles and not the largest account's bill.
				curveQuery = `sum(max by (account)(claude_extra_usage_usd{account!=""}))`
			}
			curveRes, err := mvm.QR(ctx, curveQuery, monthStart, now, "10m")
			if err != nil {
				return err
			}
			flagRes, err := mvm.QR(ctx, "max("+scope.bandSelector()+") == 1", monthStart, now, "10m")
			if err != nil {
				return err
			}
			cacheAgeRes, err := mvm.Q(ctx, scope.cacheAge())
			if err != nil {
				return err
			}
			cacheAge, cacheOK := vm.Scalar(cacheAgeRes)
			resp.BilledCacheAgeS = cacheAge
			resp.BilledAgeKnown = cacheOK

			pts := vm.Points(curveRes)
			if len(pts) == 0 {
				return nil
			}
			flagPts := vm.Points(flagRes)
			times := make([]float64, len(flagPts))
			for i, p := range flagPts {
				times[i] = p.TS
			}
			resp.OverageCurve = pointsToProto(pts)
			resp.OverageBands = bandsToProto(overage.GroupBands(times, 1200))
			resp.OverageMtdUsd = pts[len(pts)-1].V
			return nil
		}},
		{"by-model", func() error {
			win := history.RollingWindow(attributionDays, now, history.DAY)
			rows, note, shown, err := computeAttribution(ctx, mvm, scope, "model", pricing.ShortModel, win)
			if err != nil {
				return err
			}
			resp.ByModel_7D = rows
			resp.ByModelNote = note
			if len(shown) > 0 {
				usageRows, err := computeModelUsage(ctx, mvm, scope, win, shown)
				if err != nil {
					return err
				}
				resp.ModelUsage_7D = usageRows
			}
			return nil
		}},
		{"by-source", func() error {
			win := history.RollingWindow(attributionDays, now, history.DAY)
			rows, note, _, err := computeAttribution(ctx, mvm, scope, "query_source", func(v string) string { return v }, win)
			if err != nil {
				return err
			}
			resp.BySource_7D = rows
			resp.BySourceNote = note
			return nil
		}},
	}

	for _, s := range sections {
		section(s.name, &errs, s.f)
	}
	resp.Errors = errs
	if len(errs) == len(sections) {
		return nil, fmt.Errorf("overview: all %d sections failed", len(sections))
	}
	return resp, nil
}
