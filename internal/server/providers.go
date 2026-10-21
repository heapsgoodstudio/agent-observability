package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

func buildConfiguredProfiles(ctx context.Context, o Options, now int64) (*pb.ListProfilesResponse, error) {
	if o.Profiles == nil {
		return buildListProfiles(ctx, o, now)
	}
	ps, err := o.Profiles.List()
	if err != nil {
		return nil, err
	}
	out := &pb.ListProfilesResponse{Identity: identity(), PrimaryProfile: o.PrimaryProfile}
	observed, err := buildListProfiles(ctx, o, now)
	byID := map[string]*pb.ProfileRow{}
	if err == nil {
		out.Errors = observed.Errors
		for _, p := range observed.Profiles {
			byID[p.Profile] = p
		}
	} else if len(ps) > 0 {
		out.Errors = append(out.Errors, &pb.SectionError{Section: "usage", Error: err.Error()})
	}
	for _, p := range ps {
		row := byID[p.ID]
		if row == nil {
			row = &pb.ProfileRow{Profile: p.ID}
		}
		row.Provider = string(p.Provider)
		row.DisplayName = p.Name
		row.Enabled = p.Enabled
		row.IsPrimary = p.Primary
		row.Status = p.WiringStatus
		if !p.Enabled {
			row.Status = "disabled"
		}
		if p.Provider == profiles.Codex && p.Enabled {
			if snap, err := monitor.ReadSnapshot(o.DB, p.ID); err == nil {
				snap = currentCodexSnapshot(p, snap)
				row.Account = snap.Usage.AccountID
				if snap.UsageError == "" && !snap.Usage.FetchedAt.IsZero() && now-snap.Usage.FetchedAt.Unix() <= int64(monitor.QuotaFreshFor/time.Second) {
					row.UsageAvailableKnown = true
					row.UsageAvailable = len(snap.Usage.Windows) > 0 || len(snap.Usage.Buckets) > 0 || snap.Usage.OrdinaryUsageAllowed != nil
					for _, w := range snap.Usage.Windows {
						if !row.HasMaxPct || w.UsedPercent > row.MaxPct {
							row.MaxPct = w.UsedPercent
							row.HasMaxPct = true
						}
					}
				}
			}
		}
		out.Profiles = append(out.Profiles, row)
	}
	sortProfileRows(out.Profiles)
	return out, nil
}

// indexedProvider reports whether a provider's sessions come only from the
// sqlite index. Claude sessions also have a live VictoriaLogs overlay.
func indexedProvider(p string) bool { return p == "codex" || profiles.Detected(profiles.Provider(p)) }

// snapshotKey names the meta row holding a provider's last collection.
func snapshotKey(provider, profileID string) string {
	if profiles.Detected(profiles.Provider(provider)) {
		return monitor.SpendSnapshotKey(profiles.Provider(provider), profileID)
	}
	return "codex.snapshot." + profileID
}

func buildProviderSessions(ctx context.Context, o Options, profile, provider string, now int64) (*pb.ListSessionsResponse, error) {
	// onlyIndexed skips the Claude overlay: a Codex or omp filter, or a
	// selected profile of one of those providers.
	onlyIndexed := indexedProvider(provider)
	if profile != "" && o.Profiles != nil {
		if ps, err := o.Profiles.List(); err == nil {
			for _, p := range ps {
				if p.ID == profile && indexedProvider(string(p.Provider)) {
					onlyIndexed = true
				}
			}
		}
	}
	out := &pb.ListSessionsResponse{Identity: identity()}
	if !onlyIndexed {
		var err error
		out, err = buildListSessions(ctx, o, profile, now)
		if err != nil {
			return nil, err
		}
	}
	if provider != "claude" && o.DB != nil {
		rows, err := db.ListSessions(o.DB, profile)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if indexedProvider(r.Provider) && (provider == "" || provider == r.Provider) {
				out.Sessions = append(out.Sessions, dbRowToProto(r))
			}
		}
		if onlyIndexed {
			// Keyed by profile, valued by provider, so each profile's freshness
			// is read from its own provider's snapshot.
			ids := map[string]string{}
			for _, r := range rows {
				if indexedProvider(r.Provider) && (provider == "" || provider == r.Provider) {
					ids[r.Profile] = r.Provider
				}
			}
			if o.Profiles != nil {
				if ps, err := o.Profiles.List(); err == nil {
					for _, p := range ps {
						if p.ID == profile {
							ids[p.ID] = string(p.Provider)
						}
						if indexedProvider(string(p.Provider)) && p.Enabled && (profile == "" || profile == p.ID) && (provider == "" || provider == string(p.Provider)) {
							ids[p.ID] = string(p.Provider)
						}
					}
				}
			}
			oldest := int64(0)
			out.IndexStale = len(ids) == 0
			for id, prov := range ids {
				var snap struct {
					CollectedAt int64  `json:"collected_at"`
					Error       string `json:"error"`
				}
				if monitor.ReadJSON(o.DB, snapshotKey(prov, id), &snap) != nil || snap.CollectedAt == 0 {
					out.IndexStale = true
					continue
				}
				if oldest == 0 || snap.CollectedAt < oldest {
					oldest = snap.CollectedAt
				}
				if snap.Error != "" || now-snap.CollectedAt > indexStaleS {
					out.IndexStale = true
				}
			}
			if oldest > 0 {
				out.GeneratedAt = time.Unix(oldest, 0).UTC().Format(time.RFC3339)
			}
		}
	}
	sort.SliceStable(out.Sessions, func(i, j int) bool { return out.Sessions[i].CostUsd > out.Sessions[j].CostUsd })
	return out, nil
}

