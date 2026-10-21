package server

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

type dashboardScope struct {
	Provider, AccountKey, Profile, ScopeNote string
	Profiles                                 []string
	All                                      bool
}

func resolveDashboardScope(ctx context.Context, o Options, provider, accountKey, profile string, now int64) (dashboardScope, error) {
	s := dashboardScope{Provider: provider, AccountKey: accountKey, Profile: profile, All: provider == "" && accountKey == "" && profile == ""}
	if err := checkProvider(provider); err != nil {
		return s, err
	}
	if s.All {
		return s, nil
	}
	if accountKey != "" {
		prefix, _, ok := strings.Cut(accountKey, ":")
		if !ok || (prefix != "claude" && prefix != "codex" && !profiles.Detected(profiles.Provider(prefix))) || (provider != "" && prefix != provider) {
			return s, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("account and provider do not match"))
		}
		s.Provider = prefix
	}
	// Provider-only scope includes historical disconnected profiles. It never
	// needs a current identity lookup to filter provider-attributed evidence.
	if accountKey == "" && profile == "" {
		return s, nil
	}
	roster, err := dashboardProfileRoster(ctx, o, now)
	if err != nil {
		return s, connect.NewError(connect.CodeUnavailable, err)
	}
	for _, p := range roster.Profiles {
		pp := p.Provider
		if pp == "" {
			pp = "claude"
		}
		if s.Provider != "" && s.Provider != pp {
			continue
		}
		if profile != "" && p.Profile != profile {
			continue
		}
		if accountKey != "" && !profileMatchesAccount(pp, p, accountKey) {
			continue
		}
		s.Profiles = append(s.Profiles, p.Profile)
		if s.Provider == "" {
			s.Provider = pp
		}
	}
	if len(s.Profiles) == 0 {
		return s, connect.NewError(connect.CodeNotFound, fmt.Errorf("no configured profile matches the requested scope"))
	}
	// Every profile of a detected-provider harness matches every key of that
	// harness, so the profiles alone cannot reject an unknown one. The key must
	// name an account the dashboard would list, or the lookup would widen to
	// all of the harness's activity.
	if profiles.Detected(profiles.Provider(s.Provider)) && accountKey != "" && !spendAccountExists(o, roster.Profiles, accountKey, now) {
		return s, connect.NewError(connect.CodeNotFound, fmt.Errorf("no %s account matches the requested scope", profiles.ProviderName(profiles.Provider(s.Provider))))
	}
	sort.Strings(s.Profiles)
	if accountKey != "" {
		s.ScopeNote = "Activity and reports cover the currently linked profiles. Historical account ownership was not recorded for older sessions; this is profile history, not verified account history."
	}
	return s, nil
}

func buildScopedSessions(ctx context.Context, o Options, req *pb.ListSessionsRequest, now int64) (*pb.ListSessionsResponse, error) {
	scope, err := resolveDashboardScope(ctx, o, req.Provider, req.AccountKey, req.Profile, now)
	if err != nil {
		return nil, err
	}
	if req.AccountKey == "" {
		out, err := buildProviderSessions(ctx, o, req.Profile, scope.Provider, now)
		if out != nil {
			out.Provider = scope.Provider
			out.Profile = req.Profile
			out.ScopeNote = scope.ScopeNote
		}
		return out, err
	}
	out := &pb.ListSessionsResponse{Identity: identity(), Provider: scope.Provider, AccountKey: scope.AccountKey, Profile: req.Profile, ScopeNote: scope.ScopeNote}
	seen := map[string]bool{}
	for _, p := range scope.Profiles {
		one, err := buildProviderSessions(ctx, o, p, scope.Provider, now)
		if err != nil {
			return nil, err
		}
		out.Errors = append(out.Errors, one.Errors...)
		out.IndexStale = out.IndexStale || one.IndexStale
		out.OverageObserved = out.OverageObserved || one.OverageObserved
		if out.GeneratedAt == "" || one.GeneratedAt < out.GeneratedAt {
			out.GeneratedAt = one.GeneratedAt
		}
		for _, r := range one.Sessions {
			if !seen[r.Id] {
				seen[r.Id] = true
				out.Sessions = append(out.Sessions, r)
			}
		}
	}
	if harness, inference, ok := strings.Cut(req.AccountKey, ":"); ok && profiles.Detected(profiles.Provider(harness)) {
		out.Sessions = sessionsUsingProvider(out.Sessions, inference)
		out.ScopeNote = "Sessions where " + profiles.ProviderName(profiles.Provider(harness)) + " sent requests to " + inferenceName(inference) + ". A session that also used another provider is listed here and under that provider, with totals for the whole session."
	}
	sort.SliceStable(out.Sessions, func(i, j int) bool { return out.Sessions[i].End > out.Sessions[j].End })
	return out, nil
}

// sessionsUsingProvider keeps the sessions that sent at least one request to
// an inference provider. Every profile of a detected-provider harness matches
// every account of it, so without this an account's activity listed sessions
// that never used it. internal/omp and internal/opencode both name each model
// "<provider>/<model>", which is what this reads.
func sessionsUsingProvider(rows []*pb.SessionRow, inference string) []*pb.SessionRow {
	keep := rows[:0]
	for _, r := range rows {
		for _, m := range r.Models {
			if strings.HasPrefix(m.Model, inference+"/") {
				keep = append(keep, r)
				break
			}
		}
	}
	return keep
}

// A profile switching accounts leaves the previous labelled VM series in its
// lookbehind. Resolve the newest actual sample, and keep ties unresolved.
func dashboardProfileRoster(ctx context.Context, o Options, now int64) (*pb.ListProfilesResponse, error) {
	roster, err := buildConfiguredProfiles(ctx, o, now)
	if err != nil || o.VM == nil {
		return roster, err
	}
	series, err := o.VM.Q(ctx, "timestamp(claude_profile_account_info)")
	if err != nil {
		roster.Errors = append(roster.Errors, &pb.SectionError{Section: "account identity", Error: err.Error()})
		return roster, nil
	}
	type binding struct {
		account, domain string
		ts              float64
		ambiguous       bool
	}
	latest := map[string]binding{}
	for _, s := range series {
		profile := s.Metric["profile"]
		if profile == "" {
			continue
		}
		ts, ok := vm.SeriesValue(s)
		if !ok || !finite(ts) {
			continue
		}
		old, exists := latest[profile]
		if !exists || ts > old.ts {
			latest[profile] = binding{account: s.Metric["account"], domain: s.Metric["domain"], ts: ts}
		} else if ts == old.ts && old.account != s.Metric["account"] {
			old.ambiguous = true
			latest[profile] = old
		}
	}
	for _, p := range roster.Profiles {
		if p.Provider != "" && p.Provider != "claude" {
			continue
		}
		if b, ok := latest[p.Profile]; ok {
			p.Account = b.account
			p.Domain = b.domain
			if b.ambiguous {
				p.Account = ""
				p.Domain = ""
				p.HasMaxPct = false
			}
		}
	}
	return roster, nil
}
