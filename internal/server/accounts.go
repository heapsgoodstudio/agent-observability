package server

import (
	"context"
	"sort"

	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/plan"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// The per-account billing queries. Each reduces with `by (account)` for the
// same reason the window queries do: reducing across accounts answers "which
// account is highest" rather than "what is this account at", and these are
// separate allowances that must never be collapsed into one.
//
// claude_profile_account_info is a standard *_info gauge, value 1 with the
// facts in its labels. It is what turns an eight character account id into a
// name a person can read, and what says which profiles feed one account.
const (
	accountInfoQuery      = `claude_profile_account_info`
	accountExtraUsdQuery  = `max by (account)(claude_extra_usage_usd)`
	accountExtraOnQuery   = `max by (account)(claude_extra_usage_enabled)`
	accountInOverageQuery = `max by (account)(claude_in_overage)`
	accountAgeQuery       = `min by (account)(claude_plan_data_age_seconds)`
)

// accountFacts is one account's non-window state, keyed by account id.
type accountFacts struct {
	profiles     []string
	domain       string
	extraUSD     float64
	extraKnown   bool
	overageOn    bool
	overageKnown bool
	inOverage    bool
	ageS         float64
	ageKnown     bool
}

// loadAccounts assembles the per-account view GetOverview returns.
//
// An unreachable store is an error, and the section machinery records it and
// carries on, so it costs this panel and nothing else. An install whose poller
// predates the account labels is a different case entirely: the queries
// succeed and match nothing, which returns an empty list and no error,
// because "no accounts labelled yet" is an answer rather than a failure.
func loadAccounts(ctx context.Context, q *vm.Client, primaryProfile string) ([]*claudeobsv1.AccountPlan, string, error) {
	windows, err := plan.LoadAccountWindows(ctx, q)
	if err != nil {
		return nil, "", err
	}
	facts := map[string]*accountFacts{}
	get := func(id string) *accountFacts {
		if facts[id] == nil {
			facts[id] = &accountFacts{}
		}
		return facts[id]
	}

	if res, err := q.Q(ctx, accountInfoQuery); err == nil {
		for _, s := range res {
			if s.Metric["account"] == "" {
				continue
			}
			f := get(s.Metric["account"])
			f.domain = s.Metric["domain"]
			if p := s.Metric["profile"]; p != "" {
				f.profiles = append(f.profiles, p)
			}
		}
	}
	// Same rule as LoadAccountWindows: a sample with no account label belongs
	// to no account. Without this an upgrade's leftover unlabelled billed
	// figure would attach itself to an empty-string key and be reported
	// alongside the account it actually came from.
	scalarInto := func(query string, set func(*accountFacts, float64)) {
		res, err := q.Q(ctx, query)
		if err != nil {
			return
		}
		for _, s := range res {
			acct := s.Metric["account"]
			if acct == "" {
				continue
			}
			if v, ok := vm.SeriesValue(s); ok {
				set(get(acct), v)
			}
		}
	}
	scalarInto(accountExtraUsdQuery, func(f *accountFacts, v float64) { f.extraUSD, f.extraKnown = v, true })
	scalarInto(accountExtraOnQuery, func(f *accountFacts, v float64) { f.overageOn, f.overageKnown = v >= 1, true })
	scalarInto(accountInOverageQuery, func(f *accountFacts, v float64) { f.inOverage = v >= 1 })
	scalarInto(accountAgeQuery, func(f *accountFacts, v float64) { f.ageS, f.ageKnown = v, true })

	primary := ""
	out := make([]*claudeobsv1.AccountPlan, 0, len(windows))
	for _, aw := range windows {
		f := get(aw.Account)
		sort.Strings(f.profiles)
		maxPct := plan.MaxPct(aw.Windows)
		out = append(out, &claudeobsv1.AccountPlan{
			Account:         aw.Account,
			DisplayName:     displayName(f.profiles, f.domain, aw.Account),
			Domain:          f.domain,
			Profiles:        f.profiles,
			Windows:         windowsToProto(aw.Windows),
			MaxPct:          maxPct,
			HasMaxPct:       maxPct >= 0,
			OverageEnabled:  f.overageOn,
			OverageKnown:    f.overageKnown,
			ExtraUsageUsd:   f.extraUSD,
			ExtraUsageKnown: f.extraKnown,
			InOverage:       f.inOverage,
			PlanDataAgeS:    f.ageS,
			AgeKnown:        f.ageKnown,
			// How long since a live fetch confirmed these figures, as
			// against how long since they changed. See AccountWindows.
			PlanDataConfirmedAgeS: aw.ConfirmedAgeS,
			ConfirmedAgeKnown:     aw.HasConfirmedAge,
		})
		for _, p := range f.profiles {
			if p == primaryProfile {
				primary = aw.Account
			}
		}
	}
	// Falling back to the first account keeps a picker from opening on
	// nothing when PROFILE_DIR names a profile the poller has not reached.
	if primary == "" && len(out) > 0 {
		primary = out[0].Account
	}
	return out, primary, nil
}

// displayName prefers a profile name over a domain, because "acme" is what the
// operator calls it and "work.example" is an implementation detail of the account.
// With several profiles on one account the domain is the only name that
// describes all of them, so that wins instead. The raw id is the last resort,
// for an account whose info series has not landed yet.
func displayName(profiles []string, domain, account string) string {
	switch {
	case len(profiles) == 1:
		return profiles[0]
	case domain != "":
		return domain
	case len(profiles) > 1:
		return profiles[0]
	default:
		return account
	}
}
