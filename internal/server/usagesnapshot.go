package server

import (
	"context"
	"sort"
	"sync"
	"time"

	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/plan"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
	"github.com/heapsgoodstudio/agent-observability/internal/usage"
)

// changedAgeQuery is how long since each profile's figures last moved, per
// profile rather than per account: it is the poller's own per-profile series,
// and a caller deciding where to put work is choosing a profile.
const changedAgeQuery = `min by (profile)(claude_plan_data_age_seconds{source="cache"})`

// confirmedAgeByProfileQuery is how long since a live fetch last confirmed
// them. This is what max_age_s is measured against, not the query above: see
// GetUsageSnapshotRequest.max_age_s.
const confirmedAgeByProfileQuery = `min by (profile)(claude_plan_data_confirmed_age_seconds)`

// maxProbeWait bounds the whole probing phase, however many profiles are
// stale. Probes run in parallel, so this is close to one probe's own timeout
// rather than the sum of them, and a caller that asked for freshness gets a
// bounded wait rather than an open one.
const maxProbeWait = 45 * time.Second

// buildUsageSnapshot answers "which accounts have headroom right now".
//
// Two phases, and the first one is usually the whole answer. It assembles
// every profile from the poller's store, which costs a handful of instant
// queries and no processes. Then, only when max_age_s asks for something
// fresher than the store holds, it probes the profiles that fall short, in
// parallel, and overlays what comes back.
//
// A failed probe does NOT blank a row. The store's figures are still the last
// thing known to be true, so they stay, the row records the error, and the
// caller sees stale-but-real numbers with an explanation rather than a gap it
// has to interpret. Blanking would read as "this profile has no usage", which
// is the one reading that could put work somewhere it does not belong.
func buildUsageSnapshot(ctx context.Context, o Options, maxAgeS int32, now int64) (*claudeobsv1.GetUsageSnapshotResponse, error) {
	return buildUsageSnapshotScoped(ctx, o, maxAgeS, now, nil)
}

func buildUsageSnapshotScoped(ctx context.Context, o Options, maxAgeS int32, now int64, include func(*claudeobsv1.ProfileRow) bool) (*claudeobsv1.GetUsageSnapshotResponse, error) {
	roster, err := dashboardProfileRoster(ctx, o, now)
	if err != nil {
		return nil, err
	}

	resp := &claudeobsv1.GetUsageSnapshotResponse{
		Identity:       identity(),
		PrimaryProfile: o.PrimaryProfile,
		GeneratedAt:    time.Unix(now, 0).UTC().Format(time.RFC3339),
		Errors:         roster.Errors,
	}

	var windows []plan.AccountWindows
	var werr error
	if o.VM != nil {
		windows, werr = plan.LoadAccountWindows(ctx, o.VM)
	}
	if werr != nil {
		resp.Errors = append(resp.Errors, &claudeobsv1.SectionError{Section: "windows", Error: werr.Error()})
	}
	byAccount := map[string][]*claudeobsv1.PlanWindow{}
	for _, aw := range windows {
		byAccount[aw.Account] = windowsToProto(aw.Windows)
	}

	changed := scalarByProfile(ctx, o, changedAgeQuery)
	confirmed := scalarByProfile(ctx, o, confirmedAgeByProfileQuery)
	billing := accountBilling(ctx, o)

	for _, r := range roster.Profiles {
		if include != nil && !include(r) {
			continue
		}
		if r.Provider != "" && r.Provider != "claude" {
			continue
		}
		p := &claudeobsv1.UsageProfile{
			Profile:             r.Profile,
			Account:             r.Account,
			Domain:              r.Domain,
			UsageAvailable:      r.UsageAvailable,
			UsageAvailableKnown: r.UsageAvailableKnown,
		}
		for _, w := range byAccount[r.Account] {
			p.Windows = append(p.Windows, &claudeobsv1.UsageWindow{
				Window: w.Window, Pct: w.Pct, HasPct: w.HasPct,
				ResetTs: w.ResetTs, HasReset: w.HasReset,
				Source: w.PctSource,
			})
		}
		if v, ok := changed[r.Profile]; ok {
			p.ChangedAgeS, p.ChangedAgeKnown = v, true
		}
		if v, ok := confirmed[r.Profile]; ok {
			p.ConfirmedAgeS, p.ConfirmedAgeKnown = v, true
		}
		if b, ok := billing[r.Account]; ok {
			p.OverageEnabled, p.OverageKnown = b.enabled, b.enabledKnown
			p.ExtraUsageUsd, p.ExtraUsageKnown = b.usd, b.usdKnown
		}
		resp.Profiles = append(resp.Profiles, p)
	}

	resp.OnDemandProbeEnabled = o.Prober != nil && len(o.ProfileDirs) > 0
	if maxAgeS > 0 && resp.OnDemandProbeEnabled {
		refreshStale(ctx, o, resp.Profiles, float64(maxAgeS))
	}
	sortUsageProfiles(resp.Profiles)
	resp.SchemaVersion = 2
	resp.Accounts = buildDashboardAccounts(ctx, o, roster.Profiles, resp.Profiles, now)
	return resp, nil
}

