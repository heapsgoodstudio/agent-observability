package server

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

func (s *obsService) GetAccountHistory(ctx context.Context, req *connect.Request[pb.GetAccountHistoryRequest]) (*connect.Response[pb.GetAccountHistoryResponse], error) {
	out, err := buildAccountHistory(ctx, s.options(), req.Msg, s.o.now())
	if err != nil {
		return nil, dashboardError(err)
	}
	return connect.NewResponse(out), nil
}
func buildAccountHistory(ctx context.Context, o Options, req *pb.GetAccountHistoryRequest, now int64) (*pb.GetAccountHistoryResponse, error) {
	if req.AccountKey == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("account_key is required"))
	}
	scope, err := resolveDashboardScope(ctx, o, "", req.AccountKey, "", now)
	if err != nil {
		return nil, err
	}
	days := req.Days
	if days == 0 {
		days = 7
	}
	if days < 1 || days > 90 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("days must be between 1 and 90"))
	}
	from := now - int64(days)*86400
	out := &pb.GetAccountHistoryResponse{AccountKey: req.AccountKey, Provider: scope.Provider, From: float64(from), To: float64(now)}
	if profiles.Detected(profiles.Provider(scope.Provider)) {
		out.CoverageNote = "Pay-as-you-go providers report no allowance. Daily estimated spend is on the account overview."
		return out, nil
	}
	if scope.Provider == "codex" {
		rows, truncated, err := db.ListAllowanceObservations(o.DB, "codex", req.AccountKey, from, now)
		if err != nil {
			out.Errors = append(out.Errors, &pb.SectionError{Section: "allowance history", Error: err.Error()})
		}
		out.Windows = historySeries(rows, from, now, int64(monitor.QuotaFreshFor.Seconds()))
		out.CoverageNote = "Quota observations are retained for 90 days from collection, independently of dashboard visits. Gaps are not interpolated. Codex reports only the credits used so far in the current credit period, so no overage history is kept."
		if truncated {
			out.CoverageNote += " The response reached its observation limit; only the newest retained points are included."
		}
		if len(rows) == 0 {
			out.CoverageNote += " No quota observations have been retained for this scope and period."
		}
		return out, nil
	}
	account, err := url.QueryUnescape(strings.TrimPrefix(req.AccountKey, "claude:"))
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if strings.HasPrefix(strings.TrimPrefix(req.AccountKey, "claude:"), "profile:") {
		out.CoverageNote = "Account identity is unresolved. Historical account allowance and billing cannot be attributed from a current profile connection."
		return out, nil
	}
	if o.VM == nil {
		out.Errors = append(out.Errors, &pb.SectionError{Section: "history", Error: "metrics store unavailable"})
		return out, nil
	}
	loadClaudeHistory(ctx, o, account, from, now, out)
	if o.LegacyClaudeBillingAccount == req.AccountKey {
		loadLegacyClaudeBilling(ctx, o, from, now, out)
	}
	if len(out.Windows) == 0 && len(out.Billing) == 0 {
		out.CoverageNote += " No reliably timestamped observations are available for this period."
	}
	return out, nil
}

// Each reset period and every observation gap is a separate line. Missing
// reset metadata and downward readings cannot silently connect two periods.
func historySeries(rows []db.AllowanceObservation, from, to, gap int64) []*pb.AllowanceHistorySeries {
	groups := map[string][]db.AllowanceObservation{}
	for _, r := range rows {
		if r.ObservedAt < from || r.ObservedAt > to || !finite(r.UsedPercent) {
			continue
		}
		key := r.WindowID + "\x00" + strconv.FormatInt(r.ResetAt, 10) + "/" + strconv.FormatBool(r.ResetKnown || r.ResetAt != 0)
		groups[key] = append(groups[key], r)
	}
	var out []*pb.AllowanceHistorySeries
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		rs := groups[key]
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].ObservedAt < rs[j].ObservedAt })
		var current *pb.AllowanceHistorySeries
		var prev db.AllowanceObservation
		for _, r := range rs {
			if current != nil && r.ObservedAt == prev.ObservedAt {
				continue
			}
			split := current == nil || r.ObservedAt-prev.ObservedAt > gap || (!r.ResetKnown && r.ResetAt == 0 && r.UsedPercent < prev.UsedPercent)
			if split {
				next := &pb.AllowanceHistorySeries{WindowId: r.WindowID, Label: r.Label, Source: r.Source, CoverageNote: "Observed allowance; gaps and reset periods are separate segments."}
				if r.ResetKnown || r.ResetAt != 0 {
					next.ResetAt = number(float64(r.ResetAt))
				}
				if current != nil && r.ObservedAt-prev.ObservedAt > gap {
					next.Gaps = append(next.Gaps, &pb.Band{Start: float64(prev.ObservedAt), End: float64(r.ObservedAt)})
				}
				current = next
				out = append(out, current)
			}
			current.Points = append(current.Points, &pb.Point{Ts: float64(r.ObservedAt), Value: r.UsedPercent})
			prev = r
		}
	}
	return out
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

