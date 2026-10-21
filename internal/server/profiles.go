package server

import (
	"context"
	"errors"
	"sort"

	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/plan"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// usageAvailableQuery reads the poller's per-profile "can this profile fetch
// usage at all" flag. It is per PROFILE rather than per account, unlike every
// other query in accounts.go, because signing in is a property of the profile
// directory: two profiles on one account can have one working credential and
// one lapsed.
const usageAvailableQuery = `max by (profile)(claude_profile_usage_available)`

// errNoMetricsStore is the one failure that leaves ListProfiles nothing to
// return: the roster exists only as poller series, so an install with no
// metrics store configured has no profiles to name.
var errNoMetricsStore = errors.New("profiles: no metrics store configured")

// buildListProfiles assembles the roster behind the global profile selector.
//
// It is built from claude_profile_account_info rather than from the account
// windows, deliberately. loadAccounts iterates the accounts that have windows,
// so a profile whose whole account reports none (every profile on it signed
// out, the poller not yet reached) would simply not be in the list, and a
// selector that silently omits a profile is worse than one that shows it with
// nothing to say. The info gauge carries one sample per profile regardless of
// whether anything else about it is known.
//
// The percentage attached to each row is its ACCOUNT's, because that is whose
// allowance it is. Both account and domain travel with it so a client can name
// the account beside the number, which is what stops two profiles sharing one
// allowance from reading as two.
func buildListProfiles(ctx context.Context, o Options, now int64) (*claudeobsv1.ListProfilesResponse, error) {
	_ = now
	resp := &claudeobsv1.ListProfilesResponse{
		Identity:       identity(),
		PrimaryProfile: o.PrimaryProfile,
	}
	if o.VM == nil {
		return nil, errNoMetricsStore
	}
	q := o.VM
	var errs []*claudeobsv1.SectionError

	info, err := q.Q(ctx, accountInfoQuery)
	if err != nil {
		return nil, err
	}

	// The account's headline percentage, keyed by account. A query failure
	// here costs the percentages and nothing else: the roster itself is still
	// the answer to "which profiles exist".
	pctByAccount := map[string]float64{}
	if windows, werr := plan.LoadAccountWindows(ctx, q); werr != nil {
		errs = append(errs, &claudeobsv1.SectionError{Section: "windows", Error: werr.Error()})
	} else {
		for _, aw := range windows {
			if p := plan.MaxPct(aw.Windows); p >= 0 {
				pctByAccount[aw.Account] = p
			}
		}
	}

	// Same rule, same reason: a missing availability flag leaves the row
	// saying nothing rather than asserting either answer. The poller emits
	// nothing at all when it could not read a probe response.
	availByProfile := map[string]bool{}
	if res, aerr := q.Q(ctx, usageAvailableQuery); aerr != nil {
		errs = append(errs, &claudeobsv1.SectionError{Section: "usage-available", Error: aerr.Error()})
	} else {
		for _, s := range res {
			if p := s.Metric["profile"]; p != "" {
				if v, ok := vm.SeriesValue(s); ok {
					availByProfile[p] = v >= 1
				}
			}
		}
	}

	seen := map[string]bool{}
	for _, s := range info {
		name := s.Metric["profile"]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		acct := s.Metric["account"]
		row := &claudeobsv1.ProfileRow{
			Profile:   name,
			Account:   acct,
			Domain:    s.Metric["domain"],
			IsPrimary: name == o.PrimaryProfile,
		}
		if pct, ok := pctByAccount[acct]; ok {
			row.MaxPct, row.HasMaxPct = pct, true
		}
		if avail, ok := availByProfile[name]; ok {
			row.UsageAvailable, row.UsageAvailableKnown = avail, true
		}
		resp.Profiles = append(resp.Profiles, row)
	}

	sortProfileRows(resp.Profiles)
	resp.Errors = errs
	return resp, nil
}

// sortProfileRows orders the picker: highest account percentage first, so the
// account closest to throttling is the first thing the reader sees, then the
// rows with no percentage at all, each group by name for a stable list across
// polls.
//
// Ordering by percentage is the whole point rather than a nicety. With the
// list ordered by anything else, an account at 95% sits wherever its name
// happens to fall while the page opens on one at 23%.
func sortProfileRows(rows []*claudeobsv1.ProfileRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.HasMaxPct != b.HasMaxPct {
			return a.HasMaxPct
		}
		if a.HasMaxPct && a.MaxPct != b.MaxPct {
			return a.MaxPct > b.MaxPct
		}
		return a.Profile < b.Profile
	})
}

// accountForProfile resolves which account a profile is signed in to, which is
// what every allowance figure has to be grouped by once a profile is selected.
// Returns "" when nothing says, and a caller must then show no allowance
// rather than guess one.
func accountForProfile(rows []*claudeobsv1.ProfileRow, profile string) string {
	for _, r := range rows {
		if r.Profile == profile {
			return r.Account
		}
	}
	return ""
}