// needsProbe decides whether one row falls short of what the caller asked for.
//
// An unknown confirmation age counts as stale, because "nothing has ever
// confirmed this" is further from fresh than any number, not closer. A profile
// Claude Code says it cannot fetch usage for is NOT probed: the answer is
// already known and probing it again would spend three seconds to be told the
// same thing.
func needsProbe(p *claudeobsv1.UsageProfile, maxAge float64) bool {
	if p.UsageAvailableKnown && !p.UsageAvailable {
		return false
	}
	if !p.ConfirmedAgeKnown {
		return true
	}
	return p.ConfirmedAgeS > maxAge
}

// refreshStale probes every row that falls short, in parallel, and overlays
// what comes back.
func refreshStale(ctx context.Context, o Options, rows []*claudeobsv1.UsageProfile, maxAge float64) {
	if o.Prober == nil || len(o.ProfileDirs) == 0 {
		// No prober configured is not an error on this path: the store's
		// answer is still an answer, and the ages on it say how fresh it is.
		return
	}
	dirByProfile := map[string]string{}
	if o.Profiles != nil {
		ps, err := o.Profiles.List()
		if err != nil {
			return
		}
		for _, p := range ps {
			if string(p.Provider) == "claude" && p.Enabled {
				dirByProfile[p.ID] = p.Directory
			}
		}
	} else {
		for _, dir := range o.ProfileDirs {
			dirByProfile[profileNameOf(dir)] = dir
		}
	}

	ctx, cancel := context.WithTimeout(ctx, maxProbeWait)
	defer cancel()

	var wg sync.WaitGroup
	for _, row := range rows {
		dir, ok := dirByProfile[row.Profile]
		if !ok || !needsProbe(row, maxAge) {
			continue
		}
		wg.Add(1)
		go func(row *claudeobsv1.UsageProfile, dir string) {
			defer wg.Done()
			res, err := o.Prober.Probe(ctx, dir)
			if err != nil {
				// The store's figures stay. See buildUsageSnapshot.
				row.Error = err.Error()
				return
			}
			applyProbe(row, res)
		}(row, dir)
	}
	wg.Wait()
}

