package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

// GetMonitoring keeps activity, current allowances and billing separate. A
// token count or API-equivalent session cost cannot establish paid usage.
func (s *obsService) GetMonitoring(ctx context.Context, req *connect.Request[pb.GetMonitoringRequest]) (*connect.Response[pb.GetMonitoringResponse], error) {
	o := s.o
	if o.DB == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("session database unavailable"))
	}
	out := &pb.GetMonitoringResponse{
		Tokens: &pb.TokenTotals{}, UsageComplete: true, ToolsComplete: true,
		ErrorsComplete: true, DurationComplete: true,
		Billing: &pb.CodexBillingSummary{
			Note: "Codex reported no individual credit limit, so its credit use is unavailable. It does not report billed dollars or which recorded requests used included allowance versus paid credits; included and unclassified usage are not assigned a dollar value.",
		},
	}
	rows, err := db.ListSessions(o.DB, req.Msg.Profile)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	for _, r := range rows {
		if r.Provider != "codex" {
			continue
		}
		out.Sessions++
		out.Requests += float64(r.Requests)
		out.ToolCalls += float64(r.ToolCalls)
		out.Tokens.Input += float64(r.Tokens.Input)
		out.Tokens.Output += float64(r.Tokens.Output)
		out.Tokens.CacheRead += float64(r.Tokens.CacheRead)
		out.Tokens.CacheCreation += float64(r.Tokens.CacheCreation)
		out.ReasoningTokens += float64(r.ReasoningTokens)
		out.UsageComplete = out.UsageComplete && r.UsageKnown
		if !r.UsageKnown {
			out.UsageGaps = append(out.UsageGaps, &pb.CodexUsageGap{SessionId: r.ID, Profile: r.Profile, StartedAt: float64(r.Start), Reason: "Token usage was not recorded or the local records are unavailable."})
		}
		out.ToolsComplete = out.ToolsComplete && r.ToolsKnown
		out.ErrorsComplete = out.ErrorsComplete && r.ErrorsKnown
		out.DurationComplete = out.DurationComplete && r.DurationKnown
		out.ErrorCount += float64(r.ErrorCount)
		out.DurationMs += float64(r.DurationMS)
		out.LastActivityAt = max(out.LastActivityAt, float64(r.End))
		// Current quota state is never back-projected onto historical requests.
		// Even a known API price supplies no evidence of their billing route.
		out.Billing.UnclassifiedRequests += float64(r.Requests)
		out.Billing.UnclassifiedSessions++
	}
	if o.Profiles == nil {
		return connect.NewResponse(out), nil
	}
	ps, err := o.Profiles.List()
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	type accountSnapshot struct {
		profile profiles.Profile
		snap    monitor.Snapshot
		account *pb.CodexAccountUsage
	}
	var picked []accountSnapshot
	seen := map[string]int{}
	now := time.Now()
	haveIndex := false
	for _, p := range ps {
		if p.Provider != profiles.Codex || (req.Msg.Profile != "" && p.ID != req.Msg.Profile) {
			continue
		}
		out.ConfiguredProfiles++
		snap, readErr := monitor.ReadSnapshot(o.DB, p.ID)
		for _, gap := range out.UsageGaps {
			if gap.Profile != p.ID {
				continue
			}
			gap.ProfileName = p.Name
			switch snap.UsageGaps[gap.SessionId] {
			case "interrupted":
				gap.Reason = "Interrupted without recorded token usage; the amount used is unknown."
			case "missing_rollout":
				gap.Reason = "The local session log is unavailable. Restore it to recover token usage."
			case "missing_records":
				gap.Reason = "The session log contains no usable token records."
			}
		}
		if !p.Enabled {
			continue
		}
		snap = currentCodexSnapshot(p, snap)
		if readErr != nil {
			out.Errors = append(out.Errors, &pb.SectionError{Section: p.Name, Error: "Waiting for the first Codex collection"})
		}
		if !haveIndex || float64(snap.CollectedAt) < out.LastIndexedAt {
			out.LastIndexedAt = float64(snap.CollectedAt)
			haveIndex = true
		}
		for _, msg := range append([]string{snap.Error, snap.UsageError, snap.NotificationError}, snap.Warnings...) {
			// Older snapshots contained a second, generic token-coverage banner.
			// The structured gaps above replace it, including before reindexing.
			if msg != "" && msg != "Token detail is unavailable for one or more threads; totals are partial" {
				out.Errors = append(out.Errors, &pb.SectionError{Section: p.Name, Error: msg})
			}
		}
		account := codexAccountProto(p, snap, now)
		key := "profile:" + p.ID
		if account.Account != "" {
			key = "account:" + account.Account
		}
		candidate := accountSnapshot{p, snap, account}
		if i, ok := seen[key]; ok {
			old := picked[i].account
			if account.FetchedAt > old.FetchedAt || (account.FetchedAt == old.FetchedAt && old.Stale && !account.Stale) {
				picked[i] = candidate
			}
		} else {
			seen[key] = len(picked)
			picked = append(picked, candidate)
		}
	}
	for _, pick := range picked {
		out.Accounts = append(out.Accounts, pick.account)
		for _, w := range pick.snap.Usage.Windows {
			out.Windows = append(out.Windows, &pb.CodexUsageWindow{
				Profile: pick.profile.ID, ProfileName: pick.profile.Name,
				Account: pick.account.Account, Name: codexWindowName(w), LimitId: w.LimitID, Kind: w.Kind,
				UsedPercent: w.UsedPercent, ResetsAt: float64(w.ResetsAt), WindowMinutes: float64(w.WindowMinutes),
				FetchedAt: pick.account.FetchedAt, Stale: pick.account.Stale,
			})
		}
	}
	fillCodexCreditSummary(out.Billing, out.Accounts, o.CodexCreditUSD)
	return connect.NewResponse(out), nil
}

