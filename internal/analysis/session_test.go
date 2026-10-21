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

func TestSessionAnalysisUsesRecordedWorkspaceAndPreservesRunnerDefault(t *testing.T) {
	e := newTestEnv(t)
	workspace := filepath.Join(e.tmp, "actual-workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	// The highest ranked directory has since been removed. The next valid
	// workspace wins, rather than the install directory or a relative cwd.
	var log strings.Builder
	for _, cwd := range []string{"relative", "relative", filepath.Join(e.tmp, "removed"), filepath.Join(e.tmp, "removed"), workspace} {
		line, _ := json.Marshal(map[string]string{"type": "user", "cwd": cwd})
		log.Write(line)
		log.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(e.profileDir, "projects", "-t-proj", testSID+".jsonl"), []byte(log.String()), 0600); err != nil {
		t.Fatal(err)
	}
	bin, argsPath, cwdPath := fakeRecorder(t, e.tmp)
	r := e.runner(bin, true)
	r.cfg.WorkDir = e.tmp
	if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if got := readCWD(t, cwdPath); got != resolveDir(t, workspace) {
		t.Fatalf("analysis cwd = %q, wanted session workspace", got)
	}
	if r.cfg.WorkDir != e.tmp {
		t.Fatal("per-session directory mutated shared runner config")
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "disable-hooks") || strings.Contains(string(args), "disableAllHooks") {
		t.Fatal("analysis disabled profile hooks")
	}
}

func TestHookBlockedRunIsAnErrorEvenWhenCLIExitsSuccessfully(t *testing.T) {
	for _, kind := range []string{"analysis", "advice"} {
		t.Run(kind, func(t *testing.T) {
			e := newTestEnv(t)
			output := "UserPromptSubmit operation blocked by hook:\n[repo-guard]: wrong workspace.\n\nOriginal prompt: PRIVATE\n"
			bin := fakeScript(t, e.tmp, "#!/bin/sh\ncat <<'DIAGNOSTIC'\n"+output+"DIAGNOSTIC\nexit 0\n")
			r := e.runner(bin, true)
			if kind == "analysis" {
				if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := r.StartAdvice("sonnet", "standard", []byte("[]")); err != nil {
					t.Fatal(err)
				}
			}
			r.Wait()
			status, ok, err := r.AnalysisStatus(testSID)
			dir := filepath.Join(e.dataDir, "analyses", testSID)
			if kind == "advice" {
				status, ok, err = r.AdviceStatus()
				dir = filepath.Join(e.dataDir, "advice")
			}
			if err != nil || !ok || status.State != "error" || !strings.Contains(status.Error, "wrong workspace") || strings.Contains(status.Error, "PRIVATE") {
				t.Fatalf("hook failure misreported: %+v %v", status, err)
			}
			for _, name := range []string{"report.json", "report.md"} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("hook output saved as a report: %s", name)
				}
			}
		})
	}
}

func TestAnalysisUsesSQLiteWithoutLegacySessionFiles(t *testing.T) {
	for _, legacy := range []string{"absent", "stale", "malformed"} {
		t.Run(legacy, func(t *testing.T) {
			e := newTestEnv(t)
			path := filepath.Join(e.dataDir, "sessions", testSID+".json")
			if legacy == "absent" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if legacy == "malformed" {
				if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			row := db.SessionRow{ID: testSID, Project: "current-project", Requests: 77, Tokens: transcript.Tokens{Input: 456}, Tools: map[string]int{"Read": 9}, Models: []transcript.ModelCost{{Model: "claude-test", CostUSD: 98765}}, CostUSD: 98765, Trajectory: [][3]float64{{123, 98765, 4096}}}
			if err := db.UpsertSession(e.db, row); err != nil {
				t.Fatal(err)
			}
			bin, argvPath, _ := fakeRecorder(t, e.tmp)
			r := e.runner(bin, true)
			if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
				t.Fatal(err)
			}
			r.Wait()
			status, ok, err := r.AnalysisStatus(testSID)
			if err != nil || !ok || status.State != "done" {
				t.Fatalf("analysis failed: %+v %v", status, err)
			}
			args, err := os.ReadFile(argvPath + ".stdin")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"current-project, 77 requests", `"requests": 77`, `"input": 456`, `"Read": 9`, `"claude-test"`, `"input_tokens": 4096`} {
				if !strings.Contains(string(args), want) {
					t.Errorf("prompt missing current indexed fact %q", want)
				}
			}
			for _, absent := range []string{path, "98765", `"cost_usd"`, "{SESSION_FACTS_JSON}"} {
				if strings.Contains(string(args), absent) {
					t.Errorf("prompt included obsolete input %q", absent)
				}
			}
			if legacy == "absent" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("analysis recreated a session JSON projection: %v", err)
				}
			}
		})
	}
}

func TestAnalysisDoesNotFallBackToStaleFilesWhenIndexFails(t *testing.T) {
	for _, missing := range []bool{true, false} {
		e := newTestEnv(t)
		bin, argvPath, _ := fakeRecorder(t, e.tmp)
		r := e.runner(bin, true)
		if missing {
			if _, err := e.db.Exec("DELETE FROM sessions WHERE id = ?", testSID); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := e.db.Close(); err != nil {
				t.Fatal(err)
			}
		}
		err := r.StartAnalysis(testSID, "sonnet")
		if err == nil || (missing && err != ErrNotIndexed) || (!missing && err == ErrNotIndexed) {
			t.Fatalf("missing=%v: wrong index error %v", missing, err)
		}
		if r.Busy() {
			t.Fatal("rejected analysis claimed the runner")
		}
		if _, err := os.Stat(argvPath); !os.IsNotExist(err) {
			t.Fatal("rejected analysis spawned inference")
		}
	}
}