// applyProbe overlays a live reading onto a row.
//
// The confirmation age becomes 0 because this IS the confirmation. The change
// age is deliberately left alone: it is still true that the figures last moved
// whenever the poller last saw them move, and a live fetch returning the same
// numbers does not make them newer.
func applyProbe(row *claudeobsv1.UsageProfile, res usage.Result) {
	row.Probed = true
	row.UsageAvailable, row.UsageAvailableKnown = res.Available, true
	if !res.Available {
		// A profile that cannot fetch usage has been answered, so this counts
		// as a confirmation: the answer is "there is nothing to fetch".
		row.ConfirmedAgeS, row.ConfirmedAgeKnown = 0, true
		return
	}
	if len(res.Windows) == 0 {
		// Signed in, but no window parsed out of the response. That is a shape
		// change in Claude Code's payload, not a reading, and stamping a
		// confirmation here would claim the stored figures had just been
		// checked when nothing checked them. The error field is what the
		// caller reads instead.
		row.Error = "probe returned no plan windows"
		return
	}
	row.ConfirmedAgeS, row.ConfirmedAgeKnown = 0, true
	byName := map[string]*claudeobsv1.UsageWindow{}
	for _, w := range row.Windows {
		byName[w.Window] = w
	}
	for name, w := range res.Windows {
		target, ok := byName[name]
		if !ok {
			target = &claudeobsv1.UsageWindow{Window: name}
			row.Windows = append(row.Windows, target)
			byName[name] = target
		}
		target.Pct, target.HasPct = w.Pct, true
		target.Source = "probe"
		if w.HasReset {
			target.ResetTs, target.HasReset = float64(w.ResetTS), true
		}
	}
	sort.Slice(row.Windows, func(i, j int) bool { return row.Windows[i].Window < row.Windows[j].Window })
}

// sortUsageProfiles puts the account closest to throttling first, ranked on
// its highest window, with unknown rows last and ties broken on name so the
// order is stable across calls.
func sortUsageProfiles(rows []*claudeobsv1.UsageProfile) {
	peak := func(p *claudeobsv1.UsageProfile) (float64, bool) {
		best, ok := -1.0, false
		for _, w := range p.Windows {
			if w.HasPct && w.Pct > best {
				best, ok = w.Pct, true
			}
		}
		return best, ok
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, aok := peak(rows[i])
		b, bok := peak(rows[j])
		if aok != bok {
			return aok
		}
		if aok && a != b {
			return a > b
		}
		return rows[i].Profile < rows[j].Profile
	})
}

// profileNameOf derives a profile's name from its directory, the same way
// installer.ProfileName, bin/limits-poller.sh's profile_name_of and
// bin/statusline-tap.sh's profile_name do. Strip .claude-, then .claude, and
// call an empty result "default".
func profileNameOf(dir string) string {
	base := dir
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	if i := lastSlash(base); i >= 0 {
		base = base[i+1:]
	}
	if len(base) >= 8 && base[:8] == ".claude-" {
		base = base[8:]
	} else if base == ".claude" {
		base = ""
	}
	if base == "" {
		return "default"
	}
	return base
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

type billingFacts struct {
	enabled      bool
	enabledKnown bool
	usd          float64
	usdKnown     bool
}

// accountBilling reads the two billing figures per account. A query failure
// leaves the facts unknown rather than false: "cannot be billed" and "nobody
// checked" are different, and only one of them is safe to act on.
func accountBilling(ctx context.Context, o Options) map[string]billingFacts {
	out := map[string]billingFacts{}
	if o.VM == nil {
		return out
	}
	set := func(query string, f func(*billingFacts, float64)) {
		res, err := o.VM.Q(ctx, query)
		if err != nil {
			return
		}
		for _, s := range res {
			acct := s.Metric["account"]
			if acct == "" {
				continue
			}
			if v, ok := vm.SeriesValue(s); ok {
				b := out[acct]
				f(&b, v)
				out[acct] = b
			}
		}
	}
	set(accountExtraOnQuery, func(b *billingFacts, v float64) { b.enabled, b.enabledKnown = v >= 1, true })
	set(accountExtraUsdQuery, func(b *billingFacts, v float64) { b.usd, b.usdKnown = v, true })
	return out
}

// scalarByProfile runs one instant query and keys its samples by profile.
// A failure returns an empty map, which every caller reads as "unknown".
func scalarByProfile(ctx context.Context, o Options, query string) map[string]float64 {
	out := map[string]float64{}
	if o.VM == nil {
		return out
	}
	res, err := o.VM.Q(ctx, query)
	if err != nil {
		return out
	}
	for _, s := range res {
		if p := s.Metric["profile"]; p != "" {
			if v, ok := vm.SeriesValue(s); ok {
				out[p] = v
			}
		}
	}
	return out
}
