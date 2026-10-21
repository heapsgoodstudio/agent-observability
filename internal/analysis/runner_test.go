package analysis

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/report"
)

// testSID is a fixed, valid lowercase UUID with a transcript fixture behind
// it.
const testSID = "22222222-2222-4222-8222-222222222222"

// fakeJSON, fakeOK, fakeSlow and fakeFail are shell scripts standing in for
// the real `claude` binary, so no test ever spawns it (real money, real
// telemetry). fakeJSON prints the structured report the prompt contract asks
// for, and so exercises the report.json path; fakeOK prints markdown, the
// shape a model that ignored the contract produces, and so exercises the
// report.md fallback. `cat <<'EOF'` rather than echo for the JSON, so its
// quotes survive the shell.
const (
	fakeJSON = "#!/bin/sh\ncat <<'EOF'\n" +
		`{"schema":1,"verdict":"mixed","headline":"Six review rounds is the cost.","summary":"Reviews re-read the whole change each time.","problems":[{"title":"Reviewed over and over","cost":"large"}]}` +
		"\nEOF\n"
	fakeOK   = "#!/bin/sh\necho '# Report'\necho 'All fine.'\n"
	fakeSlow = "#!/bin/sh\nsleep 5\necho done\n"
	fakeFail = "#!/bin/sh\necho boom >&2\nexit 1\n"
)

