package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1/claudeobsv1connect"
	"github.com/heapsgoodstudio/agent-observability/internal/analysis"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vl"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// analysisTestSID matches internal/analysis's own runner_test.go SID: a
// fixed, valid lowercase UUID with a transcript fixture behind it.
const analysisTestSID = "22222222-2222-4222-8222-222222222222"

// Fake scripts standing in for the real `claude` binary, mirroring
// internal/analysis's own fakeOK/fakeSlow (unexported there, so this test
// keeps its own small copies rather than reaching into that package).
const (
	fakeClaudeOK   = "#!/bin/sh\necho '# Report'\necho 'All fine.'\n"
	fakeClaudeSlow = "#!/bin/sh\nsleep 2\necho done\n"
	// fakeClaudeJSON prints a structured report, the shape the prompt contract
	// asks for. `cat <<'EOF'` rather than echo, so the JSON's quotes survive
	// the shell.
	fakeClaudeJSON = "#!/bin/sh\ncat <<'EOF'\n" +
		`{"schema":1,"verdict":"mixed","headline":"H","summary":"S"}` +
		"\nEOF\n"
)

// analysisTestEnv is a minimal analogue of internal/analysis's testEnv, built
// only from that package's exported API (Config, New) since its own test
// helpers are unexported.
type analysisTestEnv struct {
	t          *testing.T
	tmp        string
	profileDir string
	dataDir    string
	db         *sql.DB
}