type historySample struct {
	labels    map[string]string
	ts, value float64
}

func historySamples(res []vm.Series, account string) []historySample {
	var out []historySample
	for _, s := range res {
		if s.Metric["account"] != account {
			continue
		}
		for _, p := range vm.Points([]vm.Series{s}) {
			if finite(p.TS) && finite(p.V) {
				out = append(out, historySample{s.Metric, p.TS, p.V})
			}
		}
	}
	return out
}
func sampleKey(s historySample, source string) string {
	return s.labels["profile"] + "\x00" + source + "\x00" + strconv.FormatFloat(s.ts, 'f', -1, 64)
}
func sampleMap(samples []historySample) map[string]float64 {
	out := map[string]float64{}
	for _, s := range samples {
		out[sampleKey(s, s.labels["source"])] = s.value
	}
	return out
}

func loadClaudeHistory(ctx context.Context, o Options, account string, from, to int64, out *pb.GetAccountHistoryResponse) {
	match := "{account=" + strconv.Quote(account) + "}"
	metrics := []string{"claude_plan_window_pct", "claude_plan_window_reset_timestamp_seconds", "claude_plan_data_age_seconds", "timestamp(claude_plan_data_age_seconds%s)", "claude_plan_data_confirmed_age_seconds", "timestamp(claude_plan_data_confirmed_age_seconds%s)", "claude_extra_usage_usd"}
	results := make([][]historySample, len(metrics))
	errs := make([]error, len(metrics))
	step := max(int64(300), (to-from)/3000)
	stepText := strconv.FormatInt(step, 10) + "s"
	var wg sync.WaitGroup
	for i, metric := range metrics {
		wg.Add(1)
		go func(i int, metric string) {
			defer wg.Done()
			query := metric + match
			if strings.Contains(metric, "%s") {
				query = fmt.Sprintf(metric, match)
			}
			res, err := o.VM.QR(ctx, query, from, to, stepText)
			errs[i] = err
			if err == nil {
				results[i] = historySamples(res, account)
			}
		}(i, metric)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			out.Errors = append(out.Errors, &pb.SectionError{Section: metrics[i], Error: err.Error()})
		}
	}
	ages, stamps := sampleMap(results[2]), sampleMap(results[3])
	confAges, confStamps := sampleMap(results[4]), sampleMap(results[5])
	resets := map[string]float64{}
	for _, s := range results[1] {
		resets[sampleKey(s, s.labels["source"])+"\x00"+s.labels["window"]] = s.value
	}
	// At each evaluation point choose the source with the newest value change,
	// not the largest percentage. Keep source confirmation separate.
	type candidate struct {
		row     db.AllowanceObservation
		changed float64
	}
	chosen := map[string]candidate{}
	for _, s := range results[0] {
		source := s.labels["source"]
		key := sampleKey(s, source)
		age, aok := ages[key]
		stamp, tok := stamps[key]
		if !aok || !tok || age < 0 || stamp > s.ts+60 {
			continue
		}
		changed := stamp - age
		if changed <= 0 || changed > s.ts+60 {
			continue
		}
		observed := changed
		if source == "cache" {
			if age, ok := confAges[key]; ok && age >= 0 {
				if ts, ok := confStamps[key]; ok && ts-age >= changed && ts-age <= s.ts+60 {
					observed = ts - age
				}
			}
		}
		if observed < float64(from) || observed > float64(to) {
			continue
		}
		resetValue, resetKnown := resets[key+"\x00"+s.labels["window"]]
		reset := int64(resetValue)
		window := s.labels["window"]
		if window == "" {
			continue
		}
		row := db.AllowanceObservation{Provider: "claude", AccountKey: db.AccountKey("claude", account, ""), WindowID: window, Label: claudeWindowLabel(window), Source: source, ObservedAt: int64(observed), ResetAt: reset, ResetKnown: resetKnown, UsedPercent: s.value}
		ck := window + "\x00" + strconv.FormatFloat(s.ts, 'f', -1, 64)
		old, ok := chosen[ck]
		if !ok || changed > old.changed || changed == old.changed && source < old.row.Source {
			chosen[ck] = candidate{row, changed}
		}
	}
	rows := make([]db.AllowanceObservation, 0, len(chosen))
	for _, c := range chosen {
		rows = append(rows, c.row)
	}
	out.Windows = historySeries(rows, from, to, max(600, step*2))
	out.Billing = claudeBillingSeries(results[6], ages, stamps, from, to, max(600, step*2), "Claude usage cache")
	out.CoverageNote = "Provider-reported month-to-date readings, not spending summed over the selected range. Historical samples use their recorded account identity and source observation time. Gaps, month boundaries and decreases remain separate; missing observations are unknown."
}