// A profile may move before its invalidation is committed. Never attach an
// older home's account headroom to the newly configured directory.
func currentCodexSnapshot(p profiles.Profile, snap monitor.Snapshot) monitor.Snapshot {
	if snap.UsageDirectory != "" && snap.UsageDirectory != p.Directory {
		snap.Usage = codex.Usage{}
		snap.UsageError = "Waiting for a plan snapshot for this profile directory"
	}
	return snap
}

func codexAccountProto(p profiles.Profile, snap monitor.Snapshot, now time.Time) *pb.CodexAccountUsage {
	u := snap.Usage
	fetched := float64(0)
	if !u.FetchedAt.IsZero() {
		fetched = float64(u.FetchedAt.Unix())
	}
	account := &pb.CodexAccountUsage{
		Profile: p.ID, Account: u.AccountID, Name: p.Name, PlanType: u.PlanType,
		FetchedAt: fetched, OrdinaryUsageAllowed: u.OrdinaryUsageAllowed,
		Stale: snap.UsageError != "" || u.FetchedAt.IsZero() || now.Sub(u.FetchedAt) > monitor.QuotaFreshFor || u.FetchedAt.After(now.Add(time.Minute)),
	}
	if u.ResetCreditsAvailable != nil {
		n := float64(*u.ResetCreditsAvailable)
		account.ResetCreditsAvailable = &n
	}
	for _, b := range u.Buckets {
		bucket := &pb.CodexLimitBucket{LimitId: b.LimitID, Name: b.Name, NormalModelSlug: b.NormalModelSlug, SpendControlReached: b.SpendControlReached}
		if b.RateLimitReachedType != nil {
			bucket.RateLimitReachedType = *b.RateLimitReachedType
		}
		if b.Credits != nil {
			bucket.Credits = &pb.CodexCredits{HasCredits: b.Credits.HasCredits, Unlimited: b.Credits.Unlimited, Balance: b.Credits.Balance}
		}
		if b.IndividualLimit != nil {
			v := b.IndividualLimit
			bucket.IndividualLimit = &pb.CodexIndividualLimit{Limit: v.Limit, Used: v.Used, RemainingPercent: float64(v.RemainingPercent), ResetsAt: float64(v.ResetsAt)}
		}
		account.Buckets = append(account.Buckets, bucket)
	}
	return account
}

func codexWindowName(w codex.Window) string {
	period := w.Kind
	switch w.WindowMinutes {
	case 7 * 24 * 60:
		period = "Weekly allowance"
	case 5 * 60:
		period = "5-hour allowance"
	case 24 * 60:
		period = "Daily allowance"
	default:
		if w.WindowMinutes > 0 {
			period = fmt.Sprintf("%d-minute allowance", w.WindowMinutes)
		} else if period == "" {
			period = "Usage allowance"
		}
	}
	return strings.TrimSpace(strings.TrimSpace(w.Name) + " " + period)
}
