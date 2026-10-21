package server

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/analysis"
	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

func TestAccountScopedFindingsNeverWidenToOtherAccounts(t *testing.T) {
	o := settingsFixture(t)
	a := regressionProfile(t, o, profiles.Codex, "Work")
	b := regressionProfile(t, o, profiles.Codex, "Personal")
	for i, p := range []profiles.Profile{a, b} {
		account := []string{"work", "personal"}[i]
		monitor.WriteJSON(o.DB, "codex.snapshot."+p.ID, monitor.Snapshot{Usage: codex.Usage{AccountID: account}})
		if err := db.UpsertSession(o.DB, db.SessionRow{ID: "codex:" + p.ID + ":session", Profile: p.ID, Provider: "codex", Requests: 111 + i, UsageKnown: true, HasTranscript: true}); err != nil {
			t.Fatal(err)
		}
	}
	scope, err := resolveDashboardScope(context.Background(), o, "codex", "codex:work", "", o.now())
	ids := scope.Profiles
	if err != nil || !reflect.DeepEqual(ids, []string{a.ID}) {
		t.Fatalf("wrong account: %v %v", ids, err)
	}
	out, err := buildFindings(context.Background(), o, scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range out.Findings {
		if f.Id == "review-largest-session" && f.Evidence == "" {
			t.Fatal("missing source evidence")
		}
	}
	for _, key := range []string{"codex:missing", "claude:work", "malformed"} {
		if _, err := resolveDashboardScope(context.Background(), o, "codex", key, "", o.now()); err == nil {
			t.Fatalf("invalid account widened scope: %s", key)
		}
	}
}

func TestStartAnalysisSelectsEvaluatorIndependently(t *testing.T) {
	o := settingsFixture(t)
	sid := "omp:source:abc123"
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	os.WriteFile(path, []byte(`{"type":"session","id":"abc123"}`), 0600)
	db.UpsertSession(o.DB, db.SessionRow{ID: sid, Provider: "omp", Profile: "source", NativeID: "abc123", HasTranscript: true})
	db.PutSessionSource(o.DB, db.SessionSource{ID: sid, Path: path, Format: "omp"})
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake")
	os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"schema\":2,\"headline\":\"Done\",\"summary\":\"Evidence reviewed\"}'\n"), 0700)
	var chosen analysis.Selection
	var runner *analysis.Runner
	o.RunnerForTarget = func(s analysis.Selection) (*analysis.Runner, error) {
		chosen = s
		runner = analysis.New(analysis.Config{Provider: "omp", ProfileID: s.Profile, Binary: bin, ProfileDir: dir, SourceProvider: s.SourceProvider, SourceProfile: s.SourceProfile, DB: o.DB, DataDir: dir, AnalysisTimeout: time.Second * 5})
		return runner, nil
	}
	_, err := (&obsService{o: o}).StartAnalysis(context.Background(), connect.NewRequest(&pb.StartAnalysisRequest{Id: sid, Provider: "omp", Profile: "evaluation-account", Model: "vendor/model"}))
	if err != nil {
		t.Fatal(err)
	}
	runner.Wait()
	if chosen.Profile != "evaluation-account" || chosen.SourceProfile != "source" || chosen.SourceProvider != "omp" {
		t.Fatalf("mixed source and evaluator: %+v", chosen)
	}
	row, _, err := runner.AnalysisStatus(sid)
	if err != nil || row.State != "done" {
		t.Fatalf("failed run: %+v %v", row, err)
	}
}

func TestNewProvidersAppearInSessionListAndDetail(t *testing.T) {
	o := settingsFixture(t)
	for _, provider := range []string{"omp", "opencode"} {
		id := provider + ":source:abc"
		if err := db.UpsertSession(o.DB, db.SessionRow{ID: id, Provider: provider, Profile: "source", NativeID: "abc", HasTranscript: true}); err != nil {
			t.Fatal(err)
		}
		list, err := buildProviderSessions(context.Background(), o, "", provider, time.Now().Unix())
		if err != nil || len(list.Sessions) != 1 || list.Sessions[0].Provider != provider {
			t.Fatalf("%s list: %+v %v", provider, list, err)
		}
		detail, err := buildProviderSession(context.Background(), o, id, time.Now().Unix())
		if err != nil || detail.Row.Provider != provider {
			t.Fatalf("%s detail: %+v %v", provider, detail, err)
		}
	}
}