func buildProviderSession(ctx context.Context, o Options, id string, now int64) (*pb.GetSessionResponse, error) {
	if o.DB != nil {
		r, ok, err := db.GetSession(o.DB, id)
		if err != nil {
			return nil, err
		}
		if ok && indexedProvider(r.Provider) {
			tools := map[string]int64{}
			for name, n := range r.Tools {
				tools[name] = int64(n)
			}
			return &pb.GetSessionResponse{Identity: identity(), Row: dbRowToProto(*r), Tools: tools, Trajectory: trajectoryToProto(r.Trajectory)}, nil
		}
	}
	// Do not interpolate arbitrary strings into VictoriaLogs queries.
	if !analysisIDRE.MatchString(id) {
		return nil, errSessionNotFound
	}
	return buildSession(ctx, o, id, now)
}

func buildConfiguredHealth(ctx context.Context, o Options, now int64) *pb.GetHealthResponse {
	out := buildHealth(ctx, o, now)
	if o.Profiles == nil {
		return out
	}
	ps, err := o.Profiles.List()
	if err != nil {
		out.Checks = append(out.Checks, &pb.HealthCheck{Name: "profile settings", State: "bad", Detail: err.Error()})
		out.Healthy = false
		return out
	}
	hasClaude := false
	for _, p := range ps {
		if p.Enabled && p.Provider == profiles.Claude {
			hasClaude = true
		}
	}
	if !hasClaude {
		checks := out.Checks[:0]
		for _, c := range out.Checks {
			if strings.HasPrefix(c.Name, "claude_") || strings.HasPrefix(c.Name, "claude_code") || strings.HasPrefix(c.Name, "plan data") || strings.Contains(strings.ToLower(c.Name), "runner") {
				continue
			}
			checks = append(checks, c)
		}
		out.Checks = checks
	}
	baseChecks := len(out.Checks)
	if len(ps) == 0 {
		out.Checks = append(out.Checks, &pb.HealthCheck{Name: "profiles", State: "unknown", Detail: "No default profiles found. Add a profile in Settings."})
	}
	for _, p := range ps {
		if !p.Enabled {
			continue
		}
		state, detail := "ok", p.WiringStatus
		if p.WiringStatus != "connected" {
			state = "warn"
			detail = p.WiringError
		}
		// A detected-provider profile (Oh My Pi, opencode) is already named by
		// its harness; "(omp)" after it would say the same thing twice.
		name := p.Name
		if !profiles.Detected(p.Provider) {
			name += " (" + string(p.Provider) + ")"
		}
		out.Checks = append(out.Checks, &pb.HealthCheck{Name: name, State: state, Detail: detail})
		if profiles.Detected(p.Provider) {
			var snap monitor.SpendSnapshot
			state, detail = "ok", "session history is current"
			if monitor.ReadJSON(o.DB, monitor.SpendSnapshotKey(p.Provider, p.ID), &snap) != nil || snap.CollectedAt == 0 {
				state, detail = "unknown", "Waiting for first collection"
				if snap.Error != "" {
					state, detail = "bad", snap.Error
				}
			} else if snap.Error != "" {
				state, detail = "bad", snap.Error
			} else if now-snap.CollectedAt > 600 {
				state, detail = "warn", fmt.Sprintf("Last collection %s ago", ageText(float64(now-snap.CollectedAt)))
			}
			out.Checks = append(out.Checks, &pb.HealthCheck{Name: p.Name + " session collection", State: state, Detail: detail})
		}
		if p.Provider == profiles.Codex {
			var snap monitor.Snapshot
			state, detail = "ok", "session history is current"
			if monitor.ReadJSON(o.DB, "codex.snapshot."+p.ID, &snap) != nil {
				state, detail = "unknown", "Waiting for first collection"
			} else if snap.Error != "" {
				state, detail = "bad", snap.Error
			} else if now-snap.CollectedAt > 600 {
				state, detail = "warn", fmt.Sprintf("Last collection %s ago", ageText(float64(now-snap.CollectedAt)))
			}
			out.Checks = append(out.Checks, &pb.HealthCheck{Name: p.Name + " session collection", State: state, Detail: detail})
			state, detail = "ok", "Plan limits refresh every minute"
			quota, err := monitor.ReadSnapshot(o.DB, p.ID)
			quota = currentCodexSnapshot(p, quota)
			if err != nil || quota.Usage.FetchedAt.IsZero() {
				state, detail = "unknown", "Waiting for the first plan snapshot"
			} else if quota.UsageError != "" {
				state, detail = "warn", quota.UsageError
			} else if now-quota.Usage.FetchedAt.Unix() > int64(monitor.QuotaFreshFor/time.Second) {
				state, detail = "warn", "Plan snapshot is stale; remaining allowance is unconfirmed"
			}
			out.Checks = append(out.Checks, &pb.HealthCheck{Name: p.Name + " plan limits", State: state, Detail: detail})
		}
	}
	out.Healthy = true
	for i, c := range out.Checks {
		if (i >= baseChecks && (c.State == "bad" || c.State == "warn")) || (i < baseChecks && ((c.State == "bad" && (c.Name == "VictoriaMetrics" || c.Name == "VictoriaLogs" || c.Name == "Analysis runner")) || (c.State == "warn" && pollerSeriesNames[c.Name]))) {
			out.Healthy = false
		}
	}
	return out
}
