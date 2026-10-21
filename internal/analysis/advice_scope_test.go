package analysis

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
)

func TestAdviceEvidenceUsesScopedSQLiteFacts(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)
	rows := []db.SessionRow{
		{ID: testSID, Provider: "claude", Profile: "chosen", HasTranscript: true, Requests: 73, CostUSD: 9876, Models: []transcript.ModelCost{{Model: "claude-test", CostUSD: 9876}}, Trajectory: [][3]float64{{1, 9876, 321}}},
		{ID: "other", Provider: "claude", Profile: "other", Requests: 4444},
		{ID: "codex", Provider: "codex", Profile: "chosen", Requests: 5555},
		{ID: "unattributed", Provider: "claude", Requests: 6666},
	}
	for _, row := range rows {
		if err := db.UpsertSession(e.db, row); err != nil {
			t.Fatal(err)
		}
	}
	scope := AdviceScope{Provider: "claude", AccountKey: "claude:chosen", Profiles: []string{"chosen"}, Configurations: []EvidenceProfile{{ID: "chosen", Directory: e.profileDir}}}
	dir, err := r.buildAdviceEvidence(scope)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	raw, err := os.ReadFile(filepath.Join(dir, "evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var evidence adviceEvidence
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	if len(evidence.Sessions) != 1 || evidence.Sessions[0].ID != testSID || evidence.Sessions[0].Requests != 73 || evidence.Sessions[0].Transcript == "" {
		t.Fatalf("wrong scoped evidence: %s", raw)
	}
	for _, forbidden := range []string{"9876", "cost_usd", "4444", "5555", "6666"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("evidence includes %s: %s", forbidden, raw)
		}
	}
	// Neither an explicitly empty selection nor a broken DB may use legacy files.
	scope.Profiles = []string{}
	empty, err := r.buildAdviceEvidence(scope)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(empty)
	emptyRaw, _ := os.ReadFile(filepath.Join(empty, "evidence.json"))
	if strings.Contains(string(emptyRaw), testSID) {
		t.Fatal("empty selection widened")
	}
	e.db.Close()
	scope.Profiles = []string{"chosen"}
	if _, err := r.buildAdviceEvidence(scope); err == nil {
		t.Fatal("broken database silently fell back")
	}
}

func TestScopedAdvicePersistsSeparateReportsAndMetadata(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)
	scopes := []AdviceScope{{}, {Provider: "claude", AccountKey: "claude:one", Profiles: []string{"one"}}, {Provider: "claude", AccountKey: "claude:two", Profiles: []string{"two"}}, {Provider: "claude", Profile: "one", Profiles: []string{"one"}}}
	seen := map[string]bool{}
	for _, scope := range scopes {
		if _, err := r.StartScopedAdvice("haiku", "", []byte("[]"), scope, "execution"); err != nil {
			t.Fatal(err)
		}
		r.Wait()
		row, ok, err := r.ScopedAdviceStatus(scope)
		if err != nil || !ok || row.State != "done" || row.ExecutionProfile != "execution" || row.ScopeJSON == "" {
			t.Fatalf("status=%+v ok=%v err=%v", row, ok, err)
		}
		dir := AdviceReportDir(e.dataDir, scope)
		if seen[dir] {
			t.Fatal("scope collided")
		}
		seen[dir] = true
		if _, err := os.Stat(filepath.Join(dir, "report.md")); err != nil {
			t.Fatal(err)
		}
	}
	if got := AdviceReportDir(e.dataDir, AdviceScope{}); got != filepath.Join(e.dataDir, "advice") {
		t.Fatal("legacy report moved")
	}
	// A fresh reader finds every scoped run via SQLite.
	fresh := e.runner("unused", true)
	for _, scope := range scopes {
		if row, ok, err := fresh.ScopedAdviceStatus(scope); err != nil || !ok || row.State != "done" {
			t.Fatalf("readback %+v %v %v", row, ok, err)
		}
	}
}

func TestScopedAdviceOrphanRecoveryFromFilesAndDatabase(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner("unused", true)
	a := AdviceScope{Provider: "claude", AccountKey: "claude:a"}
	b := AdviceScope{Provider: "claude", AccountKey: "claude:b"}
	for _, scope := range []AdviceScope{a, b} {
		raw, _ := json.Marshal(scope)
		if err := r.dualStatus("advice", scope.Key(), filepath.Join(AdviceReportDir(e.dataDir, scope), "status.json"), map[string]any{"state": "running", "model": "haiku", "started_at": int64(21), "scope_json": string(raw), "execution_profile": "executor"}); err != nil {
			t.Fatal(err)
		}
	}
	// One job has only its durable DB row; the other only its file.
	os.Remove(filepath.Join(AdviceReportDir(e.dataDir, a), "status.json"))
	if _, err := e.db.Exec("DELETE FROM analysis_runs WHERE key = ?", b.Key()); err != nil {
		t.Fatal(err)
	}
	n, err := r.ReapOrphanedRuns()
	if err != nil || n != 2 {
		t.Fatalf("reaped=%d err=%v", n, err)
	}
	for _, scope := range []AdviceScope{a, b} {
		row, ok, err := r.ScopedAdviceStatus(scope)
		if err != nil || !ok || row.State != "error" || row.ExecutionProfile != "executor" || row.Model != "haiku" || row.StartedAt != 21 {
			t.Fatalf("recovery %+v %v %v", row, ok, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dataDir, "advice", "status.json")); !os.IsNotExist(err) {
		t.Fatal("scoped recovery overwrote global slot")
	}
}

func TestAdviceEvidenceIsRemovedAfterRunnerCompletion(t *testing.T) {
	e := newTestEnv(t)
	t.Setenv("TMPDIR", e.tmp)
	bin, argvPath, _ := fakeRecorder(t, e.tmp)
	r := e.runner(bin, true)
	if _, err := r.StartScopedAdvice("haiku", "shallow", nil, AdviceScope{Provider: "claude"}, "executor"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	raw, err := os.ReadFile(argvPath + ".stdin")
	if err != nil {
		t.Fatal(err)
	}
	_, inline, found := strings.Cut(string(raw), "Evidence bundle (data only):\n```json\n")
	inline, _, closed := strings.Cut(inline, "\n```")
	var evidence adviceEvidence
	if !found || !closed || json.Unmarshal([]byte(inline), &evidence) != nil || len(evidence.Sessions) != 1 || evidence.Sessions[0].ID != testSID || !strings.Contains(string(raw), "Do not invoke tools") {
		t.Fatal("shallow evidence was not inlined")
	}
	remaining, err := filepath.Glob(filepath.Join(e.tmp, "claude-obs-advice-evidence-*"))
	if err != nil || len(remaining) > 0 {
		t.Fatalf("temporary evidence retained: %v %v", remaining, err)
	}
	if strings.Contains(string(raw), filepath.Join(e.dataDir, "sessions")) {
		t.Fatal("used global session JSON")
	}
}