// Claude's extra_usage.used_credits is a monthly cumulative reading, not an
// incremental charge. Never sum readings, bridge resets or infer a final invoice.
func claudeBillingSeries(samples []historySample, ages, stamps map[string]float64, from, to, gap int64, source string) []*pb.BillingHistorySeries {
	billed := map[int64]float64{}
	for _, s := range samples {
		key := sampleKey(s, "cache")
		age, aok := ages[key]
		stamp, tok := stamps[key]
		if !aok || !tok || age < 0 || stamp > s.ts+60 || s.value < 0 {
			continue
		}
		observed := int64(stamp - age)
		if observed < from || observed > to {
			continue
		}
		if old, ok := billed[observed]; !ok || s.value > old {
			billed[observed] = s.value
		}
	}
	times := make([]int64, 0, len(billed))
	for ts := range billed {
		times = append(times, ts)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	var out []*pb.BillingHistorySeries
	var current *pb.BillingHistorySeries
	var prev int64
	var previous float64
	for _, ts := range times {
		period := "Month to date · " + time.Unix(ts, 0).UTC().Format("Jan 2006")
		if current == nil || period != current.Period || billed[ts] < previous || ts-prev > gap {
			next := &pb.BillingHistorySeries{Period: period, Currency: "USD", Source: source}
			if current != nil && ts-prev > gap {
				next.Gaps = append(next.Gaps, &pb.Band{Start: float64(prev), End: float64(ts)})
			}
			current = next
			out = append(out, current)
		}
		current.Points = append(current.Points, &pb.Point{Ts: float64(ts), Value: billed[ts]})
		prev, previous = ts, billed[ts]
	}
	return out
}

// Read the old unlabelled collector only under an explicit account assignment.
// Keep the source provenance and stored samples unchanged. In particular, never
// inherit the current primary profile/account or include profile-only records.
func loadLegacyClaudeBilling(ctx context.Context, o Options, from, to int64, out *pb.GetAccountHistoryResponse) {
	if o.LegacyClaudeBillingAccount == "" || o.LegacyClaudeBillingAccount != out.AccountKey || !strings.HasPrefix(out.AccountKey, "claude:") {
		return
	}
	match := `{account="",profile=""}`
	queries := []string{"claude_extra_usage_usd" + match, "claude_plan_data_age_seconds" + match, "timestamp(claude_plan_data_age_seconds" + match + ")"}
	step := max(int64(300), (to-from)/3000)
	results := make([][]historySample, len(queries))
	errs := make([]error, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			res, err := o.VM.QR(ctx, q, from, to, strconv.FormatInt(step, 10)+"s")
			errs[i] = err
			for _, sample := range historySamples(res, "") {
				if sample.labels["profile"] == "" {
					results[i] = append(results[i], sample)
				}
			}
		}(i, q)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			out.Errors = append(out.Errors, &pb.SectionError{Section: "legacy billing history", Error: err.Error()})
			return
		}
	}
	// Prefer account-labelled readings of the same source observation. One
	// early labelled reading must not hide all subsequent legacy history.
	attributed := map[float64]bool{}
	for _, series := range out.Billing {
		for _, point := range series.Points {
			attributed[point.Ts] = true
		}
	}
	var legacy []*pb.BillingHistorySeries
	for _, series := range claudeBillingSeries(results[0], sampleMap(results[1]), sampleMap(results[2]), from, to, max(600, step*2), "Claude usage cache · legacy collector, manually assigned") {
		var fragment *pb.BillingHistorySeries
		for _, point := range series.Points {
			if attributed[point.Ts] {
				fragment = nil // Do not bridge over a replacement reading.
				continue
			}
			if fragment == nil {
				fragment = &pb.BillingHistorySeries{Period: series.Period, Currency: series.Currency, Source: series.Source, Gaps: series.Gaps}
				legacy = append(legacy, fragment)
			}
			fragment.Points = append(fragment.Points, point)
		}
	}
	if len(legacy) > 0 {
		out.Billing = append(legacy, out.Billing...)
		out.CoverageNote += " Includes earlier single-account readings explicitly assigned to this account by the operator. Original records are unchanged."
	}
}
