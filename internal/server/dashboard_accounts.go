package server

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/payg"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

func dashboardError(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	return connect.NewError(connect.CodeUnavailable, err)
}
func number(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}
func buildFilteredUsageSnapshot(ctx context.Context, o Options, req *pb.GetUsageSnapshotRequest, now int64) (*pb.GetUsageSnapshotResponse, error) {
	scope, err := resolveDashboardScope(ctx, o, req.Provider, req.AccountKey, req.Profile, now)
	if err != nil {
		return nil, err
	}
	out, err := buildUsageSnapshotScoped(ctx, o, req.MaxAgeS, now, func(p *pb.ProfileRow) bool {
		provider := p.Provider
		if provider == "" {
			provider = "claude"
		}
		return (scope.Provider == "" || scope.Provider == provider) && (req.Profile == "" || req.Profile == p.Profile) && (scope.AccountKey == "" || profileMatchesAccount(provider, p, scope.AccountKey))
	})
	if err != nil {
		return nil, err
	}
	if scope.All {
		return out, nil
	}
	keep := map[string]bool{}
	for _, p := range scope.Profiles {
		keep[p] = true
	}
	accounts := out.Accounts[:0]
	for _, a := range out.Accounts {
		if scope.Provider != "" && a.Provider != scope.Provider {
			continue
		}
		if scope.AccountKey != "" && a.Key != scope.AccountKey {
			continue
		}
		if req.Profile != "" {
			found := false
			for _, p := range a.Profiles {
				found = found || p.Profile == req.Profile
			}
			if !found {
				continue
			}
		}
		accounts = append(accounts, a)
	}
	out.Accounts = accounts
	legacy := out.Profiles[:0]
	for _, p := range out.Profiles {
		if scope.Provider == "codex" {
			continue
		}
		if len(keep) > 0 && !keep[p.Profile] {
			continue
		}
		legacy = append(legacy, p)
	}
	out.Profiles = legacy
	return out, nil
}

// VM's sample timestamp matters: now-age would make a dead poller appear fresh.
func sourceTimes(ctx context.Context, o Options, metric string) map[string]float64 {
	out := map[string]float64{}
	if o.VM == nil {
		return out
	}
	res, err := o.VM.Q(ctx, "max by (account,source)(timestamp("+metric+") - "+metric+")")
	if err != nil {
		return out
	}
	for _, s := range res {
		if v, ok := vm.SeriesValue(s); ok && v > 0 && !math.IsInf(v, 0) {
			out[s.Metric["account"]+"\x00"+s.Metric["source"]] = v
		}
	}
	return out
}