// testEnv is the Go analogue of TestRunner.setUp: a temp profile dir holding
// one project's transcript under testSID, an indexed SQLite session and a
// legacy detail file for the explicit file-only runner tests.
type testEnv struct {
	t          *testing.T
	tmp        string
	profileDir string
	dataDir    string
	db         *sql.DB
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tmp := t.TempDir()

	profileDir := filepath.Join(tmp, "profile")
	proj := filepath.Join(profileDir, "projects", "-t-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, testSID+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(tmp, "data")
	sessionsDir := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	detail, err := json.Marshal(map[string]any{
		"id": testSID, "project": "-t-proj", "cost_usd": 1.5, "requests": 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, testSID+".json"), detail, 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(filepath.Join(tmp, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSession(d, db.SessionRow{ID: testSID, Project: "-t-proj", Requests: 3, HasTranscript: true}); err != nil {
		t.Fatal(err)
	}

	return &testEnv{t: t, tmp: tmp, profileDir: profileDir, dataDir: dataDir, db: d}
}

// runner builds a Runner against this env's fixtures. vlURL defaults to a
// port nothing listens on (127.0.0.1:1), so the subagent count query fails
// fast: no test reaches the real log store unless it stands up its own.
func (e *testEnv) runner(claudeBin string, withDB bool) *Runner {
	e.t.Helper()
	d := e.db
	if !withDB {
		d = nil
	}
	return New(Config{
		ClaudeBin:       claudeBin,
		ProfileDir:      e.profileDir,
		DataDir:         e.dataDir,
		DB:              d,
		VLURL:           "http://127.0.0.1:1",
		AnalysisTimeout: 30 * time.Second,
		EvidenceTimeout: 30 * time.Second,
	})
}

// fakeScript writes contents to a new executable file under dir and returns
// its path.
func fakeScript(t *testing.T, dir, contents string) string {
	t.Helper()
	f, err := os.CreateTemp(dir, "fake-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(contents); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

// fakeRecorder is a fake claude that records its argv to a file, one arg per
// line, and its working directory to a second file, so a test can assert the
// tool
// allowlist/denylist, model, and Config.WorkDir actually sent rather than
// assuming them.
func fakeRecorder(t *testing.T, dir string) (bin, argvPath, cwdPath string) {
	t.Helper()
	argvPath = filepath.Join(dir, "argv.txt")
	cwdPath = filepath.Join(dir, "cwd.txt")
	script := "#!/bin/sh\n" +
		`printf '%s\n' "$@" > "` + argvPath + `"` + "\n" +
		`cat > "` + argvPath + `.stdin"` + "\n" +
		`pwd > "` + cwdPath + `"` + "\n" +
		"echo '# Report'\n" +
		"echo 'All fine.'\n"
	return fakeScript(t, dir, script), argvPath, cwdPath
}

// readCWD reads back a cwd file fakeRecorder wrote and resolves symlinks, so
// it can be compared against a configured WorkDir even when the temp
// directory root itself is reached through a symlink (macOS's /tmp ->
// /private/tmp, for one).
func readCWD(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cwd file: %v", err)
	}
	return resolveDir(t, strings.TrimSpace(string(data)))
}

func resolveDir(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}

// captureStderr redirects the process-wide os.Stderr to a pipe for the rest
// of the test (restored on cleanup), and returns a function that closes the
// write end and returns everything written to it so far.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })

	return func() string {
		os.Stderr = orig
		w.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
}

// readArgv reads back an argv file fakeRecorder wrote, split into lines.
func readArgv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read argv file: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// argAfter returns the line right after marker, for reading a flag's value
// out of a recorded argv.
func argAfter(t *testing.T, argv []string, marker string) string {
	t.Helper()
	for i, a := range argv {
		if a == marker && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	t.Fatalf("marker %q not found in argv %v", marker, argv)
	return ""
}

// fileStatus is the status.json shape: the six base keys plus the
// advice-only session_id/subagents fields, all as pointers so a test can
// tell "absent from the JSON object" (nil) apart from "present as an
// explicit value".
type fileStatus struct {
	raw        map[string]any
	State      string  `json:"state"`
	Model      *string `json:"model"`
	Depth      *string `json:"depth"`
	StartedAt  *int64  `json:"started_at"`
	FinishedAt *int64  `json:"finished_at"`
	Error      *string `json:"error"`
	SessionID  *string `json:"session_id"`
	Subagents  *int64  `json:"subagents"`
}

func readStatus(t *testing.T, path string) fileStatus {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read status %s: %v", path, err)
	}
	var st fileStatus
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("parse status %s: %v", path, err)
	}
	if err := json.Unmarshal(data, &st.raw); err != nil {
		t.Fatalf("parse status %s as map: %v", path, err)
	}
	return st
}

// writeStatus plants a status.json at path, creating its directory. Used to
// stage states a test cannot reach by running the real spawn path, such as a
// run left mid-flight by a process that died.
func writeStatus(t *testing.T, path string, fields map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertFileAndDBAgree reads both the status.json at path and the sqlite
// row for (kind, key), and fails the test if any field disagrees between
// them. Every spawn-path test that runs with a db asserts this: the whole
// point of the dual write is that the two representations never drift. The file-only-mode tests (TestFileOnlyModeAnalysis/Advice)
// have no db to agree with, so they assert the absence of a row instead.
func assertFileAndDBAgree(t *testing.T, d *sql.DB, kind, key, path string) (fileStatus, *db.AnalysisRun) {
	t.Helper()
	fs := readStatus(t, path)
	row, ok, err := db.GetAnalysisRun(d, kind, key)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("no db row for %s/%s", kind, key)
	}
	if row.State != fs.State {
		t.Errorf("state: db=%q file=%q", row.State, fs.State)
	}
	if row.Model != strPtrOr(fs.Model) {
		t.Errorf("model: db=%q file=%v", row.Model, fs.Model)
	}
	if row.Depth != strPtrOr(fs.Depth) {
		t.Errorf("depth: db=%q file=%v", row.Depth, fs.Depth)
	}
	if row.Error != strPtrOr(fs.Error) {
		t.Errorf("error: db=%q file=%v", row.Error, fs.Error)
	}
	if row.RunSessionID != strPtrOr(fs.SessionID) {
		t.Errorf("session_id: db=%q file=%v", row.RunSessionID, fs.SessionID)
	}
	if row.StartedAt != int64PtrOr(fs.StartedAt) {
		t.Errorf("started_at: db=%v file=%v", row.StartedAt, fs.StartedAt)
	}
	if row.FinishedAt != int64PtrOr(fs.FinishedAt) {
		t.Errorf("finished_at: db=%v file=%v", row.FinishedAt, fs.FinishedAt)
	}
	if (row.Subagents == nil) != (fs.Subagents == nil) {
		t.Errorf("subagents nil-ness: db=%v file=%v", row.Subagents, fs.Subagents)
	} else if row.Subagents != nil && *row.Subagents != *fs.Subagents {
		t.Errorf("subagents: db=%d file=%d", *row.Subagents, *fs.Subagents)
	}
	return fs, row
}

func strPtrOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func int64PtrOr(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func analysisStatusPath(dataDir, sid string) string {
	return filepath.Join(dataDir, "analyses", sid, "status.json")
}

func adviceStatusPath(dataDir string) string {
	return filepath.Join(dataDir, "advice", "status.json")
}

// --- analyse path ----------------------------------------------------------

func TestStartAnalysisValidation(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)

	if err := r.StartAnalysis("nope", "sonnet"); err != ErrBadSessionID {
		t.Errorf("bad session id: err = %v, want ErrBadSessionID", err)
	}
	if err := r.StartAnalysis(testSID, "--invalid-model"); err != ErrBadModel {
		t.Errorf("bad model: err = %v, want ErrBadModel", err)
	}
	missing := "33333333-3333-4333-8333-333333333333"
	if err := r.StartAnalysis(missing, "sonnet"); err != ErrNoTranscript {
		t.Errorf("missing transcript: err = %v, want ErrNoTranscript", err)
	}

	// A transcript without a SQLite session is not indexed. Transcript
	// validation still precedes the database lookup.
	notIndexed := "44444444-4444-4444-8444-444444444444"
	proj := filepath.Join(e.profileDir, "projects", "-t-proj")
	if err := os.WriteFile(filepath.Join(proj, notIndexed+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.StartAnalysis(notIndexed, "sonnet"); err != ErrNotIndexed {
		t.Errorf("not indexed: err = %v, want ErrNotIndexed", err)
	}
	if r.Busy() {
		t.Error("Busy() = true after only rejected starts")
	}
}

func TestStartAnalysisHappyPath(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeJSON), true)

	if err := r.StartAnalysis(testSID, "haiku"); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	statusPath := analysisStatusPath(e.dataDir, testSID)
	fs, row := assertFileAndDBAgree(t, e.db, "analysis", testSID, statusPath)
	if fs.State != "done" {
		t.Errorf("state = %q, want done", fs.State)
	}
	if strPtrOr(fs.Model) != "haiku" {
		t.Errorf("model = %v, want haiku", fs.Model)
	}
	if fs.StartedAt == nil || fs.FinishedAt == nil {
		t.Error("started_at/finished_at should not be null on a done run")
	}
	if row.Error != "" {
		t.Errorf("db error = %q, want empty", row.Error)
	}

	data, err := os.ReadFile(filepath.Join(e.dataDir, "analyses", testSID, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := report.Parse(data)
	if err != nil {
		t.Fatalf("report.json did not parse back: %v", err)
	}
	if rep.Verdict != "mixed" || len(rep.Problems) != 1 {
		t.Errorf("report = %+v, want the verdict and one problem", rep)
	}
}

// The two landing paths, and the rule that keeps them from colliding. A run
// writes exactly one report file and removes the other: the server prefers
// report.json, so a stale one left beside a fresh report.md would quietly
// serve the previous run's report as though it were this one's.
func TestAnalysisStructuredRunWritesOnlyJSON(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeJSON), true)
	if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
		t.Fatalf("StartAnalysis: %v", err)
	}
	r.Wait()

	dir := filepath.Join(e.dataDir, "analyses", testSID)
	if _, err := os.Stat(filepath.Join(dir, "report.json")); err != nil {
		t.Fatalf("report.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "report.md")); !os.IsNotExist(err) {
		t.Errorf("report.md should not exist for a structured run, stat err = %v", err)
	}
}

func TestAnalysisFallsBackToMarkdown(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)
	if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
		t.Fatalf("StartAnalysis: %v", err)
	}
	r.Wait()

	dir := filepath.Join(e.dataDir, "analyses", testSID)
	md, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatalf("report.md: %v", err)
	}
	if !strings.Contains(string(md), "# Report") {
		t.Errorf("report.md = %q, want the raw output kept verbatim", md)
	}
	if _, err := os.Stat(filepath.Join(dir, "report.json")); !os.IsNotExist(err) {
		t.Errorf("report.json should not exist for an unparseable run, stat err = %v", err)
	}
	row, ok, err := r.AnalysisStatus(testSID)
	if err != nil || !ok {
		t.Fatalf("AnalysisStatus: %v ok=%v", err, ok)
	}
	if row.State != "done" {
		t.Errorf("state = %q, want done: an unparseable report is still a report", row.State)
	}
}

func TestRerunClearsTheOtherFormat(t *testing.T) {
	e := newTestEnv(t)
	dir := filepath.Join(e.dataDir, "analyses", testSID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A structured report from an earlier run, which the server would prefer.
	if err := os.WriteFile(filepath.Join(dir, "report.json"),
		[]byte(`{"schema":1,"headline":"old","summary":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)
	if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
		t.Fatalf("StartAnalysis: %v", err)
	}
	r.Wait()
	if _, err := os.Stat(filepath.Join(dir, "report.json")); !os.IsNotExist(err) {
		t.Errorf("the stale report.json survived a markdown re-run, stat err = %v", err)
	}
}

func TestRerunClearsAStaleMarkdownReport(t *testing.T) {
	e := newTestEnv(t)
	dir := filepath.Join(e.dataDir, "analyses", testSID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# old"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.runner(fakeScript(t, e.tmp, fakeJSON), true)
	if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
		t.Fatalf("StartAnalysis: %v", err)
	}
	r.Wait()
	if _, err := os.Stat(filepath.Join(dir, "report.md")); !os.IsNotExist(err) {
		t.Errorf("the stale report.md survived a structured re-run, stat err = %v", err)
	}
}

// --- orphaned runs ---------------------------------------------------------

// A run is a goroutine inside one serve process, and only that goroutine
// writes the terminal status, so a process that dies mid-run leaves a status
// stuck at "running" with nothing alive to finish it. This is what a fresh
// process does about it. Measured consequence of not doing it: a real run
// killed by a server restart left "running" on the page for the full timeout
// plus the 300s grace, 30 minutes, before the UI would even offer a retry.
func TestReapOrphanedRunsFailsEveryInFlightRun(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeJSON), true)

	// One orphaned analysis, one orphaned advice run, and one finished
	// analysis that must be left exactly as it is.
	writeStatus(t, analysisStatusPath(e.dataDir, testSID), map[string]any{
		"state": "running", "model": "opus", "started_at": 1000,
	})
	writeStatus(t, adviceStatusPath(e.dataDir), map[string]any{
		"state": "running", "model": "sonnet", "depth": "standard", "started_at": 1100,
	})
	doneSID := "55555555-5555-4555-8555-555555555555"
	writeStatus(t, analysisStatusPath(e.dataDir, doneSID), map[string]any{
		"state": "done", "model": "haiku", "started_at": 900, "finished_at": 950,
	})

	n, err := r.ReapOrphanedRuns()
	if err != nil {
		t.Fatalf("ReapOrphanedRuns: %v", err)
	}
	if n != 2 {
		t.Errorf("reaped %d, want 2", n)
	}

	for _, c := range []struct{ kind, key, path string }{
		{"analysis", testSID, analysisStatusPath(e.dataDir, testSID)},
		{"advice", "advice", adviceStatusPath(e.dataDir)},
	} {
		fs := readStatus(t, c.path)
		if fs.State != "error" {
			t.Errorf("%s/%s file state = %q, want error", c.kind, c.key, fs.State)
		}
		if fs.Error == nil || !strings.Contains(*fs.Error, "server restart") {
			t.Errorf("%s/%s error = %v, want it to name a server restart", c.kind, c.key, fs.Error)
		}
		if fs.FinishedAt == nil {
			t.Errorf("%s/%s finished_at is null, want it set", c.kind, c.key)
		}
		// The dual-write invariant has to hold here too, or the db and the
		// file disagree about the same run.
		row, ok, err := db.GetAnalysisRun(e.db, c.kind, c.key)
		if err != nil || !ok {
			t.Fatalf("%s/%s db row: err=%v ok=%v", c.kind, c.key, err, ok)
		}
		if row.State != "error" {
			t.Errorf("%s/%s db state = %q, want error", c.kind, c.key, row.State)
		}
	}

	// What was already terminal is untouched, model and timings included.
	fs := readStatus(t, analysisStatusPath(e.dataDir, doneSID))
	if fs.State != "done" || strPtrOr(fs.Model) != "haiku" {
		t.Errorf("a finished run was modified: state=%q model=%v", fs.State, fs.Model)
	}
}

// A file-only Runner (DB nil, which is what a fresh install serves with before
// the db opens) still has to repair the statuses it can see.
func TestReapOrphanedRunsWorksWithoutADB(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeJSON), false)
	writeStatus(t, analysisStatusPath(e.dataDir, testSID), map[string]any{
		"state": "running", "model": "opus", "started_at": 1000,
	})

	n, err := r.ReapOrphanedRuns()
	if err != nil {
		t.Fatalf("ReapOrphanedRuns: %v", err)
	}
	if n != 1 {
		t.Errorf("reaped %d, want 1", n)
	}
	if fs := readStatus(t, analysisStatusPath(e.dataDir, testSID)); fs.State != "error" {
		t.Errorf("state = %q, want error", fs.State)
	}
}

func TestReapOrphanedRunsIsANoOpWhenNothingIsRunning(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeJSON), true)
	n, err := r.ReapOrphanedRuns()
	if err != nil {
		t.Fatalf("ReapOrphanedRuns: %v", err)
	}
	if n != 0 {
		t.Errorf("reaped %d, want 0", n)
	}
}

func TestStartAnalysisBusyReturnsErrBusy(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, "#!/bin/sh\nsleep 2\necho ok\n"), true)

	if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
		t.Fatal(err)
	}
	if err := r.StartAnalysis(testSID, "sonnet"); err != ErrBusy {
		t.Errorf("second start: err = %v, want ErrBusy", err)
	}
	r.Wait()
}

func TestStartAnalysisTimeout(t *testing.T) {
	e := newTestEnv(t)
	r := New(Config{
		ClaudeBin:       fakeScript(t, e.tmp, fakeSlow),
		ProfileDir:      e.profileDir,
		DataDir:         e.dataDir,
		DB:              e.db,
		VLURL:           "http://127.0.0.1:1",
		AnalysisTimeout: 1 * time.Second,
		EvidenceTimeout: 30 * time.Second,
	})

	if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	fs, _ := assertFileAndDBAgree(t, e.db, "analysis", testSID, analysisStatusPath(e.dataDir, testSID))
	if fs.State != "error" {
		t.Errorf("state = %q, want error", fs.State)
	}
	if fs.Error == nil || !strings.Contains(*fs.Error, "timed out") {
		t.Errorf("error = %v, want it to contain 'timed out'", fs.Error)
	}
}

func TestStartAnalysisFailureCapturesStderr(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeFail), true)

	if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	fs, _ := assertFileAndDBAgree(t, e.db, "analysis", testSID, analysisStatusPath(e.dataDir, testSID))
	if fs.State != "error" {
		t.Errorf("state = %q, want error", fs.State)
	}
	if fs.Error == nil || !strings.Contains(*fs.Error, "boom") {
		t.Errorf("error = %v, want it to contain 'boom'", fs.Error)
	}
}

func TestStartAnalysisFailedDispatchResetsBusy(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)

	blocked := filepath.Join(e.dataDir, "analyses")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := r.StartAnalysis(testSID, "sonnet"); err == nil {
		t.Fatal("expected an error dispatching against a blocked analyses dir")
	}
	if r.Busy() {
		t.Error("Busy() = true after a failed dispatch, want false")
	}

	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
		t.Fatalf("start after clearing the blocker: %v", err)
	}
	r.Wait()
}

// TestStartAnalysisLogsWhenTheFinalStatusWriteFails covers the guard added
// to runAnalysis's terminal dualStatus call: without it, a failed final
// status write left a run stuck at "running" forever with nothing in any
// log to say why. fakeSlow buys enough real time for the test to swap
// status.json for a directory right after the synchronous initial "running"
// write has already created it, so the terminal write's os.Rename fails
// ("file exists") without touching the run directory's own permissions -
// report.md, written just before that terminal write, still needs to land
// normally, which is what tells the two failures apart.
func TestStartAnalysisLogsWhenTheFinalStatusWriteFails(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeSlow), true)

	if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
		t.Fatal(err)
	}

	statusPath := analysisStatusPath(e.dataDir, testSID)
	if err := os.Remove(statusPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statusPath, 0o755); err != nil {
		t.Fatal(err)
	}

	stderr := captureStderr(t)
	r.Wait()
	got := stderr()

	if !strings.Contains(got, "analysis: final status write failed for "+testSID) {
		t.Errorf("stderr = %q, want it to report the failed final write for %s", got, testSID)
	}

	reportPath := filepath.Join(e.dataDir, "analyses", testSID, "report.md")
	if _, err := os.Stat(reportPath); err != nil {
		t.Errorf("report.md should have been written before the status write failed: %v", err)
	}
}

// TestStartAnalysisSpawnsWithModelAndToolLists asserts the analyse path's
// full argv: --model, --allowedTools, and --disallowedTools all carry the
// exact values reference-facts.md specifies. Without this, dropping Glob
// from the allowedTools literal in runAnalysis would still pass every other
// test in this file.
func TestStartAnalysisSpawnsWithModelAndToolLists(t *testing.T) {
	e := newTestEnv(t)
	bin, argvPath, _ := fakeRecorder(t, e.tmp)
	r := e.runner(bin, true)

	if err := r.StartAnalysis(testSID, "opus"); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	assertFileAndDBAgree(t, e.db, "analysis", testSID, analysisStatusPath(e.dataDir, testSID))

	argv := readArgv(t, argvPath)
	if got := argAfter(t, argv, "--model"); got != "opus" {
		t.Errorf("--model = %q, want opus", got)
	}
	if got := argAfter(t, argv, "--allowedTools"); got != "Read,Grep,Glob" {
		t.Errorf("--allowedTools = %q, want Read,Grep,Glob", got)
	}
	if got := argAfter(t, argv, "--disallowedTools"); got != AnalysisDisallowedTools {
		t.Errorf("--disallowedTools = %q, want %q", got, AnalysisDisallowedTools)
	}
}

// --- advice path -------------------------------------------------------

func TestStartAdviceValidation(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)

	if _, err := r.StartAdvice("--invalid-model", "standard", []byte("[]")); err != ErrBadModel {
		t.Errorf("bad model: err = %v, want ErrBadModel", err)
	}
	if _, err := r.StartAdvice("sonnet", "deep", []byte("[]")); err != ErrBadDepth {
		t.Errorf("bad depth: err = %v, want ErrBadDepth", err)
	}
	huge := make([]byte, MaxFindingsBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	if _, err := r.StartAdvice("sonnet", "standard", huge); err != ErrFindingsTooLarge {
		t.Errorf("huge findings: err = %v, want ErrFindingsTooLarge", err)
	}
	if r.Busy() {
		t.Error("Busy() = true after only rejected starts")
	}
}

func TestStartAdviceHappyPath(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeJSON), true)

	findings := []byte(`[{"id":"real-money","severity":"high","title":"91% billed"}]`)
	sessionID, err := r.StartAdvice("haiku", "standard", findings)
	if err != nil {
		t.Fatal(err)
	}
	if sessionID == "" {
		t.Fatal("StartAdvice returned an empty session id")
	}
	r.Wait()

	statusPath := adviceStatusPath(e.dataDir)
	fs, _ := assertFileAndDBAgree(t, e.db, "advice", "advice", statusPath)
	if fs.State != "done" {
		t.Errorf("state = %q, want done", fs.State)
	}
	if strPtrOr(fs.SessionID) != sessionID {
		t.Errorf("recorded session_id = %v, want %q", fs.SessionID, sessionID)
	}

	data, err := os.ReadFile(filepath.Join(e.dataDir, "advice", "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := report.Parse(data); err != nil {
		t.Errorf("advice report.json did not parse back: %v", err)
	}
}

func TestStartAdviceRespectsTheSharedLock(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, "#!/bin/sh\nsleep 2\necho ok\n"), true)

	if err := r.StartAnalysis(testSID, "sonnet"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.StartAdvice("sonnet", "standard", []byte("[]")); err != ErrBusy {
		t.Errorf("advice while analyse is running: err = %v, want ErrBusy", err)
	}
	r.Wait()
}

func TestStartAdviceAbsentDepthDefaultsToStandard(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)

	if _, err := r.StartAdvice("haiku", "", []byte("[]")); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	fs, _ := assertFileAndDBAgree(t, e.db, "advice", "advice", adviceStatusPath(e.dataDir))
	if strPtrOr(fs.Depth) != "standard" {
		t.Errorf("depth = %v, want standard", fs.Depth)
	}
}

func TestStartAdviceEachDepthReachesTheRunner(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)

	for _, depth := range []string{"evidence", "shallow", "standard"} {
		t.Run(depth, func(t *testing.T) {
			if _, err := r.StartAdvice("haiku", depth, []byte("[]")); err != nil {
				t.Fatal(err)
			}
			r.Wait()
			fs, _ := assertFileAndDBAgree(t, e.db, "advice", "advice", adviceStatusPath(e.dataDir))
			if fs.State != "done" {
				t.Errorf("state = %q, want done", fs.State)
			}
			if strPtrOr(fs.Depth) != depth {
				t.Errorf("depth = %v, want %q", fs.Depth, depth)
			}
		})
	}
}

func TestStartAdviceDepthControlsToolAllowlist(t *testing.T) {
	e := newTestEnv(t)

	for _, depth := range []string{"evidence", "shallow", "standard"} {
		t.Run(depth, func(t *testing.T) {
			bin, argvPath, _ := fakeRecorder(t, e.tmp)
			r := e.runner(bin, true)
			if _, err := r.StartAdvice("haiku", depth, []byte("[]")); err != nil {
				t.Fatal(err)
			}
			r.Wait()
			assertFileAndDBAgree(t, e.db, "advice", "advice", adviceStatusPath(e.dataDir))

			argv := readArgv(t, argvPath)
			found := false
			for _, a := range argv {
				if a == "haiku" {
					found = true
				}
			}
			if !found {
				t.Errorf("argv %v does not contain model haiku", argv)
			}
			allowed := strings.Split(argAfter(t, argv, "--allowedTools"), ",")
			wantAgent := depth == "evidence"
			gotAgent := containsStr(allowed, "Agent")
			if gotAgent != wantAgent {
				t.Errorf("depth %s: Agent in allowedTools = %v, want %v", depth, gotAgent, wantAgent)
			}
			if !containsStr(allowed, "Bash") {
				t.Errorf("depth %s: Bash missing from allowedTools %v", depth, allowed)
			}
		})
	}
}

func TestStartAdviceDepthControlsToolDenylist(t *testing.T) {
	e := newTestEnv(t)

	for _, depth := range []string{"evidence", "shallow", "standard"} {
		t.Run(depth, func(t *testing.T) {
			bin, argvPath, _ := fakeRecorder(t, e.tmp)
			r := e.runner(bin, true)
			if _, err := r.StartAdvice("haiku", depth, []byte("[]")); err != nil {
				t.Fatal(err)
			}
			r.Wait()
			assertFileAndDBAgree(t, e.db, "advice", "advice", adviceStatusPath(e.dataDir))

			argv := readArgv(t, argvPath)
			denied := strings.Split(argAfter(t, argv, "--disallowedTools"), ",")
			wantAgent := depth != "evidence"
			gotAgent := containsStr(denied, "Agent")
			if gotAgent != wantAgent {
				t.Errorf("depth %s: Agent in disallowedTools = %v, want %v", depth, gotAgent, wantAgent)
			}
			if containsStr(denied, "Bash") {
				t.Errorf("depth %s: Bash should never be denied, got %v", depth, denied)
			}
			for _, tool := range []string{"Write", "Edit", "NotebookEdit"} {
				if !containsStr(denied, tool) {
					t.Errorf("depth %s: %s missing from disallowedTools %v", depth, tool, denied)
				}
			}
		})
	}
}

func TestStartAdviceSpawnsWithTheSessionIDItRecords(t *testing.T) {
	e := newTestEnv(t)
	bin, argvPath, _ := fakeRecorder(t, e.tmp)
	r := e.runner(bin, true)

	sessionID, err := r.StartAdvice("haiku", "standard", []byte("[]"))
	if err != nil {
		t.Fatal(err)
	}
	r.Wait()

	fs, _ := assertFileAndDBAgree(t, e.db, "advice", "advice", adviceStatusPath(e.dataDir))
	if strPtrOr(fs.SessionID) != sessionID {
		t.Errorf("recorded session_id = %v, want %q", fs.SessionID, sessionID)
	}
	argv := readArgv(t, argvPath)
	if got := argAfter(t, argv, "--session-id"); got != sessionID {
		t.Errorf("--session-id = %q, want %q", got, sessionID)
	}
}

func TestBothSpawnPathsDenyTheMutatingTools(t *testing.T) {
	e := newTestEnv(t)

	binA, argvPathA, _ := fakeRecorder(t, e.tmp)
	rA := e.runner(binA, true)
	if err := rA.StartAnalysis(testSID, "haiku"); err != nil {
		t.Fatal(err)
	}
	rA.Wait()
	assertFileAndDBAgree(t, e.db, "analysis", testSID, analysisStatusPath(e.dataDir, testSID))
	argvA := readArgv(t, argvPathA)
	if got := argAfter(t, argvA, "--disallowedTools"); got != AnalysisDisallowedTools {
		t.Errorf("analyse disallowedTools = %q, want %q", got, AnalysisDisallowedTools)
	}

	binB, argvPathB, _ := fakeRecorder(t, e.tmp)
	rB := e.runner(binB, true)
	if _, err := rB.StartAdvice("haiku", "standard", []byte("[]")); err != nil {
		t.Fatal(err)
	}
	rB.Wait()
	assertFileAndDBAgree(t, e.db, "advice", "advice", adviceStatusPath(e.dataDir))
	argvB := readArgv(t, argvPathB)
	if got := argAfter(t, argvB, "--disallowedTools"); got != AdviceDisallowedTools["standard"] {
		t.Errorf("advice disallowedTools = %q, want %q", got, AdviceDisallowedTools["standard"])
	}
}

// --- WorkDir (cwd) ----------------------------------------------------

// TestStartAnalysisSpawnsWithConfiguredWorkDir pins Config.WorkDir onto the
// analyse spawn path: without it the spawned claude inherits whatever
// launched this binary instead of the project checkout, breaking its own
// telemetry attribution and project-scoped
// CLAUDE.md/.claude/settings.local.json loading.
func TestStartAnalysisSpawnsWithConfiguredWorkDir(t *testing.T) {
	e := newTestEnv(t)
	bin, _, cwdPath := fakeRecorder(t, e.tmp)
	workDir := filepath.Join(e.tmp, "workdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	r := New(Config{
		ClaudeBin:       bin,
		ProfileDir:      e.profileDir,
		DataDir:         e.dataDir,
		DB:              e.db,
		VLURL:           "http://127.0.0.1:1",
		WorkDir:         workDir,
		AnalysisTimeout: 30 * time.Second,
		EvidenceTimeout: 30 * time.Second,
	})

	if err := r.StartAnalysis(testSID, "haiku"); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	if got, want := readCWD(t, cwdPath), resolveDir(t, workDir); got != want {
		t.Errorf("spawned cwd = %q, want %q", got, want)
	}
}

// TestStartAdviceSpawnsWithConfiguredWorkDir is
// TestStartAnalysisSpawnsWithConfiguredWorkDir's advice-path twin. An advice
// run is Bash-armed at every depth, so a missing cwd also means its Bash
// calls resolve relative paths from the wrong place.
func TestStartAdviceSpawnsWithConfiguredWorkDir(t *testing.T) {
	e := newTestEnv(t)
	bin, _, cwdPath := fakeRecorder(t, e.tmp)
	workDir := filepath.Join(e.tmp, "workdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	r := New(Config{
		ClaudeBin:       bin,
		ProfileDir:      e.profileDir,
		DataDir:         e.dataDir,
		DB:              e.db,
		VLURL:           "http://127.0.0.1:1",
		WorkDir:         workDir,
		AnalysisTimeout: 30 * time.Second,
		EvidenceTimeout: 30 * time.Second,
	})

	if _, err := r.StartAdvice("haiku", "standard", []byte("[]")); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	if got, want := readCWD(t, cwdPath), resolveDir(t, workDir); got != want {
		t.Errorf("spawned cwd = %q, want %q", got, want)
	}
}

// TestSpawnLeavesWorkDirUnsetWhenConfigDoesNotSetIt pins the empty-means-
// unset half of the contract: e.runner never sets WorkDir, so the spawned
// process should inherit this test binary's own cwd rather than some
// unrelated default.
func TestSpawnLeavesWorkDirUnsetWhenConfigDoesNotSetIt(t *testing.T) {
	e := newTestEnv(t)
	bin, _, cwdPath := fakeRecorder(t, e.tmp)
	r := e.runner(bin, true)

	wantCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	wantCWD = resolveDir(t, wantCWD)

	if err := r.StartAnalysis(testSID, "haiku"); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	if got := readCWD(t, cwdPath); got != wantCWD {
		t.Errorf("spawned cwd = %q, want this process's cwd %q (WorkDir unset)", got, wantCWD)
	}
}

// TestStartAdviceOmitsTheSubagentCountWhenTheQueryFails asserts the "unknown,
// never zero" contract: the runner's VLURL points at 127.0.0.1:1 (testEnv's
// default), which nothing listens on, so count_subagents fails fast.
//
// The subagents key is not literally absent from the JSON: the dispatch-time
// write (reference-facts.md: "clears session_id and subagents to null before
// starting") already put an explicit null there, and the completion write
// simply never overlays it, so it stays null rather than becoming a zero.
// That is the "omitted from this call" the brief describes, one merge layer
// down from the byte on disk.
func TestStartAdviceOmitsTheSubagentCountWhenTheQueryFails(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)

	if _, err := r.StartAdvice("haiku", "standard", []byte("[]")); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	fs, row := assertFileAndDBAgree(t, e.db, "advice", "advice", adviceStatusPath(e.dataDir))
	if fs.State != "done" {
		t.Errorf("state = %q, want done", fs.State)
	}
	if _, present := fs.raw["subagents"]; !present {
		t.Error(`"subagents" key absent from status.json, want present as explicit null`)
	}
	if fs.Subagents != nil {
		t.Errorf("subagents = %d, want null", *fs.Subagents)
	}
	if row.Subagents != nil {
		t.Errorf("db subagents = %d, want nil (never zero)", *row.Subagents)
	}
}

func TestStartAdviceRecordsTheSubagentCountTheStoreReports(t *testing.T) {
	e := newTestEnv(t)
	vl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/stream+json")
		fmt.Fprint(w, `{"n":"7"}`+"\n")
	}))
	defer vl.Close()

	r := New(Config{
		ClaudeBin:       fakeScript(t, e.tmp, fakeOK),
		ProfileDir:      e.profileDir,
		DataDir:         e.dataDir,
		DB:              e.db,
		VLURL:           vl.URL,
		AnalysisTimeout: 30 * time.Second,
		EvidenceTimeout: 30 * time.Second,
	})

	if _, err := r.StartAdvice("haiku", "evidence", []byte("[]")); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	fs, row := assertFileAndDBAgree(t, e.db, "advice", "advice", adviceStatusPath(e.dataDir))
	if fs.Subagents == nil || *fs.Subagents != 7 {
		t.Errorf("subagents = %v, want 7", fs.Subagents)
	}
	if row.Subagents == nil || *row.Subagents != 7 {
		t.Errorf("db subagents = %v, want 7", row.Subagents)
	}
}

// TestStartAdviceFailedDispatchResetsBusy mirrors
// TestStartAnalysisFailedDispatchResetsBusy for the advice path: the two
// share the exact same dualStatus/acquire/release machinery, but the advice
// dispatch takes a different branch (kind="advice", key="advice", and a
// freshly generated session id), so it gets its own test rather than relying
// on the analyse test alone to prove the shared code path.
func TestStartAdviceFailedDispatchResetsBusy(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)

	blocked := filepath.Join(e.dataDir, "advice")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.StartAdvice("sonnet", "standard", []byte("[]")); err == nil {
		t.Fatal("expected an error dispatching against a blocked advice dir")
	}
	if r.Busy() {
		t.Error("Busy() = true after a failed dispatch, want false")
	}

	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if _, err := r.StartAdvice("sonnet", "standard", []byte("[]")); err != nil {
		t.Fatalf("start after clearing the blocker: %v", err)
	}
	r.Wait()
}

// --- file-only mode (DB nil) ---------------------------------------------

func TestFileOnlyModeAnalysis(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), false)

	if err := r.StartAnalysis(testSID, "haiku"); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	fs := readStatus(t, analysisStatusPath(e.dataDir, testSID))
	if fs.State != "done" {
		t.Errorf("state = %q, want done", fs.State)
	}

	row, ok, err := db.GetAnalysisRun(e.db, "analysis", testSID)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("db row present in file-only mode: %+v", row)
	}
}

func TestFileOnlyModeAdvice(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), false)

	if _, err := r.StartAdvice("haiku", "standard", []byte("[]")); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	fs := readStatus(t, adviceStatusPath(e.dataDir))
	if fs.State != "done" {
		t.Errorf("state = %q, want done", fs.State)
	}

	row, ok, err := db.GetAnalysisRun(e.db, "advice", "advice")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("db row present in file-only mode: %+v", row)
	}
}

// --- status fallback (db configured, but no row for this run) -------------

func TestAnalysisStatusFallsBackToFileWhenNoDBRow(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)

	path := analysisStatusPath(e.dataDir, testSID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A hand-written status.json with no db row behind it, as a file-only
	// Runner would have left, or a run whose db row was lost.
	if err := os.WriteFile(path, []byte(`{"state":"done","model":"opus","depth":null,`+
		`"started_at":1755000000,"finished_at":1755000600,"error":null}`), 0o644); err != nil {
		t.Fatal(err)
	}

	row, ok, err := r.AnalysisStatus(testSID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false, want true (fallback to file)")
	}
	if row.State != "done" || row.Model != "opus" {
		t.Errorf("row = %+v, want state=done model=opus", row)
	}
	if row.Depth != "" {
		t.Errorf("depth = %q, want empty (null in file)", row.Depth)
	}
	if row.Subagents != nil {
		t.Errorf("subagents = %v, want nil (absent in file)", row.Subagents)
	}
}

func TestAnalysisStatusMissingReturnsNotOK(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)

	row, ok, err := r.AnalysisStatus("55555555-5555-4555-8555-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("ok = true, want false; row = %+v", row)
	}
	if row != nil {
		t.Fatalf("row = %+v, want nil", row)
	}
}

func TestAdviceStatusFallsBackToFileWhenNoDBRow(t *testing.T) {
	e := newTestEnv(t)
	r := e.runner(fakeScript(t, e.tmp, fakeOK), true)

	path := adviceStatusPath(e.dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"state":"error","model":"sonnet","depth":"shallow",`+
		`"started_at":1755000000,"finished_at":1755000100,"error":"boom",`+
		`"session_id":"11111111-1111-4111-8111-111111111111","subagents":3}`), 0o644); err != nil {
		t.Fatal(err)
	}

	row, ok, err := r.AdviceStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false, want true (fallback to file)")
	}
	if row.State != "error" || row.Error != "boom" {
		t.Errorf("row = %+v, want state=error error=boom", row)
	}
	if row.Subagents == nil || *row.Subagents != 3 {
		t.Errorf("subagents = %v, want 3", row.Subagents)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// --- default profile CLAUDE_CONFIG_DIR ---------------------------------

// TestSpawnEnvSetsConfigDirForANamedProfile is the ordinary case.
func TestSpawnEnvSetsConfigDirForANamedProfile(t *testing.T) {
	env := spawnEnv([]string{"PATH=/usr/bin"}, "/Users/x/Claude/.claude-acme")
	want := "CLAUDE_CONFIG_DIR=/Users/x/Claude/.claude-acme"
	if !slices.Contains(env, want) {
		t.Errorf("missing %q in %v", want, env)
	}
}

// TestSpawnEnvUnsetsConfigDirForTheDefaultProfile pins the one case where the
// variable must be absent rather than set.
//
// Claude Code keeps the default profile's config at ~/.claude.json, and only
// moves it inside a directory when CLAUDE_CONFIG_DIR names that directory. So
// exporting CLAUDE_CONFIG_DIR=$HOME/.claude does not describe the default
// profile, it creates a second one beside it, and a run spawned that way reads
// different settings and a different account cache from the profile the
// operator asked to instrument.
//
// The inherited value must be removed, not merely left alone: this process
// inherits launchd's environment, and the operator's shell may export a
// CLAUDE_CONFIG_DIR of its own.
func TestSpawnEnvUnsetsConfigDirForTheDefaultProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := []string{"PATH=/usr/bin", "CLAUDE_CONFIG_DIR=/inherited/from/somewhere"}

	env := spawnEnv(base, filepath.Join(home, ".claude"))

	for _, e := range env {
		if strings.HasPrefix(e, "CLAUDE_CONFIG_DIR=") {
			t.Fatalf("CLAUDE_CONFIG_DIR survived for the default profile: %q", e)
		}
	}
	if !slices.Contains(env, "PATH=/usr/bin") {
		t.Error("dropped an unrelated variable")
	}
}

func TestProfileRunnersShareFlightAndBusyState(t *testing.T) {
	first, second := newTestEnv(t), newTestEnv(t)
	gate := &FlightGate{}
	// The root runner only reads status. Profile runners perform the work.
	observer := New(Config{FlightGate: gate})
	released := filepath.Join(first.tmp, "release")
	script := "#!/bin/sh\nwhile [ ! -f \"" + released + "\" ]; do sleep 0.01; done\necho '# Report'\n"
	binary := fakeScript(t, first.tmp, script)
	makeRunner := func(e *testEnv) *Runner {
		return New(Config{FlightGate: gate, ClaudeBin: binary, ProfileDir: e.profileDir,
			DataDir: first.dataDir, VLURL: "http://127.0.0.1:1", AnalysisTimeout: 5 * time.Second})
	}
	a, b := makeRunner(first), makeRunner(second)
	if err := a.StartAnalysis(testSID, "sonnet"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.WriteFile(released, []byte("release"), 0o600); a.Wait(); b.Wait() })
	if !a.Busy() || !b.Busy() || !observer.Busy() {
		t.Fatal("profile work is invisible to another runner's busy state")
	}
	if err := b.StartAnalysis(testSID, "sonnet"); err != ErrBusy {
		t.Fatalf("second profile analysis: %v", err)
	}
	if _, err := b.StartAdvice("sonnet", "standard", []byte("[]")); err != ErrBusy {
		t.Fatalf("second profile advice: %v", err)
	}
	if err := b.StartAnalysis("invalid", "sonnet"); err != ErrBadSessionID {
		t.Fatalf("validation must precede shared busy check: %v", err)
	}
	if _, err := b.StartAdvice("--invalid", "standard", nil); err != ErrBadModel {
		t.Fatalf("advice validation must precede busy check: %v", err)
	}
	if err := os.WriteFile(released, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if a.Busy() || b.Busy() || observer.Busy() {
		t.Fatal("completion did not release shared gate")
	}
	if _, err := b.StartAdvice("sonnet", "standard", []byte("[]")); err != nil {
		t.Fatalf("next profile could not start after release: %v", err)
	}
	b.Wait()
	if observer.Busy() {
		t.Fatal("advice did not release shared gate")
	}
}
