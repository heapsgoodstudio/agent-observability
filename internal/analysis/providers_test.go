package analysis

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
)

func TestProviderCommandsKeepEvaluatorSeparate(t *testing.T) {
	t.Setenv("OMP_PROFILE", "wrong")
	t.Setenv("PI_PROFILE", "wrong")
	for _, provider := range []string{"claude", "codex", "omp", "opencode"} {
		t.Run(provider, func(t *testing.T) {
			dir := "/chosen-account"
			if provider == "opencode" {
				dir += "/opencode"
			}
			r := New(Config{Provider: provider, Binary: "/usr/bin/true", ProfileDir: dir})
			cmd, _, cleanup, err := r.command([]string{"-p", "PRIVATE EVIDENCE", "--model", "vendor/custom-model", "--allowedTools", "Read", "--disallowedTools", "Write"})
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if slices.Contains(cmd.Args, "PRIVATE EVIDENCE") {
				t.Fatal("evidence leaked through argv")
			}
			input, _ := io.ReadAll(cmd.Stdin)
			if string(input) != "PRIVATE EVIDENCE" {
				t.Fatalf("stdin = %q", input)
			}
			if !slices.Contains(cmd.Args, "vendor/custom-model") {
				t.Fatalf("model not passed: %v", cmd.Args)
			}
			key := map[string]string{"claude": "CLAUDE_CONFIG_DIR", "codex": "CODEX_HOME", "omp": "PI_CODING_AGENT_DIR", "opencode": "XDG_DATA_HOME"}[provider]
			if !slices.Contains(cmd.Env, key+"=/chosen-account") {
				t.Fatalf("wrong account environment: %s", key)
			}
			if provider == "omp" && (slices.Contains(cmd.Env, "PI_PROFILE=wrong") || slices.Contains(cmd.Env, "OMP_PROFILE=wrong")) {
				t.Fatal("inherited OMP profile overrode evaluator")
			}
			if provider == "codex" && !slices.Contains(cmd.Args, "read-only") {
				t.Fatal("Codex needs read-only sandbox")
			}
		})
	}
}
func TestOpenCodeEventsNeverSaveErrorsAsReports(t *testing.T) {
	out, err := openCodeOutput("{\"type\":\"step_start\"}\n{\"type\":\"text\",\"part\":{\"text\":\"report\"}}\n")
	if err != nil || out != "report\n" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := openCodeOutput(`{"type":"error","error":{"message":"no credentials"}}`); err == nil {
		t.Fatal("error accepted as report")
	}
}
func TestIdentityValidation(t *testing.T) {
	for _, id := range []string{"codex:profile:abc-123", "omp:profile:abcdef", "opencode:profile:ses_123", "omp:legacy.profile:parent/reader", testSID} {
		if !ValidSessionID(id) {
			t.Errorf("rejected %q", id)
		}
	}
	for _, id := range []string{"../../outside", "omp:../profile:id", "opencode:p:id/../x", "codex:p:id\n"} {
		if ValidSessionID(id) {
			t.Errorf("unsafe id %q", id)
		}
	}
	for _, m := range []string{"", "gpt-custom", "vendor/model:variant", "claude-opus[1m]"} {
		if !ValidModel(m) {
			t.Errorf("valid model rejected: %q", m)
		}
	}
	for _, m := range []string{"--help", "model\n--option", "model with spaces"} {
		if ValidModel(m) {
			t.Errorf("invalid model accepted: %q", m)
		}
	}
}
func TestCrossProviderAnalysisUsesSourceEvidenceAndRecordsEvaluator(t *testing.T) {
	e := newTestEnv(t)
	bin, argv, _ := fakeRecorder(t, e.tmp)
	sid := "omp:source:abc123"
	path := filepath.Join(e.tmp, "source.jsonl")
	os.WriteFile(path, []byte(`{"type":"session","id":"abc123","cwd":"/tmp"}`), 0600)
	if err := db.UpsertSession(e.db, db.SessionRow{ID: sid, NativeID: "abc123", Provider: "omp", Profile: "source", HasTranscript: true, UsageKnown: true, Requests: 9}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSessionSource(e.db, db.SessionSource{ID: sid, Path: path, Cwd: e.tmp, Format: "omp"}); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Provider: "claude", ProfileID: "evaluator", ClaudeBin: bin, ProfileDir: e.profileDir, SourceProvider: "omp", SourceProfile: "source", DataDir: e.dataDir, DB: e.db, AnalysisTimeout: time.Second * 5})
	if err := r.StartAnalysis(sid, "custom-model"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	run, ok, err := r.AnalysisStatus(sid)
	if err != nil || !ok || run.State != "done" || run.Profile != "evaluator" || run.SourceProvider != "omp" {
		t.Fatalf("run: %+v %v", run, err)
	}
	prompt, _ := os.ReadFile(argv + ".stdin")
	if !strings.Contains(string(prompt), path) || !strings.Contains(string(prompt), `"provider": "omp"`) {
		t.Fatalf("wrong source evidence: %s", prompt)
	}
}
func TestAdviceSnapshotUsesDatabaseScopeAndNoValuations(t *testing.T) {
	e := newTestEnv(t)
	for _, row := range []db.SessionRow{{ID: "omp:one:a", Provider: "omp", Profile: "one", Requests: 11, CostUSD: 987654}, {ID: "omp:two:b", Provider: "omp", Profile: "two", Requests: 99}, {ID: "codex:one:c", Provider: "codex", Profile: "one", Requests: 42}} {
		if err := db.UpsertSession(e.db, row); err != nil {
			t.Fatal(err)
		}
	}
	r := New(Config{DB: e.db, DataDir: e.dataDir, SourceProvider: "omp", SourceProfiles: []string{"one"}})
	dir, err := r.buildAdviceEvidence(AdviceScope{Provider: "omp", Profiles: []string{"one"}}, "standard")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "evidence.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var bundle struct {
		Total    int            `json:"total_sessions"`
		Sessions []sessionFacts `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Total != 1 || len(bundle.Sessions) != 1 || bundle.Sessions[0].Requests != 11 || strings.Contains(string(raw), "987654") || strings.Contains(string(raw), "cost_usd") {
		t.Fatalf("wrong scoped bundle: %s", raw)
	}
}

func TestCodexUsesFinalMessageFileInsteadOfProgressOutput(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	script := `#!/bin/sh
while [ "$#" -gt 0 ]; do
 if [ "$1" = "--output-last-message" ]; then shift; result="$1"; fi
 shift
done
cat >/dev/null
printf '%s' '{"schema":2,"headline":"Reviewed","summary":"Evidence checked"}' > "$result"
printf '%s' 'progress is not the report'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Provider: "codex", Binary: bin, ProfileDir: dir})
	out, _, runErr, timedOut, spawnErr := r.spawn([]string{"-p", "test evidence", "--model", "custom"}, time.Second*5)
	if runErr != nil || spawnErr != nil || timedOut || !strings.Contains(out, `"headline":"Reviewed"`) || strings.Contains(out, "progress") {
		t.Fatalf("wrong final output %q %v %v", out, runErr, spawnErr)
	}
}