func newAnalysisTestEnv(t *testing.T) *analysisTestEnv {
	t.Helper()
	tmp := t.TempDir()

	profileDir := filepath.Join(tmp, "profile")
	proj := filepath.Join(profileDir, "projects", "-t-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, analysisTestSID+".jsonl"), []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(tmp, "data")
	sessionsDir := filepath.Join(dataDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	detail, err := json.Marshal(map[string]any{
		"id": analysisTestSID, "project": "-t-proj", "cost_usd": 1.5, "requests": 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, analysisTestSID+".json"), detail, 0o644); err != nil {
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
	if err := db.UpsertSession(d, db.SessionRow{ID: analysisTestSID, Project: "-t-proj", Requests: 3, HasTranscript: true}); err != nil {
		t.Fatal(err)
	}

	return &analysisTestEnv{t: t, tmp: tmp, profileDir: profileDir, dataDir: dataDir, db: d}
}

// fakeScript writes contents to a new executable file under e.tmp and
// returns its path.
func (e *analysisTestEnv) fakeScript(contents string) string {
	e.t.Helper()
	f, err := os.CreateTemp(e.tmp, "fake-*.sh")
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := f.WriteString(contents); err != nil {
		e.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		e.t.Fatal(err)
	}
	return f.Name()
}

// fakeRecorder is a fake claude that records its argv to a file (one arg per
// line; a multi-line -p prompt still lands as one "line" with its own
// embedded newlines, which is fine for a substring check).
func (e *analysisTestEnv) fakeRecorder() (bin, argvPath string) {
	e.t.Helper()
	argvPath = filepath.Join(e.tmp, "argv.txt")
	script := "#!/bin/sh\n" +
		`printf '%s\n' "$@" > "` + argvPath + `"` + "\n" +
		`cat > "` + argvPath + `.stdin"` + "\n" +
		"echo '# Report'\n" +
		"echo 'All fine.'\n"
	return e.fakeScript(script), argvPath
}

// runner builds an analysis.Runner against this env's fixtures, using
// timeout for both the analysis and evidence budgets.
func (e *analysisTestEnv) runner(claudeBin string, timeout time.Duration) *analysis.Runner {
	e.t.Helper()
	return analysis.New(analysis.Config{
		ClaudeBin:       claudeBin,
		ProfileDir:      e.profileDir,
		DataDir:         e.dataDir,
		DB:              e.db,
		VLURL:           "http://127.0.0.1:1",
		AnalysisTimeout: timeout,
		EvidenceTimeout: timeout,
	})
}

// options builds Options wired to r, with a Memo (required: buildFindings
// calls Memo.Do unconditionally, so a nil Memo would panic before ever
// reaching a VM/VL client's own nil-safety).
func (e *analysisTestEnv) options(r *analysis.Runner) Options {
	return Options{Runner: r, DataDir: e.dataDir, DB: e.db, StaticDir: e.tmp, Memo: store.New(store.TTL)}
}

// failingStore stands up two httptest servers that answer every VictoriaLogs
// and VictoriaMetrics query with a 500, matching
// TestBuildFindingsErrorsWhenAllSourcesFail's own construction. Paired with a
// nil Options.DB (see TestStartAdviceFindingsAssemblyFailureDegradesToEmptyList,
// its one caller that needs this), buildFindings has nothing left to compute
// and returns an error, the "assembly failure" StartAdvice must degrade to
// "[]" rather than block on.
func failingStore(t *testing.T) (vmClient *vm.Client, vlClient *vl.Client, cleanup func()) {
	t.Helper()
	vmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	vlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	return &vm.Client{BaseURL: vmSrv.URL, HTTP: vmSrv.Client()},
		&vl.Client{BaseURL: vlSrv.URL, HTTP: vlSrv.Client()},
		func() { vmSrv.Close(); vlSrv.Close() }
}

func newAnalysisClient(t *testing.T, o Options) claudeobsv1connect.ObsServiceClient {
	t.Helper()
	srv := httptest.NewServer(Handler(o))
	t.Cleanup(srv.Close)
	return claudeobsv1connect.NewObsServiceClient(http.DefaultClient, srv.URL)
}

/* --- GetMeta ------------------------------------------------------------ */

func TestGetMetaCarriesAnalysisDefaultsAndLists(t *testing.T) {
	client := newAnalysisClient(t, Options{StaticDir: t.TempDir()})
	resp, err := client.GetMeta(context.Background(), connect.NewRequest(&claudeobsv1.GetMetaRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Msg
	if m.AnalysisTimeoutS != 1500 {
		t.Errorf("AnalysisTimeoutS = %d, want 1500", m.AnalysisTimeoutS)
	}
	if m.AdviceEvidenceTimeoutS != 2700 {
		t.Errorf("AdviceEvidenceTimeoutS = %d, want 2700", m.AdviceEvidenceTimeoutS)
	}
	if got := strings.Join(m.AnalysisModels, ","); got != "haiku,opus,sonnet" {
		t.Errorf("AnalysisModels = %v, want sorted [haiku opus sonnet]", m.AnalysisModels)
	}
	if got := strings.Join(m.AdviceDepths, ","); got != "evidence,shallow,standard" {
		t.Errorf("AdviceDepths = %v, want sorted [evidence shallow standard]", m.AdviceDepths)
	}
}

/* --- StartAnalysis validation matrix ------------------------------------ */

func TestStartAnalysisUsesIndexedProfileWithoutLegacyJSON(t *testing.T) {
	e := newAnalysisTestEnv(t)
	if err := os.Remove(filepath.Join(e.dataDir, "sessions", analysisTestSID+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec("UPDATE sessions SET profile = ? WHERE id = ?", "secondary", analysisTestSID); err != nil {
		t.Fatal(err)
	}
	r := e.runner(e.fakeScript(fakeClaudeOK), 30*time.Second)
	o := e.options(r)
	o.RunnerForProfile = func(profile string) *analysis.Runner {
		if profile == "secondary" {
			return r
		}
		return nil
	}
	client := newAnalysisClient(t, o)
	if _, err := client.StartAnalysis(context.Background(), connect.NewRequest(&claudeobsv1.StartAnalysisRequest{Id: analysisTestSID, Model: "sonnet"})); err != nil {
		t.Fatalf("SQLite-indexed session could not start analysis: %v", err)
	}
	r.Wait()
	status, ok, err := r.AnalysisStatus(analysisTestSID)
	if err != nil || !ok || status.State != "done" {
		t.Fatalf("profile analysis did not finish: %+v %v", status, err)
	}
}

func TestStartAnalysisProfileLookupReportsDatabaseFailure(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := e.runner(e.fakeScript(fakeClaudeOK), 30*time.Second)
	o := e.options(r)
	o.RunnerForProfile = func(string) *analysis.Runner { t.Error("runner selected after database failure"); return r }
	if err := e.db.Close(); err != nil {
		t.Fatal(err)
	}
	client := newAnalysisClient(t, o)
	_, err := client.StartAnalysis(context.Background(), connect.NewRequest(&claudeobsv1.StartAnalysisRequest{Id: analysisTestSID, Model: "sonnet"}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("database failure was misreported as unindexed: %v", err)
	}
}

func TestStartAnalysisValidationCodes(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := e.runner(e.fakeScript(fakeClaudeOK), 30*time.Second)
	client := newAnalysisClient(t, e.options(r))
	ctx := context.Background()

	notIndexed := "44444444-4444-4444-8444-444444444444"
	proj := filepath.Join(e.profileDir, "projects", "-t-proj")
	if err := os.WriteFile(filepath.Join(proj, notIndexed+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		id       string
		model    string
		wantCode connect.Code
	}{
		{"bad uuid", "nope", "sonnet", connect.CodeInvalidArgument},
		{"bad model", analysisTestSID, "--invalid-model", connect.CodeInvalidArgument},
		{"missing transcript", "33333333-3333-4333-8333-333333333333", "sonnet", connect.CodeNotFound},
		{"missing index", notIndexed, "sonnet", connect.CodeNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := client.StartAnalysis(ctx, connect.NewRequest(&claudeobsv1.StartAnalysisRequest{Id: c.id, Model: c.model}))
			if err == nil {
				t.Fatal("want an error")
			}
			if connect.CodeOf(err) != c.wantCode {
				t.Errorf("code = %v, want %v", connect.CodeOf(err), c.wantCode)
			}
		})
	}
	if r.Busy() {
		t.Error("Busy() = true after only rejected starts")
	}
}

func TestStartAdviceValidationCodes(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := e.runner(e.fakeScript(fakeClaudeOK), 30*time.Second)
	o := e.options(r)
	vmC, vlC, cleanup := failingStore(t)
	defer cleanup()
	o.VM, o.VL = vmC, vlC
	client := newAnalysisClient(t, o)
	ctx := context.Background()

	_, err := client.StartAdvice(ctx, connect.NewRequest(&claudeobsv1.StartAdviceRequest{Model: "--invalid-model", Depth: "standard"}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad model: code = %v, want CodeInvalidArgument", connect.CodeOf(err))
	}
	_, err = client.StartAdvice(ctx, connect.NewRequest(&claudeobsv1.StartAdviceRequest{Model: "sonnet", Depth: "deep"}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad depth: code = %v, want CodeInvalidArgument", connect.CodeOf(err))
	}
	if r.Busy() {
		t.Error("Busy() = true after only rejected starts")
	}
}

/* --- 409 mapping for both kinds ------------------------------------------ */

func TestBusyMapsToAlreadyExistsForBothKinds(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := e.runner(e.fakeScript(fakeClaudeSlow), 30*time.Second)
	o := e.options(r)
	vmC, vlC, cleanup := failingStore(t)
	defer cleanup()
	o.VM, o.VL = vmC, vlC
	client := newAnalysisClient(t, o)
	ctx := context.Background()

	if _, err := client.StartAnalysis(ctx, connect.NewRequest(&claudeobsv1.StartAnalysisRequest{Id: analysisTestSID, Model: "sonnet"})); err != nil {
		t.Fatalf("first StartAnalysis: %v", err)
	}

	_, err := client.StartAnalysis(ctx, connect.NewRequest(&claudeobsv1.StartAnalysisRequest{Id: analysisTestSID, Model: "sonnet"}))
	if err == nil || connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("second StartAnalysis: code = %v, want CodeAlreadyExists", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "another analysis is already running") {
		t.Errorf("message = %q, want it to contain %q", err.Error(), "another analysis is already running")
	}

	_, err = client.StartAdvice(ctx, connect.NewRequest(&claudeobsv1.StartAdviceRequest{Model: "sonnet", Depth: "standard"}))
	if err == nil || connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("StartAdvice while analyse busy: code = %v, want CodeAlreadyExists", connect.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "another run is already in progress") {
		t.Errorf("message = %q, want it to contain %q", err.Error(), "another run is already in progress")
	}

	r.Wait()
}

/* --- GetAnalysis ---------------------------------------------------------- */

func TestSavedHookFailuresAreExposedAsErrorsWithoutEchoedPrompts(t *testing.T) {
	for _, kind := range []string{"analysis", "advice"} {
		t.Run(kind, func(t *testing.T) {
			e := newAnalysisTestEnv(t)
			r := e.runner(e.fakeScript(fakeClaudeOK), 30*time.Second)
			key, dir := analysisTestSID, filepath.Join(e.dataDir, "analyses", analysisTestSID)
			if kind == "advice" {
				key, dir = "advice", filepath.Join(e.dataDir, "advice")
			}
			if err := db.PutAnalysisRun(e.db, db.AnalysisRun{Kind: kind, Key: key, State: "done"}); err != nil {
				t.Fatal(err)
			}
			writeReportFile(t, dir, "report.md", "UserPromptSubmit operation blocked by hook:\n[guard]: wrong workspace.\n\nOriginal prompt: PRIVATE")
			client := newAnalysisClient(t, e.options(r))
			var run *claudeobsv1.AnalysisRun
			var markdown string
			if kind == "analysis" {
				resp, err := client.GetAnalysis(context.Background(), connect.NewRequest(&claudeobsv1.GetAnalysisRequest{Id: analysisTestSID}))
				if err != nil {
					t.Fatal(err)
				}
				run, markdown = resp.Msg.Run, resp.Msg.ReportMd
			} else {
				resp, err := client.GetAdvice(context.Background(), connect.NewRequest(&claudeobsv1.GetAdviceRequest{}))
				if err != nil {
					t.Fatal(err)
				}
				run, markdown = resp.Msg.Run, resp.Msg.ReportMd
			}
			if run.State != "error" || !strings.Contains(run.Error, "wrong workspace") || strings.Contains(run.Error, "PRIVATE") || markdown != "" {
				t.Fatalf("saved failure misclassified or echoed prompt exposed: %+v, %q", run, markdown)
			}
		})
	}
}

func TestGetAnalysisBadUUID(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := e.runner(e.fakeScript(fakeClaudeOK), 30*time.Second)
	client := newAnalysisClient(t, e.options(r))

	_, err := client.GetAnalysis(context.Background(), connect.NewRequest(&claudeobsv1.GetAnalysisRequest{Id: "nope"}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want CodeInvalidArgument", connect.CodeOf(err))
	}
}

func TestGetAnalysisNoRecordIsNotFound(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := e.runner(e.fakeScript(fakeClaudeOK), 30*time.Second)
	client := newAnalysisClient(t, e.options(r))

	_, err := client.GetAnalysis(context.Background(), connect.NewRequest(&claudeobsv1.GetAnalysisRequest{Id: analysisTestSID}))
	if err == nil || connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want CodeNotFound", connect.CodeOf(err))
	}
}

func TestGetAnalysisAfterHappyRunReturnsReportAndTimeout(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := e.runner(e.fakeScript(fakeClaudeOK), 30*time.Second)
	o := e.options(r)
	o.AnalysisTimeout = 30 * time.Second
	client := newAnalysisClient(t, o)
	ctx := context.Background()

	if _, err := client.StartAnalysis(ctx, connect.NewRequest(&claudeobsv1.StartAnalysisRequest{Id: analysisTestSID, Model: "haiku"})); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	resp, err := client.GetAnalysis(ctx, connect.NewRequest(&claudeobsv1.GetAnalysisRequest{Id: analysisTestSID}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.Run == nil || resp.Msg.Run.State != "done" {
		t.Fatalf("run = %+v, want state done", resp.Msg.Run)
	}
	if resp.Msg.Run.Model != "haiku" {
		t.Errorf("run.Model = %q, want haiku", resp.Msg.Run.Model)
	}
	if !strings.Contains(resp.Msg.ReportMd, "# Report") {
		t.Errorf("ReportMd = %q, want it to contain %q", resp.Msg.ReportMd, "# Report")
	}
	if resp.Msg.TimeoutS != 30 {
		t.Errorf("TimeoutS = %d, want 30", resp.Msg.TimeoutS)
	}
	if resp.Msg.Report != nil {
		t.Errorf("Report = %+v, want nil: fakeClaudeOK prints markdown, which is the fallback path", resp.Msg.Report)
	}
}

// The structured path, driven through a real run rather than a hand-written
// file, so the write side and the read side are held to one filename.
func TestGetAnalysisServesStructuredReport(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := e.runner(e.fakeScript(fakeClaudeJSON), 30*time.Second)
	client := newAnalysisClient(t, e.options(r))
	ctx := context.Background()

	if _, err := client.StartAnalysis(ctx, connect.NewRequest(&claudeobsv1.StartAnalysisRequest{
		Id: analysisTestSID, Model: "sonnet",
	})); err != nil {
		t.Fatalf("StartAnalysis: %v", err)
	}
	r.Wait()

	resp, err := client.GetAnalysis(ctx, connect.NewRequest(&claudeobsv1.GetAnalysisRequest{Id: analysisTestSID}))
	if err != nil {
		t.Fatalf("GetAnalysis: %v", err)
	}
	if resp.Msg.Report == nil || resp.Msg.Report.Headline != "H" {
		t.Fatalf("report = %+v, want the structured report", resp.Msg.Report)
	}
	if resp.Msg.Report.Verdict != "mixed" {
		t.Errorf("verdict = %q, want mixed", resp.Msg.Report.Verdict)
	}
	if resp.Msg.ReportMd != "" {
		t.Errorf("report_md = %q, want empty beside a structured report", resp.Msg.ReportMd)
	}
}

/* --- GetAdvice ------------------------------------------------------------ */

func TestGetAdviceTimeoutDependsOnDepth(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := analysis.New(analysis.Config{
		ClaudeBin:       e.fakeScript(fakeClaudeOK),
		ProfileDir:      e.profileDir,
		DataDir:         e.dataDir,
		DB:              e.db,
		VLURL:           "http://127.0.0.1:1",
		AnalysisTimeout: 2 * time.Second,
		EvidenceTimeout: 9 * time.Second,
	})
	o := e.options(r)
	o.AnalysisTimeout = 2 * time.Second
	o.EvidenceTimeout = 9 * time.Second
	vmC, vlC, cleanup := failingStore(t)
	defer cleanup()
	o.VM, o.VL = vmC, vlC
	client := newAnalysisClient(t, o)
	ctx := context.Background()

	if _, err := client.StartAdvice(ctx, connect.NewRequest(&claudeobsv1.StartAdviceRequest{Model: "haiku", Depth: "standard"})); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	resp, err := client.GetAdvice(ctx, connect.NewRequest(&claudeobsv1.GetAdviceRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.TimeoutS != 2 {
		t.Errorf("standard depth TimeoutS = %d, want 2", resp.Msg.TimeoutS)
	}

	if _, err := client.StartAdvice(ctx, connect.NewRequest(&claudeobsv1.StartAdviceRequest{Model: "haiku", Depth: "evidence"})); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	resp, err = client.GetAdvice(ctx, connect.NewRequest(&claudeobsv1.GetAdviceRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.TimeoutS != 9 {
		t.Errorf("evidence depth TimeoutS = %d, want 9", resp.Msg.TimeoutS)
	}
}

func TestGetAdviceNoRunIsNotFound(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := e.runner(e.fakeScript(fakeClaudeOK), 30*time.Second)
	client := newAnalysisClient(t, e.options(r))

	_, err := client.GetAdvice(context.Background(), connect.NewRequest(&claudeobsv1.GetAdviceRequest{}))
	if err == nil || connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want CodeNotFound", connect.CodeOf(err))
	}
}

/* --- runner busyness ------------------------------------------------------ */

func TestGetAnalysisReportsRunnerBusy(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := e.runner(e.fakeScript(fakeClaudeSlow), 30*time.Second)
	client := newAnalysisClient(t, e.options(r))
	ctx := context.Background()

	if _, err := client.StartAnalysis(ctx, connect.NewRequest(&claudeobsv1.StartAnalysisRequest{Id: analysisTestSID, Model: "sonnet"})); err != nil {
		t.Fatal(err)
	}

	resp, err := client.GetAnalysis(ctx, connect.NewRequest(&claudeobsv1.GetAnalysisRequest{Id: analysisTestSID}))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Msg.RunnerBusy {
		t.Error("RunnerBusy = false while the slow fake is still running, want true")
	}

	r.Wait()

	resp, err = client.GetAnalysis(ctx, connect.NewRequest(&claudeobsv1.GetAnalysisRequest{Id: analysisTestSID}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.RunnerBusy {
		t.Error("RunnerBusy = true after Wait, want false")
	}
}

func TestGetAdviceReportsRunnerBusy(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := e.runner(e.fakeScript(fakeClaudeSlow), 30*time.Second)
	o := e.options(r)
	vmC, vlC, cleanup := failingStore(t)
	defer cleanup()
	o.VM, o.VL = vmC, vlC
	client := newAnalysisClient(t, o)
	ctx := context.Background()

	if _, err := client.StartAdvice(ctx, connect.NewRequest(&claudeobsv1.StartAdviceRequest{Model: "sonnet", Depth: "standard"})); err != nil {
		t.Fatal(err)
	}

	resp, err := client.GetAdvice(ctx, connect.NewRequest(&claudeobsv1.GetAdviceRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Msg.RunnerBusy {
		t.Error("RunnerBusy = false while the slow fake is still running, want true")
	}

	r.Wait()

	resp, err = client.GetAdvice(ctx, connect.NewRequest(&claudeobsv1.GetAdviceRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.RunnerBusy {
		t.Error("RunnerBusy = true after Wait, want false")
	}
}

// TestRunnerBusyCrossKind pins the property TestGetAnalysisReportsRunnerBusy
// and TestGetAdviceReportsRunnerBusy don't: the field reports the Runner's
// single shared lock (analysis.Runner.Busy(), one bool covering both run
// kinds), not two independent per-kind flags. Without this, a future
// refactor that split the shared bool into an analysis-busy flag and an
// advice-busy flag would still pass both of those same-kind tests while
// silently breaking the contract this field exists for: greying out both
// Start buttons whenever ANY run, of either kind, is in flight.
//
// Each Get RPC only answers once a status row exists for it (a fresh
// GetAdvice with no advice ever started, or GetAnalysis for an id never
// analysed, is CodeNotFound), so both rows are established first, and the
// slow fake plus Wait() drains each run before starting the next one.
func TestRunnerBusyCrossKind(t *testing.T) {
	e := newAnalysisTestEnv(t)
	r := e.runner(e.fakeScript(fakeClaudeSlow), 30*time.Second)
	o := e.options(r)
	vmC, vlC, cleanup := failingStore(t)
	defer cleanup()
	o.VM, o.VL = vmC, vlC
	client := newAnalysisClient(t, o)
	ctx := context.Background()

	// Establish an analysis row (ends up "done") so GetAnalysis has
	// something to read back once an advice run is the one in flight.
	if _, err := client.StartAnalysis(ctx, connect.NewRequest(&claudeobsv1.StartAnalysisRequest{Id: analysisTestSID, Model: "sonnet"})); err != nil {
		t.Fatal(err)
	}
	r.Wait()

	// A running ADVICE run must make GetAnalysis (the OTHER kind, reading
	// back the already-done row above) report busy.
	if _, err := client.StartAdvice(ctx, connect.NewRequest(&claudeobsv1.StartAdviceRequest{Model: "sonnet", Depth: "standard"})); err != nil {
		t.Fatal(err)
	}
	analysisResp, err := client.GetAnalysis(ctx, connect.NewRequest(&claudeobsv1.GetAnalysisRequest{Id: analysisTestSID}))
	if err != nil {
		t.Fatal(err)
	}
	if !analysisResp.Msg.RunnerBusy {
		t.Error("GetAnalysis.RunnerBusy = false while an advice run is in flight, want true (shared lock covers both kinds)")
	}
	r.Wait()

	// A running ANALYSIS run must make GetAdvice (the OTHER kind, now
	// backed by the done row the StartAdvice above left behind) report
	// busy.
	if _, err := client.StartAnalysis(ctx, connect.NewRequest(&claudeobsv1.StartAnalysisRequest{Id: analysisTestSID, Model: "sonnet"})); err != nil {
		t.Fatal(err)
	}
	adviceResp, err := client.GetAdvice(ctx, connect.NewRequest(&claudeobsv1.GetAdviceRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !adviceResp.Msg.RunnerBusy {
		t.Error("GetAdvice.RunnerBusy = false while an analysis run is in flight, want true (shared lock covers both kinds)")
	}
	r.Wait()
}

// TestRunnerBusyNilRunner exercises the package-level runnerBusy helper
// directly rather than through an RPC: GetAnalysis/GetAdvice both answer
// CodeInternal before ever reaching this line when Options.Runner is nil
// (errRunnerNotConfigured, see analysis.go), so there is no response to
// assert on at the RPC level. The helper's own nil handling is what keeps
// RunnerBusy's meaning coherent independent of that guard.
func TestRunnerBusyNilRunner(t *testing.T) {
	if runnerBusy(nil) {
		t.Error("runnerBusy(nil) = true, want false")
	}
}

/* --- findings assembly failure degrades to [] ---------------------------- */

func TestStartAdviceFindingsAssemblyFailureDegradesToEmptyList(t *testing.T) {
	e := newAnalysisTestEnv(t)
	bin, argvPath := e.fakeRecorder()
	r := e.runner(bin, 30*time.Second)
	o := e.options(r)
	vmC, vlC, cleanup := failingStore(t)
	defer cleanup()
	// o.DB is nil'd here (rather than left as e.db, options()'s own default)
	// because e.db is a real, freshly migrated but empty sessions table:
	// listIndexedSessions succeeds trivially on that (zero rows is not an
	// error), which would leave IndexOk true and defeat this test's point.
	// Nil reproduces "no index at all", matching
	// TestBuildFindingsErrorsWhenAllSourcesFail's own construction; the
	// Runner itself keeps using e.db internally (set at construction in
	// e.runner, untouched here) for its own status.json/db writes, so state
	// still reads "done" below.
	o.VM, o.VL, o.DB = vmC, vlC, nil // all three sources fail: buildFindings
	// has nothing left to compute.
	client := newAnalysisClient(t, o)

	if _, err := client.StartAdvice(context.Background(), connect.NewRequest(&claudeobsv1.StartAdviceRequest{Model: "haiku", Depth: "standard"})); err != nil {
		t.Fatalf("StartAdvice: %v", err)
	}
	r.Wait()

	data, err := os.ReadFile(argvPath + ".stdin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\n[]\n") {
		t.Errorf("prompt did not carry the degraded findings []; argv = %q", data)
	}

	statusPath := filepath.Join(e.dataDir, "advice", "status.json")
	fsData, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	var fs struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(fsData, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.State != "done" {
		t.Errorf("state = %q, want done (a findings-assembly failure must not block the run)", fs.State)
	}
}