func buildDashboardAccounts(ctx context.Context, o Options, roster []*pb.ProfileRow, legacy []*pb.UsageProfile, now int64) []*pb.AccountSnapshot {
	changed := sourceTimes(ctx, o, "claude_plan_data_age_seconds")
	confirmed := sourceTimes(ctx, o, "claude_plan_data_confirmed_age_seconds")
	old := map[string]*pb.UsageProfile{}
	for _, p := range legacy {
		old[p.Profile] = p
	}
	configs := map[string]profiles.Profile{}
	if o.Profiles != nil {
		if ps, err := o.Profiles.List(); err == nil {
			for _, p := range ps {
				configs[p.ID] = p
			}
		}
	}
	grouped := map[string]*pb.AccountSnapshot{}
	spendRows := map[string][]*pb.ProfileRow{}
	for _, p := range roster {
		provider := p.Provider
		if provider == "" {
			provider = "claude"
			p.Provider = provider
			p.Enabled = true
			p.DisplayName = p.Profile
		}
		if cfg, ok := configs[p.Profile]; ok && !cfg.Enabled {
			continue
		}
		if profiles.Detected(profiles.Provider(provider)) {
			spendRows[provider] = append(spendRows[provider], p)
			continue
		}
		key := db.AccountKey(provider, p.Account, p.Profile)
		a := &pb.AccountSnapshot{Key: key, Provider: provider, Account: p.Account, DisplayName: p.DisplayName, Profiles: []*pb.ProfileRow{p}, State: "unknown", IdentityState: "observed", Billing: &pb.BillingSnapshot{State: "unknown"}, Capabilities: []string{"activity", "allowance", "allowance_history"}}
		if a.DisplayName == "" {
			a.DisplayName = p.Profile
		}
		if p.Account == "" {
			a.IdentityState = "unresolved"
			a.CoverageNote = "Account identity has not been reported; this connection is kept separate."
		}
		if provider == "codex" {
			a.Billing = &pb.BillingSnapshot{State: "unavailable", Note: codexNoCreditNote}

			if cfg, ok := configs[p.Profile]; ok {
				fillCodexAccount(o, cfg, p, a, now)
				key = a.Key
			}
			fillCodexCreditBilling(a, o.CodexCreditUSD)

		} else {
			a.Capabilities = append(a.Capabilities, "reported_overage", "billing_history", "findings", "analysis", "advice")
			if l := old[p.Profile]; l != nil {
				a.Error = l.Error
				if v, ok := changed[p.Account+"\x00cache"]; ok {
					a.ObservedAt = number(v)
				}
				if v, ok := confirmed[p.Account+"\x00cache"]; ok {
					a.ConfirmedAt = number(v)
				}
				if l.Probed && l.Error == "" && l.UsageAvailable {
					a.ConfirmedAt = number(float64(now))
				}
				if a.ConfirmedAt != nil {
					a.State = "fresh"
					if float64(now)-*a.ConfirmedAt > staleAfterS || *a.ConfirmedAt > float64(now+60) {
						a.State = "stale"
					}
				} else if a.ObservedAt != nil {
					a.State = "stale"
				}
				if l.Error != "" {
					a.State = "stale"
				}
				if l.UsageAvailableKnown && !l.UsageAvailable {
					a.State = "unavailable"
				}
				for _, w := range l.Windows {
					win := &pb.AllowanceWindow{Id: w.Window, Label: claudeWindowLabel(w.Window), Kind: w.Window, BucketId: "claude", Source: w.Source, State: a.State, Error: l.Error}
					if w.HasPct {
						win.UsedPercent = number(w.Pct)
						win.RemainingPercent = number(100 - w.Pct)
					}
					if w.HasReset {
						win.ResetsAt = number(w.ResetTs)
					}
					if v, ok := changed[p.Account+"\x00"+w.Source]; ok {
						win.ObservedAt = number(v)
					}
					if w.Source == "cache" {
						win.ConfirmedAt = a.ConfirmedAt
					} else if w.Source == "probe" {
						win.ConfirmedAt = a.ConfirmedAt
						win.ObservedAt = a.ConfirmedAt
					} else {
						win.ConfirmedAt = win.ObservedAt
					}
					if w.Window == "five_hour" {
						win.DurationSeconds = number(5 * 3600)
					} else if w.Window == "seven_day" || w.Window == "weekly_scoped" {
						win.DurationSeconds = number(7 * 86400)
					}
					if win.UsedPercent == nil {
						win.State = "unknown"
					}
					a.Windows = append(a.Windows, win)
				}
				b := a.Billing
				b.Currency = "USD"
				b.Period = "Month to date at observation"
				b.Note = "Provider-reported month-to-date overage from the Claude usage cache, not a total for the selected history range or a final invoice."
				b.PricingSource = "Claude usage cache"
				b.ObservedAt = a.ObservedAt
				if l.OverageKnown {
					v := l.OverageEnabled
					b.Enabled = &v
				}
				if l.ExtraUsageKnown {
					b.ReportedOverageUsd = number(l.ExtraUsageUsd)
					b.State = "stale"
					if b.ObservedAt != nil && *b.ObservedAt <= float64(now+60) && float64(now)-*b.ObservedAt <= 21600 {
						b.State = "fresh"
					}
				}
			}
		}
		selectWindowRoles(a)
		if previous, ok := grouped[key]; ok {
			linked := append(previous.Profiles, p)
			if accountNewer(a, previous) {
				a.Profiles = linked
				grouped[key] = a
			} else {
				previous.Profiles = linked
			}
		} else {
			grouped[key] = a
		}
	}
	applyClaudeObservations(ctx, o, grouped, now)
	for harness, rows := range spendRows {
		for _, a := range spendAccounts(o, harness, rows, now) {
			grouped[a.Key] = a
		}
	}
	out := make([]*pb.AccountSnapshot, 0, len(grouped))
	for _, a := range grouped {
		sort.Slice(a.Profiles, func(i, j int) bool { return a.Profiles[i].Profile < a.Profiles[j].Profile })
		if len(a.Profiles) > 1 && a.Profiles[0].Domain != "" {
			a.DisplayName = a.Profiles[0].Domain
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func fillCodexAccount(o Options, cfg profiles.Profile, p *pb.ProfileRow, a *pb.AccountSnapshot, now int64) {
	snap, err := monitor.ReadSnapshot(o.DB, p.Profile)
	if err != nil {
		a.Error = "Waiting for the first quota observation"
		return
	}
	snap = currentCodexSnapshot(cfg, snap)
	c := codexAccountProto(cfg, snap, time.Unix(now, 0))
	a.Account = c.Account
	p.Account = c.Account
	a.Key = db.AccountKey("codex", c.Account, p.Profile)
	if c.Account == "" {
		a.IdentityState = "unresolved"
		a.CoverageNote = "Account identity has not been reported; this connection is kept separate."
	} else {
		a.IdentityState = "observed"
		a.CoverageNote = ""
	}
	a.Plan = c.PlanType
	a.IncludedUsageAllowed = c.OrdinaryUsageAllowed
	a.CodexBuckets = c.Buckets
	a.ResetCreditsAvailable = c.ResetCreditsAvailable
	a.Error = snap.UsageError
	if c.FetchedAt > 0 {
		a.ObservedAt = number(c.FetchedAt)
		a.ConfirmedAt = number(c.FetchedAt)
		a.State = "fresh"
		if c.Stale {
			a.State = "stale"
		}
	}
	for _, w := range snap.Usage.Windows {
		win := &pb.AllowanceWindow{Id: db.AllowanceWindowID(w.LimitID, w.Kind), Label: codexWindowName(w), Kind: w.Kind, BucketId: w.LimitID, UsedPercent: number(w.UsedPercent), RemainingPercent: number(100 - w.UsedPercent), ObservedAt: a.ObservedAt, ConfirmedAt: a.ConfirmedAt, State: a.State, Source: "codex", Error: a.Error}
		if w.WindowMinutesKnown {
			win.DurationSeconds = number(float64(w.WindowMinutes * 60))
		}
		if w.ResetsAtKnown {
			win.ResetsAt = number(float64(w.ResetsAt))
		}
		for _, b := range snap.Usage.Buckets {
			if b.LimitID == w.LimitID {
				win.Model = b.NormalModelSlug
			}
		}
		a.Windows = append(a.Windows, win)
	}
}

func accountNewer(a, b *pb.AccountSnapshot) bool {
	at, bt := a.GetObservedAt(), b.GetObservedAt()
	if at != bt {
		return at > bt
	}
	if a.State == "fresh" && b.State != "fresh" {
		return true
	}
	return a.GetConfirmedAt() > b.GetConfirmedAt()
}
func claudeWindowLabel(kind string) string {
	switch kind {
	case "five_hour":
		return "5-hour allowance"
	case "seven_day":
		return "Weekly allowance"
	case "weekly_scoped":
		return "Model weekly allowance"
	}
	return kind
}
func selectWindowRoles(a *pb.AccountSnapshot) {
	for _, w := range a.Windows {
		if w.Kind == "five_hour" || w.Kind == "primary" && (a.CurrentWindowId == "" || w.BucketId == "codex") {
			a.CurrentWindowId = w.Id
		}
		if w.Kind == "seven_day" || w.DurationSeconds != nil && *w.DurationSeconds == 7*86400 && (a.WeeklyWindowId == "" || w.BucketId == "codex") {
			a.WeeklyWindowId = w.Id
		}
	}
	if a.CurrentWindowId == "" && len(a.Windows) > 0 {
		a.CurrentWindowId = a.Windows[0].Id
	}
}

// spendAccountKey is one inference provider's account across every profile of
// one pay-as-you-go harness ("omp:baseten", "opencode:baseten"). Neither
// harness records the provider-side account, only the provider, so all of a
// harness's Baseten usage is one Baseten account, and the same provider used
// from two harnesses is two accounts, one under each harness.
func spendAccountKey(harness, inference string) string { return db.AccountKey(harness, inference, "") }

// profileMatchesAccount reports whether a roster profile belongs to an account
// key. A detected-provider harness profile belongs to every account of its
// harness, because any one of its sessions may have used several providers.
func profileMatchesAccount(provider string, p *pb.ProfileRow, key string) bool {
	if profiles.Detected(profiles.Provider(provider)) {
		return strings.HasPrefix(key, provider+":")
	}
	return db.AccountKey(provider, p.Account, p.Profile) == key
}

// spendAccounts turns one pay-as-you-go harness's recorded usage into one
// account per inference provider. There is no allowance to report, so the
// account has no windows; its billing is the harness's estimate, which
// BillingSnapshot keeps in the estimated field, apart from anything a
// provider reported.
func spendAccounts(o Options, harness string, rows []*pb.ProfileRow, now int64) []*pb.AccountSnapshot {
	if len(rows) == 0 || o.DB == nil {
		return nil
	}
	name := profiles.ProviderName(profiles.Provider(harness))
	var buckets []payg.Bucket
	oldest := int64(0)
	var errs []string
	for _, p := range rows {
		var snap monitor.SpendSnapshot
		if monitor.ReadJSON(o.DB, monitor.SpendSnapshotKey(profiles.Provider(harness), p.Profile), &snap) != nil || snap.CollectedAt == 0 {
			errs = append(errs, p.DisplayName+": waiting for the first collection")
			continue
		}
		if snap.Error != "" {
			errs = append(errs, p.DisplayName+": "+snap.Error)
		}
		if oldest == 0 || snap.CollectedAt < oldest {
			oldest = snap.CollectedAt
		}
		buckets = append(buckets, snap.Usage...)
	}
	month, providers := buildPayg(buckets, time.Unix(now, 0))
	var hidden map[string]bool
	if o.Profiles != nil {
		hidden, _ = o.Profiles.HiddenAccounts()
	}
	out := make([]*pb.AccountSnapshot, 0, len(providers))
	for _, pp := range providers {
		// An account with no recorded cost (a local model, or one the harness
		// cannot price) is still an account: its token usage is its headline,
		// and it carries no estimated_spend capability, so no client shows
		// a dollar figure as its spend.
		plan, capabilities := "Pay-as-you-go", []string{"activity", "estimated_spend"}
		if !pp.CostRecorded {
			plan, capabilities = "No cost recorded", []string{"activity", "token_usage"}
		}
		a := &pb.AccountSnapshot{
			Key: spendAccountKey(harness, pp.Provider), Provider: harness, Account: pp.Provider,
			DisplayName: inferenceName(pp.Provider), Plan: plan,
			Profiles: rows, State: "unknown", IdentityState: "observed",
			Capabilities:      capabilities,
			MonthToDateTokens: number(pp.MonthToDateTokens),
			Hidden:            hidden[spendAccountKey(harness, pp.Provider)],
			CoverageNote:      "Requests " + name + " sent from this machine. Other clients on this provider account are not included.",
			Error:             strings.Join(errs, "; "),
		}
		b := &pb.BillingSnapshot{
			Currency: "USD", Period: "Month to date (" + month + ")", State: "unknown",
			PricingSource: name + " model catalog",
			Note:          name + "'s estimate: provider-reported tokens at public API rates, not a bill.",
		}
		if pp.CostRecorded && (pp.MonthToDateComplete || pp.MonthToDateUsd > 0) {
			b.EstimatedOverageUsd = number(pp.MonthToDateUsd)
			b.EstimatedOverageComplete = &pp.MonthToDateComplete
		}
		if !pp.CostRecorded {
			b.Note = name + " recorded no cost for these requests: a local model, or one it has no price for. Token usage is shown instead."
		}
		if oldest > 0 {
			a.ObservedAt = number(float64(oldest))
			a.ConfirmedAt = a.ObservedAt
			b.ObservedAt = a.ObservedAt
			a.State, b.State = "fresh", "fresh"
			if now-oldest > indexStaleS {
				a.State, b.State = "stale", "stale"
			}
		}
		a.Billing = b
		out = append(out, a)
	}
	return out
}

// spendAccountExists reports whether key names one of the accounts
// buildDashboardAccounts would list for the enabled profiles of the harness
// the key's prefix names.
func spendAccountExists(o Options, roster []*pb.ProfileRow, key string, now int64) bool {
	harness, _, _ := strings.Cut(key, ":")
	var rows []*pb.ProfileRow
	for _, p := range roster {
		if p.Provider == harness && p.Enabled {
			rows = append(rows, p)
		}
	}
	for _, a := range spendAccounts(o, harness, rows, now) {
		if a.Key == key {
			return true
		}
	}
	return false
}

// inferenceName is an inference provider's display name. The frontend's
// providerName map in frontend/src/lib/providers.ts spells the same names.
func inferenceName(p string) string {
	switch p {
	case "baseten":
		return "Baseten"
	case "fireworks":
		return "Fireworks AI"
	case "lms", "lm-studio":
		return "LM Studio"
	}
	if p == "" {
		return p
	}
	return strings.ToUpper(p[:1]) + p[1:]
}
