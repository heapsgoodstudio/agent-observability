package server

import (
	"context"
	"sort"
	"sync"

	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// The legacy snapshot's max-by-account projection is retained for consumers.
// Neutral cards select individual observations before deduplicating profiles,
// so an older high percentage cannot override a newer lower value after reset.
func applyClaudeObservations(ctx context.Context, o Options, accounts map[string]*pb.AccountSnapshot, now int64) {
	if o.VM == nil {
		for _, a := range accounts {
			summarizeClaudeConfirmation(a)
		}
		return
	}
	queries := []string{"claude_plan_window_pct", "claude_plan_window_reset_timestamp_seconds", "timestamp(claude_plan_data_age_seconds) - claude_plan_data_age_seconds", "timestamp(claude_plan_data_confirmed_age_seconds) - claude_plan_data_confirmed_age_seconds", "claude_extra_usage_usd", "claude_extra_usage_enabled"}
	results := make([][]vm.Series, len(queries))
	queryErrors := make([]error, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			res, err := o.VM.Q(ctx, q)
			queryErrors[i] = err
			if err == nil {
				results[i] = res
			}
		}(i, q)
	}
	wg.Wait()
	key := func(m map[string]string, source string) string {
		return m["account"] + "\x00" + m["profile"] + "\x00" + source
	}
	changed, confirmed := map[string]float64{}, map[string]float64{}
	for i, target := range []map[string]float64{changed, confirmed} {
		for _, s := range results[i+2] {
			if v, ok := vm.SeriesValue(s); ok && finite(v) && v > 0 {
				target[key(s.Metric, s.Metric["source"])] = v
			}
		}
	}
	resets := map[string]float64{}
	for _, s := range results[1] {
		if v, ok := vm.SeriesValue(s); ok && finite(v) {
			resets[key(s.Metric, s.Metric["source"])+"\x00"+s.Metric["window"]] = v
		}
	}
	type pick struct {
		window  *pb.AllowanceWindow
		changed float64
	}
	picked := map[string]map[string]pick{}
	observations := map[string]map[string][]*pb.AllowanceWindow{}
	for _, s := range results[0] {
		account := s.Metric["account"]
		if account == "" {
			continue
		}
		name := s.Metric["window"]
		if name == "" {
			continue
		}
		value, ok := vm.SeriesValue(s)
		if !ok || !finite(value) {
			continue
		}
		source := s.Metric["source"]
		k := key(s.Metric, source)
		ts, hasTime := changed[k]
		w := &pb.AllowanceWindow{Id: name, Label: claudeWindowLabel(name), Kind: name, BucketId: "claude", UsedPercent: number(value), RemainingPercent: number(100 - value), Source: source, State: "unknown"}
		if hasTime {
			w.ObservedAt = number(ts)
			w.State = "stale"
		}
		if v, ok := confirmed[k]; ok && source == "cache" {
			w.ConfirmedAt = number(v)
		} else if source == "statusline" {
			w.ConfirmedAt = w.ObservedAt
		}
		if w.ConfirmedAt != nil && *w.ConfirmedAt <= float64(now+60) && float64(now)-*w.ConfirmedAt <= staleAfterS {
			w.State = "fresh"
		}
		if reset, ok := resets[k+"\x00"+name]; ok {
			w.ResetsAt = number(reset)
		}
		if name == "five_hour" {
			w.DurationSeconds = number(18000)
		} else if name == "seven_day" || name == "weekly_scoped" {
			w.DurationSeconds = number(604800)
		}
		if picked[account] == nil {
			picked[account] = map[string]pick{}
			observations[account] = map[string][]*pb.AllowanceWindow{}
		}
		observations[account][name] = append(observations[account][name], w)
		old, exists := picked[account][name]
		if !exists || ts > old.changed || ts == old.changed && w.Source < old.window.Source {
			picked[account][name] = pick{w, ts}
		}
	}
	// Keep choosing the value by change time. A different source can confirm
	// that exact reading only when its value and known reset period agree.
	// This matters when a statusline stops rendering while the cache poller
	// continues confirming unchanged data. It never reconciles disagreements.
	for account, windows := range picked {
		for name, selected := range windows {
			w := selected.window
			for _, candidate := range observations[account][name] {
				if candidate == w || candidate.ConfirmedAt == nil || candidate.ResetsAt == nil || w.ResetsAt == nil ||
					candidate.UsedPercent == nil || w.UsedPercent == nil || *candidate.UsedPercent != *w.UsedPercent || *candidate.ResetsAt != *w.ResetsAt {
					continue
				}
				confirmedAt := *candidate.ConfirmedAt
				if confirmedAt > float64(now+60) || (w.ObservedAt != nil && confirmedAt < *w.ObservedAt) ||
					(w.ConfirmedAt != nil && confirmedAt <= *w.ConfirmedAt) {
					continue
				}
				w.ConfirmedAt = number(confirmedAt)
			}
			if w.ConfirmedAt != nil && *w.ConfirmedAt <= float64(now+60) && float64(now)-*w.ConfirmedAt <= staleAfterS {
				w.State = "fresh"
			}
		}
	}

	for _, a := range accounts {
		if a.Provider != "claude" {
			continue
		}
		windows, ok := picked[a.Account]
		if !ok {
			continue
		}
		// A successful requested live probe is stronger than cached VM data.
		probe := map[string]*pb.AllowanceWindow{}
		for _, w := range a.Windows {
			if w.Source == "probe" && w.Error == "" {
				probe[w.Id] = w
			}
		}
		a.Windows = nil
		a.ObservedAt = nil
		a.ConfirmedAt = nil
		allFresh := true
		anyTime := false
		for name, p := range windows {
			w := p.window
			if live := probe[name]; live != nil {
				w = live
			}
			a.Windows = append(a.Windows, w)
			allFresh = allFresh && w.State == "fresh"
			anyTime = anyTime || w.ObservedAt != nil
			if w.GetObservedAt() > a.GetObservedAt() {
				a.ObservedAt = w.ObservedAt
			}
		}
		sort.Slice(a.Windows, func(i, j int) bool { return a.Windows[i].Id < a.Windows[j].Id })
		if a.State != "unavailable" {
			a.State = "unknown"
			if anyTime {
				a.State = "stale"
			}
			if allFresh && a.Error == "" {
				a.State = "fresh"
			}
		}
		a.CurrentWindowId = ""
		a.WeeklyWindowId = ""
		selectWindowRoles(a)
	}
	// Billing uses its own selected cache observation, never a sum or max of
	// profiles sharing one cumulative account figure.
	for i := 4; i <= 5; i++ {
		seen := map[string]float64{}
		for _, s := range results[i] {
			account := s.Metric["account"]
			if account == "" {
				continue
			}
			v, ok := vm.SeriesValue(s)
			if !ok || !finite(v) {
				continue
			}
			ts := changed[key(s.Metric, "cache")]
			old, exists := seen[account]
			if exists && ts <= old {
				continue
			}
			seen[account] = ts
			for _, a := range accounts {
				if a.Provider != "claude" || a.Account != account {
					continue
				}
				b := a.Billing
				if i == 5 {
					enabled := v >= 1
					b.Enabled = &enabled
					continue
				}
				b.ReportedOverageUsd = number(v)
				b.ObservedAt = nil
				b.State = "unknown"
				if ts > 0 {
					b.ObservedAt = number(ts)
					b.State = "stale"
					if ts <= float64(now+60) && float64(now)-ts <= 21600 {
						b.State = "fresh"
					}
				}
			}
		}
	}
	// Keep a failed source visible alongside retained readings. Allowance and
	// billing failures stay separate, since one does not establish the other.
	for _, a := range accounts {
		if a.Provider != "claude" {
			continue
		}
		for i, err := range queryErrors {
			if err == nil {
				continue
			}
			if i < 4 {
				if a.Error == "" {
					a.Error = err.Error()
				}
				if a.State == "fresh" {
					a.State = "stale"
				}
				for _, w := range a.Windows {
					if w.Source == "probe" {
						continue
					}
					w.Error = err.Error()
					if w.State == "fresh" {
						w.State = "stale"
					}
				}
			} else {
				a.Billing.Note += " Billing source unavailable: " + err.Error() + "."
				if a.Billing.State == "fresh" {
					a.Billing.State = "stale"
				}
			}
		}
	}

	for _, a := range accounts {
		summarizeClaudeConfirmation(a)
	}

}

// Account confirmedAt means the oldest confirmation across every selected
// Claude window, including a retained response when VM queries fail. Absence
// on any window makes account confirmation unknown. observedAt remains the
// latest source observation (last value change for the cache); it is deliberately not a confirmation fallback.
func summarizeClaudeConfirmation(a *pb.AccountSnapshot) {
	if a.Provider != "claude" {
		return
	}
	a.ConfirmedAt = nil
	for _, w := range a.Windows {
		if w.ConfirmedAt == nil {
			a.ConfirmedAt = nil
			return
		}
		if a.ConfirmedAt == nil || *w.ConfirmedAt < *a.ConfirmedAt {
			a.ConfirmedAt = w.ConfirmedAt
		}
	}
}
