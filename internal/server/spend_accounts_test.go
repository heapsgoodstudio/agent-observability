package server

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/payg"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
)

// An omp profile is not an account. It becomes one account per paid inference
// provider, keyed omp:<provider>, whose billing is the labelled estimate and
// whose activity scope is the omp profile.
func TestOmpProfileBecomesOneAccountPerPaidProvider(t *testing.T) {
	o := settingsFixture(t)
	home := filepath.Join(t.TempDir(), ".omp", "agent")
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := o.Profiles.Save(profiles.Profile{Provider: profiles.Omp, Name: "omp", Directory: home, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	day := now.Format("2006-01-02")
	snap := monitor.SpendSnapshot{CollectedAt: now.Unix(), Directory: home, Usage: []payg.Bucket{
		{Day: day, Provider: "baseten", Model: "glm-5.3", Requests: 3, CostUSD: 6.49, CostKnown: true},
		{Day: day, Provider: "fireworks", Model: "glm-5.3-flash", Requests: 1, CostUSD: 0.0035, CostKnown: true},
		{Day: day, Provider: "fireworks", Model: "unpriced", Requests: 1, CostKnown: false},
		{Day: day, Provider: "lm-studio", Model: "qwen3.8-27b", Requests: 5, CostUSD: 0, CostKnown: true},
	}}
	if err := monitor.WriteJSON(o.DB, monitor.SpendSnapshotKey(profiles.Omp, p.ID), snap); err != nil {
		t.Fatal(err)
	}
	roster, err := buildConfiguredProfiles(context.Background(), o, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	accounts := buildDashboardAccounts(context.Background(), o, roster.Profiles, nil, now.Unix())
	byKey := map[string]bool{}
	for _, a := range accounts {
		byKey[a.Key] = true
		if a.Key == "omp:lm-studio" {
			if a.DisplayName != "LM Studio" || a.Plan != "No cost recorded" || a.Billing.EstimatedOverageUsd != nil || slices.Contains(a.Capabilities, "estimated_spend") || a.GetMonthToDateTokens() != 0 {
				t.Fatalf("lm-studio account = %+v; a free provider shows tokens, never an estimate", a)
			}
		}
		if a.Key == "omp:fireworks" {
			if a.Billing.GetEstimatedOverageUsd() != 0.0035 || a.Billing.EstimatedOverageComplete == nil || a.Billing.GetEstimatedOverageComplete() {
				t.Fatalf("partially priced usage must stay incomplete: %+v", a.Billing)
			}
		}
		if a.Key == "omp:baseten" {
			if a.Provider != "omp" || a.DisplayName != "Baseten" || len(a.Windows) != 0 || a.Billing.GetEstimatedOverageUsd() != 6.49 || a.Billing.ReportedOverageUsd != nil || a.State != "fresh" || a.Billing.EstimatedOverageComplete == nil || !a.Billing.GetEstimatedOverageComplete() {
				t.Fatalf("baseten account = %+v", a)
			}
		}
	}
	if !byKey["omp:baseten"] || !byKey["omp:fireworks"] || !byKey["omp:lm-studio"] || byKey["omp:profile:"+p.ID] {
		t.Fatalf("account keys = %v; want baseten, fireworks and the free lm-studio", byKey)
	}
	scope, err := resolveDashboardScope(context.Background(), o, "", "omp:baseten", "", now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if scope.Provider != "omp" || len(scope.Profiles) != 1 || scope.Profiles[0] != p.ID {
		t.Fatalf("scope = %+v", scope)
	}
	// An account the dashboard does not list must be refused, not widened to
	// every omp profile.
	for _, key := range []string{"omp:nonexistent", "omp:anthropic"} {
		if _, err := resolveDashboardScope(context.Background(), o, "", key, "", now.Unix()); err == nil {
			t.Fatalf("scope %q resolved; want not found", key)
		}
	}
}

// An omp account's activity lists only sessions that used its provider. Every
// omp profile matches every omp account, so the profile scope alone listed a
// Fireworks-only session under Baseten.
func TestOmpAccountActivityListsOnlySessionsThatUsedTheProvider(t *testing.T) {
	o := settingsFixture(t)
	home := filepath.Join(t.TempDir(), ".omp", "agent")
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := o.Profiles.Save(profiles.Profile{Provider: profiles.Omp, Directory: home, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	day := now.Format("2006-01-02")
	if err := monitor.WriteJSON(o.DB, monitor.SpendSnapshotKey(profiles.Omp, p.ID), monitor.SpendSnapshot{CollectedAt: now.Unix(), Directory: home, Usage: []payg.Bucket{
		{Day: day, Provider: "baseten", Model: "glm-5.3", Requests: 2, CostUSD: 1, CostKnown: true},
		{Day: day, Provider: "fireworks", Model: "glm-5.3-flash", Requests: 2, CostUSD: 1, CostKnown: true},
	}}); err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{"both": "baseten/glm-5.3", "baseten-only": "baseten/glm-5.3", "fireworks-only": "fireworks/glm-5.3-flash"}
	for id, model := range rows {
		r := db.SessionRow{ID: "omp:" + p.ID + ":" + id, Provider: "omp", Profile: p.ID, Start: now.Unix() - 60, End: now.Unix(), Models: []transcript.ModelCost{{Model: model}}}
		if id == "both" {
			r.Models = append(r.Models, transcript.ModelCost{Model: "fireworks/glm-5.3-flash"})
		}
		if err := db.UpsertSession(o.DB, r); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(key string) map[string]bool {
		out, err := buildScopedSessions(context.Background(), o, &pb.ListSessionsRequest{Provider: "omp", AccountKey: key}, now.Unix())
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, r := range out.Sessions {
			got[r.Id] = true
		}
		return got
	}
	baseten, fireworks := ids("omp:baseten"), ids("omp:fireworks")
	prefix := "omp:" + p.ID + ":"
	if len(baseten) != 2 || !baseten[prefix+"both"] || !baseten[prefix+"baseten-only"] {
		t.Fatalf("baseten activity = %v", baseten)
	}
	if len(fireworks) != 2 || !fireworks[prefix+"both"] || !fireworks[prefix+"fireworks-only"] {
		t.Fatalf("fireworks activity = %v", fireworks)
	}
}

// The same inference provider used from two harnesses is two accounts, one
// under each harness, and each account's scope, activity and spend are its
// own harness's only.
func TestSameProviderFromTwoHarnessesIsTwoAccounts(t *testing.T) {
	o := settingsFixture(t)
	ompHome := filepath.Join(t.TempDir(), ".omp", "agent")
	ocDir := filepath.Join(t.TempDir(), ".local", "share", "opencode")
	if err := os.MkdirAll(filepath.Join(ompHome, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ocDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocDir, "opencode.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pOmp, err := o.Profiles.Save(profiles.Profile{Provider: profiles.Omp, Directory: ompHome, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	pOc, err := o.Profiles.Save(profiles.Profile{Provider: profiles.Opencode, Directory: ocDir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	day := now.Format("2006-01-02")
	write := func(p profiles.Profile, cost float64) {
		t.Helper()
		if err := monitor.WriteJSON(o.DB, monitor.SpendSnapshotKey(p.Provider, p.ID), monitor.SpendSnapshot{CollectedAt: now.Unix(), Directory: p.Directory, Usage: []payg.Bucket{
			{Day: day, Provider: "baseten", Model: "glm-5.3", Requests: 1, CostUSD: cost, CostKnown: true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	write(pOmp, 6.49)
	write(pOc, 2.10)
	roster, err := buildConfiguredProfiles(context.Background(), o, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	spend := map[string]float64{}
	for _, a := range buildDashboardAccounts(context.Background(), o, roster.Profiles, nil, now.Unix()) {
		spend[a.Key] = a.Billing.GetEstimatedOverageUsd()
		if a.Key == "opencode:baseten" && (a.Provider != "opencode" || a.DisplayName != "Baseten" || len(a.Profiles) != 1 || a.Profiles[0].Profile != pOc.ID || !strings.Contains(a.Billing.Note, "opencode's estimate")) {
			t.Fatalf("opencode account = %+v", a)
		}
	}
	if spend["omp:baseten"] != 6.49 || spend["opencode:baseten"] != 2.10 {
		t.Fatalf("spend = %v; each harness's Baseten must stay its own account", spend)
	}
	for key, want := range map[string]string{"omp:baseten": pOmp.ID, "opencode:baseten": pOc.ID} {
		scope, err := resolveDashboardScope(context.Background(), o, "", key, "", now.Unix())
		if err != nil {
			t.Fatal(err)
		}
		if len(scope.Profiles) != 1 || scope.Profiles[0] != want {
			t.Fatalf("scope %s = %+v; want only %s", key, scope, want)
		}
	}
	if _, err := resolveDashboardScope(context.Background(), o, "", "opencode:fireworks", "", now.Unix()); err == nil {
		t.Fatal("an opencode account the dashboard does not list must be refused")
	}
	oc, err := (&obsService{o: o}).GetPayAsYouGo(context.Background(), connect.NewRequest(&pb.GetPayAsYouGoRequest{Harness: "opencode"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(oc.Msg.Providers) != 1 || oc.Msg.Providers[0].MonthToDateUsd != 2.10 {
		t.Fatalf("opencode pay-as-you-go = %+v", oc.Msg.Providers)
	}
	if _, err := (&obsService{o: o}).GetPayAsYouGo(context.Background(), connect.NewRequest(&pb.GetPayAsYouGoRequest{Harness: "codex"})); err == nil {
		t.Fatal("codex is not a pay-as-you-go harness")
	}
}

// A hidden account is still built and still resolvable; it only carries the
// flag the sidebar and Overview filter on. Claude accounts cannot be hidden.
func TestHiddenDetectedAccountIsFlaggedNotRemoved(t *testing.T) {
	o := settingsFixture(t)
	home := filepath.Join(t.TempDir(), ".omp", "agent")
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := o.Profiles.Save(profiles.Profile{Provider: profiles.Omp, Directory: home, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	day := now.Format("2006-01-02")
	if err := monitor.WriteJSON(o.DB, monitor.SpendSnapshotKey(profiles.Omp, p.ID), monitor.SpendSnapshot{CollectedAt: now.Unix(), Directory: home, Usage: []payg.Bucket{
		{Day: day, Provider: "baseten", Model: "glm-5.3", Requests: 1, CostUSD: 1, CostKnown: true},
		{Day: day, Provider: "wire", Model: "qwen", Requests: 1, Input: 10, CostKnown: true},
	}}); err != nil {
		t.Fatal(err)
	}
	svc := &obsService{o: o}
	if _, err := svc.SetAccountHidden(context.Background(), connect.NewRequest(&pb.SetAccountHiddenRequest{AccountKey: "omp:wire", Hidden: true})); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetAccountHidden(context.Background(), connect.NewRequest(&pb.SetAccountHiddenRequest{AccountKey: "claude:abc", Hidden: true})); err == nil {
		t.Fatal("a Claude account must not be hideable")
	}
	roster, err := buildConfiguredProfiles(context.Background(), o, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	hidden := map[string]bool{}
	for _, a := range buildDashboardAccounts(context.Background(), o, roster.Profiles, nil, now.Unix()) {
		hidden[a.Key] = a.Hidden
	}
	if v, ok := hidden["omp:wire"]; !ok || !v || hidden["omp:baseten"] {
		t.Fatalf("hidden = %v; wire is listed and flagged, baseten is not", hidden)
	}
	if _, err := resolveDashboardScope(context.Background(), o, "", "omp:wire", "", now.Unix()); err != nil {
		t.Fatalf("a hidden account's page must still open: %v", err)
	}
}
