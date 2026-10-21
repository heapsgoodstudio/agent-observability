package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/analysis"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
)

func TestScopedFindingsConstrainEveryStoreAndCache(t *testing.T) {
	d := openTestDB(t)
	seedFindingsIndex(t, d,
		db.SessionRow{ID: "a", Provider: "claude", Profile: "a", Models: []transcript.ModelCost{{Model: "claude-haiku-a"}}},
		db.SessionRow{ID: "b", Provider: "claude", Profile: "b", Models: []transcript.ModelCost{{Model: "claude-opus-b"}}},
		db.SessionRow{ID: "cx", Provider: "codex", Profile: "a", Models: []transcript.ModelCost{{Model: "codex-foreign"}}},
		db.SessionRow{ID: "blank", Provider: "claude", Models: []transcript.ModelCost{{Model: "claude-unattributed"}}},
	)
	var mu sync.Mutex
	var vmQueries, vlQueries []string
	vmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		vmQueries = append(vmQueries, r.FormValue("query"))
		mu.Unlock()
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer vmSrv.Close()
	vlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		vlQueries = append(vlQueries, r.FormValue("query"))
		mu.Unlock()
	}))
	defer vlSrv.Close()
	o := findingsOptions(vmSrv, vlSrv, d)
	for _, id := range []string{"a", "b"} {
		resp, err := buildFindings(context.Background(), o, dashboardScope{Provider: "claude", AccountKey: "claude:" + id, Profiles: []string{id}})
		if err != nil {
			t.Fatal(err)
		}
		model := findByID(resp.Findings, "model-mix")
		if model == nil {
			t.Fatal("model finding missing")
		}
		raw, _ := json.Marshal(model)
		if !strings.Contains(string(raw), "-"+id) || strings.Contains(string(raw), "foreign") || strings.Contains(string(raw), "unattributed") {
			t.Fatalf("wrong evidence for %s: %s", id, raw)
		}
		if resp.Cost_7D != 0 || resp.Overage_7D != 0 || resp.OverageKnown {
			t.Fatal("unclassified money exposed")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(vmQueries) != 6 || len(vlQueries) != 20 {
		t.Fatalf("scope cache collided: vm=%d vl=%d", len(vmQueries), len(vlQueries))
	}
	for i, q := range vmQueries {
		id := "a"
		if i >= 3 {
			id = "b"
		}
		if !strings.Contains(q, `provider=~"claude|"`) || !strings.Contains(q, `profile=~"`+id+`"`) {
			t.Fatalf("unscoped VM query %s", q)
		}
		if strings.Contains(q, "cost") || strings.Contains(q, "overage") {
			t.Fatalf("compatibility monetary query %s", q)
		}
	}
	for i, q := range vlQueries {
		id := "a"
		if i >= 10 {
			id = "b"
		}
		if !strings.Contains(q, `(provider:="claude" OR provider:="")`) || !strings.Contains(q, `profile:="`+id+`"`) {
			t.Fatalf("unscoped VL query %s", q)
		}
	}
}

func TestCodexFindingsNeverReadClaudeEvidence(t *testing.T) {
	d := openTestDB(t)
	seedFindingsIndex(t, d, db.SessionRow{ID: "claude-only", Profile: "claude", Provider: "claude", Requests: 900}, db.SessionRow{ID: "codex-only", Profile: "codex", Provider: "codex", Requests: 10, UsageKnown: true})
	s := &obsService{o: Options{DB: d}}
	response, err := s.GetFindings(context.Background(), connect.NewRequest(&pb.GetFindingsRequest{Provider: "codex"}))
	if err != nil {
		t.Fatal(err)
	}
	if !response.Msg.IndexOk || len(response.Msg.Findings) == 0 {
		t.Fatalf("indexed checks unavailable: %+v", response.Msg)
	}
	for _, finding := range response.Msg.Findings {
		if strings.Contains(finding.Evidence, "claude-only") || strings.Contains(finding.Evidence, "900") {
			t.Fatal("Claude evidence leaked")
		}
	}
}

func TestScopedAdviceRequiresEligibleExecutionProfileAndIsolatesConfig(t *testing.T) {
	o := settingsFixture(t)
	a := regressionProfile(t, o, profiles.Claude, "Evidence")
	b := regressionProfile(t, o, profiles.Claude, "Execution")
	cx := regressionProfile(t, o, profiles.Codex, "Codex")
	data := t.TempDir()
	binary := filepath.Join(t.TempDir(), "fake.sh")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho '{\"schema\":2,\"verdict\":\"mixed\",\"headline\":\"Scoped\",\"summary\":\"Scoped\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runner := analysis.New(analysis.Config{ClaudeBin: binary, ProfileDir: b.Directory, DataDir: data, DB: o.DB, AnalysisTimeout: time.Second})
	o.Runner = runner
	o.DataDir = data
	o.Memo = store.New(store.TTL)
	chosen := ""
	o.RunnerForProfile = func(id string) *analysis.Runner { chosen = id; return runner }
	s := &obsService{o: o}
	req := &pb.StartAdviceRequest{Provider: "claude", Profile: a.ID, Model: "haiku"}
	for _, invalid := range []string{"", cx.ID, "does-not-exist"} {
		req.ExecutionProfile = invalid
		if _, err := s.StartAdvice(context.Background(), connect.NewRequest(req)); err == nil {
			t.Fatalf("execution %q accepted", invalid)
		}
	}
	if chosen != "" {
		t.Fatal("invalid execution reached runner")
	}
	scope, err := resolveDashboardScope(context.Background(), o, "claude", "", a.ID, o.now())
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := scopedAdviceEvidence(o, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Configurations) != 1 || evidence.Configurations[0].ID != a.ID {
		t.Fatalf("config scope widened: %+v", evidence)
	}
	req.ExecutionProfile = b.ID
	if _, err := s.StartAdvice(context.Background(), connect.NewRequest(req)); err != nil {
		t.Fatal(err)
	}
	runner.Wait()
	if chosen != b.ID {
		t.Fatalf("executed with %q", chosen)
	}
	saved, err := s.GetAdvice(context.Background(), connect.NewRequest(&pb.GetAdviceRequest{Provider: "claude", Profile: a.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if saved.Msg.ExecutionProfile != b.ID || saved.Msg.Profile != a.ID || saved.Msg.Run.State != "done" {
		t.Fatalf("wrong metadata %+v", saved.Msg)
	}
	if _, err := s.GetAdvice(context.Background(), connect.NewRequest(&pb.GetAdviceRequest{Provider: "claude", Profile: b.ID})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("other profile saw report: %v", err)
	}
	if _, err := s.GetAdvice(context.Background(), connect.NewRequest(&pb.GetAdviceRequest{})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("aggregate saw scoped report: %v", err)
	}
}
