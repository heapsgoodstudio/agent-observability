// Package analysis is a headless `claude -p` runner that either analyses one
// session's transcript (analyse) or advises across the whole corpus (advise)
// at a chosen depth. Only one run, of either kind, is ever in flight at a
// time.
//
// A spawned run is trusted, not sandboxed: it executes with the
// instrumented profile's own permissions, and only the tools named in
// --disallowedTools are actually refused. --allowedTools is an allow rule,
// not a restriction, so --disallowedTools is where the real tier boundaries
// live (see AdviceAllowedTools/AdviceDisallowedTools below).
//
// Every status transition dual-writes: a status.json beside the run's report,
// and a full-row upsert into sqlite via internal/db's PutAnalysisRun. DB may
// be nil, in which case a Runner is file-only, so AnalysisStatus/AdviceStatus
// read the db first and fall back to parsing the file when no row exists
// there.
//
// The report itself lands as report.json (the structured document
// internal/report validates) or, when the run's output will not parse as one,
// as report.md holding that output verbatim. Exactly one of the two exists per
// run; see writeReport.
package analysis

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/heapsgoodstudio/agent-observability/internal/localhistory"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/report"
	"github.com/heapsgoodstudio/agent-observability/internal/usage"
)

// promptFS embeds the prompt frames. prompts/*.md is the only copy: they are
// compiled into the binary rather than read off disk, so a run cannot pick up
// a half-edited frame.
//
//go:embed prompts/*.md
var promptFS embed.FS

// Models and Depths are the accepted values for a run's two knobs.
var (
	Models = map[string]bool{"sonnet": true, "opus": true, "haiku": true}
	Depths = map[string]bool{"shallow": true, "standard": true, "evidence": true}
)

// AnalysisDisallowedTools is ANALYSIS_DISALLOWED_TOOLS verbatim: the analyse
// path reads one transcript at a known path, so it is denied a shell along
// with the mutating tools.
const AnalysisDisallowedTools = "Write,Edit,NotebookEdit,Bash"

// AdviceAllowedTools and AdviceDisallowedTools mirror ADVICE_ALLOWED_TOOLS /
// ADVICE_DISALLOWED_TOOLS verbatim. Bash is allowed at every depth (the
// corpus survey needs a shell); Agent is granted only at evidence depth,
// since shallow and standard are single-pass by definition and a fan-out
// there would multiply the bill rather than being a stylistic lapse.
var (
	AdviceAllowedTools = map[string]string{
		"shallow":  "Read,Grep,Glob,Bash",
		"standard": "Read,Grep,Glob,Bash",
		"evidence": "Read,Grep,Glob,Bash,Agent",
	}
	AdviceDisallowedTools = map[string]string{
		"shallow":  "Write,Edit,NotebookEdit,Agent",
		"standard": "Write,Edit,NotebookEdit,Agent",
		"evidence": "Write,Edit,NotebookEdit",
	}
)

// MaxFindingsBytes is MAX_FINDINGS_BYTES verbatim (64 * 1024).
const MaxFindingsBytes = 65536

// DefaultAnalysisTimeout and DefaultEvidenceTimeout are TIMEOUT_S and
// EVIDENCE_TIMEOUT_S's defaults verbatim: one measured standard-depth advice
// run took 537s; 1500s leaves close to triple that. Evidence fans out to
// per-session subagents, so it gets the longer budget.
const (
	DefaultAnalysisTimeout = 1500 * time.Second
	DefaultEvidenceTimeout = 2700 * time.Second
)

// uuidRE is UUID_RE verbatim: a lowercase, hyphenated UUID.
var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Sentinel errors the RPC layer maps to its own status codes.
var (
	ErrBusy             = fmt.Errorf("another run is already in progress")
	ErrBadModel         = fmt.Errorf("model must be a valid model identifier (or blank for the profile default)")
	ErrBadDepth         = fmt.Errorf("depth must be one of shallow, standard, evidence")
	ErrBadSessionID     = fmt.Errorf("session_id must be an indexed session identifier")
	ErrNoTranscript     = fmt.Errorf("transcript not on disk (aged out?)")
	ErrNotIndexed       = fmt.Errorf("session not in the index yet")
	ErrFindingsTooLarge = fmt.Errorf("findings payload too large")
)

// FlightGate shares the single active analysis/advice slot across profile
// runners. Its zero value is ready for use. Share one gate for every runner
// that writes the same dashboard data directory.
type FlightGate struct {
	mu   sync.Mutex
	busy bool
}

// Config configures one Runner.
type Config struct {
	Provider, ProfileID, Binary                                string
	SourceProvider, SourceProfile, SourceDir, SourceAccountKey string
	SourceProfiles                                             []string
	Sources                                                    map[string]string // configured profile ID -> directory

	// FlightGate is shared by profile runners and the server status reader.
	// Nil gives a standalone runner its own gate.
	FlightGate *FlightGate
	ClaudeBin  string // overrides the claude binary; tests point this at a fake script
	ProfileDir string // CLAUDE_CONFIG_DIR; <ProfileDir>/projects/*/<sid>.jsonl holds transcripts
	DataDir    string // <install-dir>/dashboard/data: analyses/ and advice/ reports live here

	// WorkDir is advice's working directory and the fallback for session
	// analysis without a surviving recorded cwd. Session analysis prefers its
	// own transcript's workspace so profile hooks see the correct repository.
	// Without a configured or recorded directory
	// the child inherits whatever
	// cwd launched this binary (launchd's default, in production), not the
	// project checkout: its own telemetry attributes the run to the wrong
	// project, the project CLAUDE.md and .claude/settings.local.json stop
	// loading, and an advice run's Bash-resolved relative paths resolve from
	// the wrong place. Empty means unset: cmd.Dir is left zero, so the
	// child inherits this process's own cwd, same as not setting it at all.
	WorkDir string

	// DB is state/claude-obs.db. Nil means file-only: the db half of each
	// status dual-write is skipped, and AnalysisStatus/AdviceStatus read the
	// status.json instead.
	DB *sql.DB

	// VLURL is VictoriaLogs' base URL, used only to count an advice run's
	// subagents after it finishes.
	VLURL string

	// AnalysisTimeout governs the analyse path and advice's shallow/standard
	// depths; EvidenceTimeout governs advice's evidence depth alone. Zero
	// means DefaultAnalysisTimeout/DefaultEvidenceTimeout.
	AnalysisTimeout time.Duration
	EvidenceTimeout time.Duration

	// Now returns the current Unix time in seconds; nil means time.Now().
	// Unix(), which is real behaviour. Tests can pin it for deterministic
	// started_at/finished_at values.
	Now func() int64
}

// Runner spawns and tracks at most one headless `claude -p` run at a time,
// of either kind (analyse or advise).
type Runner struct {
	cfg Config
	wg  sync.WaitGroup
}

// New builds a Runner from cfg, filling in zero-value defaults.
func New(cfg Config) *Runner {
	if cfg.FlightGate == nil {
		cfg.FlightGate = &FlightGate{}
	}
	if cfg.AnalysisTimeout == 0 {
		cfg.AnalysisTimeout = DefaultAnalysisTimeout
	}
	if cfg.EvidenceTimeout == 0 {
		cfg.EvidenceTimeout = DefaultEvidenceTimeout
	}
	if cfg.Now == nil {
		cfg.Now = func() int64 { return time.Now().Unix() }
	}
	return &Runner{cfg: cfg}
}

// Busy reports whether a run is currently in flight, of either kind.
func (r *Runner) Busy() bool {
	r.cfg.FlightGate.mu.Lock()
	defer r.cfg.FlightGate.mu.Unlock()
	return r.cfg.FlightGate.busy
}

// Wait blocks until the in-flight run (if any) finishes. Test hook: lets
// tests avoid sleep-polling a background goroutine.
func (r *Runner) Wait() {
	r.wg.Wait()
}

// acquire claims the shared busy lock for a new run, or reports ErrBusy.
func (r *Runner) acquire() error {
	r.cfg.FlightGate.mu.Lock()
	defer r.cfg.FlightGate.mu.Unlock()
	if r.cfg.FlightGate.busy {
		return ErrBusy
	}
	r.cfg.FlightGate.busy = true
	return nil
}

// release frees the shared busy lock. Deferred ahead of wg.Done in both
// spawn goroutines below, so by the time Wait() returns, Busy() already
// reports false: Go defers run LIFO, so registering release after wg.Add's
// matching Done ensures release fires first.
func (r *Runner) release() {
	r.cfg.FlightGate.mu.Lock()
	r.cfg.FlightGate.busy = false
	r.cfg.FlightGate.mu.Unlock()
}

// --- orphaned runs ---------------------------------------------------------

// orphanError is the status a reaped run ends in. It names the cause it can
// actually establish (something ended the process) rather than guessing
// between a restart, a crash and a kill, since the status carries no evidence
// to tell them apart.
const orphanError = "interrupted by a server restart or crash: the run was in flight when the process ended, so it never finished"

// ReapOrphanedRuns marks every run still recorded as "running" as failed, and
// returns how many it repaired.
//
// Call this once at startup, before serving. A run exists only as a goroutine
// inside one `claude-obs serve` process, and the terminal status is written by
// that goroutine, so a process that dies mid-run leaves the status at
// "running" with nothing alive to finish it. A freshly started process owns no
// runs by definition, so every "running" status it finds is dead, and it knows
// that with certainty rather than inferring it from elapsed time.
//
// Without this, the only thing that ever notices is the frontend's isStuck,
// which waits out the full timeout plus a 300s grace before it will even offer
// a retry. Measured on 2026-08-17: a real run killed by a server restart at 7
// minutes left "analysing" on the page for 30 minutes, with the runner idle the
// whole time.
//
// Statuses are found from the file layout, which is authoritative because
// dualStatus writes the file before the db row, and from the db as well when
// one is configured, so a row whose file has been removed is still repaired.
// Each repair goes through dualStatus like any other transition, so the file
// and the db cannot come out of this disagreeing.
func (r *Runner) ReapOrphanedRuns() (int, error) {
	type target struct{ kind, key, path string }
	seen := map[string]bool{}
	var targets []target
	add := func(kind, key, path string) {
		k := kind + "/" + key
		if seen[k] {
			return
		}
		seen[k] = true
		targets = append(targets, target{kind, key, path})
	}

	advicePath := filepath.Join(r.cfg.DataDir, "advice", "status.json")
	if statusIsRunning(advicePath) {
		add("advice", "advice", advicePath)
	}
	scopedPaths, err := filepath.Glob(filepath.Join(r.cfg.DataDir, "advice", "scopes", "*", "status.json"))
	if err != nil {
		return 0, err
	}
	for _, path := range scopedPaths {
		key := filepath.Base(filepath.Dir(path))
		if adviceDirForKey(r.cfg.DataDir, key) != "" && statusIsRunning(path) {
			add("advice", key, path)
		}
	}
	matches, err := filepath.Glob(filepath.Join(r.cfg.DataDir, "analyses", "*", "status.json"))
	if err != nil {
		return 0, fmt.Errorf("analysis: scan for orphaned runs: %w", err)
	}
	sort.Strings(matches)
	for _, path := range matches {
		if statusIsRunning(path) {
			add("analysis", filepath.Base(filepath.Dir(path)), path)
		}
	}

	if r.cfg.DB != nil {
		rows, err := db.RunningAnalysisRuns(r.cfg.DB)
		if err != nil {
			return 0, err
		}
		for _, row := range rows {
			if row.Kind == "advice" {
				dir := adviceDirForKey(r.cfg.DataDir, row.Key)
				if dir != "" {
					add(row.Kind, row.Key, filepath.Join(dir, "status.json"))
				}
				continue
			}
			add(row.Kind, row.Key, filepath.Join(r.cfg.DataDir, "analyses", row.Key, "status.json"))
		}
	}

	n := 0
	var firstErr error
	for _, t := range targets {
		err := r.dualStatus(t.kind, t.key, t.path, map[string]any{
			"state": "error", "finished_at": r.cfg.Now(), "error": orphanError,
		})
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("analysis: reap %s/%s: %w", t.kind, t.key, err)
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// statusIsRunning reports whether the status.json at path records a run still
// in flight. An unreadable or malformed file is not running: this runs at
// startup, where refusing to serve over a corrupt status file would be a worse
// failure than ignoring it.
func statusIsRunning(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var st struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return false
	}
	return st.State == "running"
}

// --- analyse -----------------------------------------------------------

// StartAnalysis validates sid/model, claims the shared lock, writes the
// initial "running" status (file + db), and spawns the analyse run in the
// background. It mirrors do_POST's /analyse branch plus run_analysis's
// entry, in that order: full validation happens before the busy check.
func (r *Runner) StartAnalysis(sid, model string) error {
	if !ValidSessionID(sid) {
		return ErrBadSessionID
	}
	if !ValidModel(model) {
		return ErrBadModel
	}
	transcript, ok := r.findTranscript(sid)
	if !ok {
		return ErrNoTranscript
	}
	facts, err := r.loadSessionFacts(sid)
	if err != nil {
		return err
	}

	if err := r.acquire(); err != nil {
		return err
	}

	statusPath := filepath.Join(r.cfg.DataDir, "analyses", sid, "status.json")
	if err := r.dualStatus("analysis", sid, statusPath, map[string]any{
		"state": "running", "model": model,
		"provider": r.provider(), "profile": r.cfg.ProfileID, "source_provider": r.cfg.SourceProvider, "source_profile": r.cfg.SourceProfile, "source_account_key": r.cfg.SourceAccountKey,
		"started_at": r.cfg.Now(), "finished_at": nil, "error": nil,
	}); err != nil {
		r.release()
		return fmt.Errorf("analysis: start dispatch: %w", err)
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.release()
		r.runAnalysis(sid, model, transcript, facts, statusPath)
	}()
	return nil
}

// AnalysisStatus reads sid's status, preferring the db and falling back to
// the status.json when no row exists (a file-only Runner).
func (r *Runner) AnalysisStatus(sid string) (*db.AnalysisRun, bool, error) {
	return r.status("analysis", sid, filepath.Join(r.cfg.DataDir, "analyses", sid, "status.json"))
}

// findTranscript mirrors find_transcript: sorted glob, first hit wins.
// analysisPrompt fills the analyse frame. The shared report contract goes in
// first, then the frame's own placeholders, so a contract that itself mentions
// a placeholder still resolves. Every placeholder either frame can carry is
// substituted here: one left unfilled reaches the model as literal braces, and
// the run still costs money.
func analysisPrompt(sid, transcript, factsJSON, headline string) (string, error) {
	frame, err := promptFS.ReadFile("prompts/analysis-prompt.md")
	if err != nil {
		return "", err
	}
	contract, err := promptFS.ReadFile("prompts/report-contract.md")
	if err != nil {
		return "", err
	}
	p := strings.ReplaceAll(string(frame), "{REPORT_CONTRACT}", string(contract))
	p = strings.ReplaceAll(p, "{SESSION_ID}", sid)
	p = strings.ReplaceAll(p, "{TRANSCRIPT_PATH}", transcript)
	p = strings.ReplaceAll(p, "{HEADLINE}", headline)
	p = strings.ReplaceAll(p, "{SESSION_FACTS_JSON}", factsJSON)
	return p, nil
}

// advicePrompt fills the advise frame at one depth, in the same order: the
// depth brief and the report contract first, then the frame's own
// placeholders.
func advicePrompt(depth string, findingsJSON []byte, indexPath, profileDir, sessionsDir string) (string, error) {
	briefName, ok := map[string]string{
		"shallow":  "advice-depth-shallow.md",
		"standard": "advice-depth-standard.md",
		"evidence": "advice-depth-evidence.md",
	}[depth]
	if !ok {
		return "", fmt.Errorf("unknown depth %q", depth)
	}
	brief, err := promptFS.ReadFile("prompts/" + briefName)
	if err != nil {
		return "", err
	}
	contract, err := promptFS.ReadFile("prompts/report-contract.md")
	if err != nil {
		return "", err
	}
	frame, err := promptFS.ReadFile("prompts/advice-prompt.md")
	if err != nil {
		return "", err
	}
	p := strings.ReplaceAll(string(frame), "{DEPTH_BRIEF}", string(brief))
	p = strings.ReplaceAll(p, "{REPORT_CONTRACT}", string(contract))
	p = strings.ReplaceAll(p, "{FINDINGS_JSON}", indentFindings(findingsJSON))
	p = strings.ReplaceAll(p, "{INDEX_PATH}", indexPath)
	p = strings.ReplaceAll(p, "{PROFILE_DIR}", profileDir)
	p = strings.ReplaceAll(p, "{SESSIONS_DIR}", sessionsDir)
	return p, nil
}

// runAnalysis builds the prompt, spawns claude, and dual-writes whatever
// status the run ends in. Every early return short-circuits straight to an
// error status: no failure leaves the run without a terminal status.
func (r *Runner) runAnalysis(sid, model, transcript string, facts sessionFacts, statusPath string) {
	fail := func(msg string) {
		r.dualStatus("analysis", sid, statusPath, map[string]any{
			"state": "error", "finished_at": r.cfg.Now(), "error": lastRunes(msg, 500),
		})
	}

	if r.cfg.DB != nil {
		if source, ok, err := db.GetSessionSource(r.cfg.DB, sid); err != nil {
			fail(err.Error())
			return
		} else if ok && source.Format == "opencode" {
			row, _, err := db.GetSession(r.cfg.DB, sid)
			if err != nil {
				fail(err.Error())
				return
			}
			transcript = filepath.Join(filepath.Dir(statusPath), "evidence.jsonl")
			if err := localhistory.ExportOpenCode(context.Background(), source.Path, row.NativeID, transcript); err != nil {
				fail(err.Error())
				return
			}
			defer os.Remove(transcript)
		}
	}
	factsData, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		fail(err.Error())
		return
	}
	headline := fmt.Sprintf("session %s, project %s, %d requests",
		sid, facts.Project, facts.Requests)

	prompt, err := analysisPrompt(sid, transcript, string(factsData), headline)
	if err != nil {
		fail(err.Error())
		return
	}

	args := []string{
		"-p", prompt, "--model", model,
		"--allowedTools", "Read,Grep,Glob",
		"--disallowedTools", AnalysisDisallowedTools,
	}
	workDir, err := r.analysisWorkDir(transcript)
	if r.cfg.DB != nil {
		if source, ok, _ := db.GetSessionSource(r.cfg.DB, sid); ok && filepath.IsAbs(source.Cwd) {
			if st, e := os.Stat(source.Cwd); e == nil && st.IsDir() {
				workDir = source.Cwd
				err = nil
			}
		}
	}
	if err != nil {
		fail(err.Error())
		return
	}
	out, errOut, runErr, timedOut, spawnErr := r.spawnInDir(args, r.cfg.AnalysisTimeout, workDir)
	if spawnErr != nil {
		fail(spawnErr.Error())
		return
	}
	if timedOut {
		r.dualStatus("analysis", sid, statusPath, map[string]any{
			"state": "error", "finished_at": r.cfg.Now(),
			"error": fmt.Sprintf("timed out after %ds", int(r.cfg.AnalysisTimeout.Seconds())),
		})
		return
	}
	if runErr == nil && strings.TrimSpace(out) != "" {
		if err := writeReport(filepath.Join(r.cfg.DataDir, "analyses", sid), out); err != nil {
			fail(err.Error())
			return
		}
		if err := r.dualStatus("analysis", sid, statusPath, map[string]any{
			"state": "done", "finished_at": r.cfg.Now(), "error": nil,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "analysis: final status write failed for %s: %v\n", sid, err)
		}
		return
	}
	if err := r.dualStatus("analysis", sid, statusPath, map[string]any{
		"state": "error", "finished_at": r.cfg.Now(), "error": errorTail(errOut),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "analysis: final status write failed for %s: %v\n", sid, err)
	}
}

// --- advise --------------------------------------------------------------

// StartAdvice validates model/depth/findings size, claims the shared lock,
// generates this run's session id, writes the initial "running" status
// (file + db, with session_id already set and subagents cleared to null),
// and spawns the advice run in the background. The session id is generated
// here rather than inside the background goroutine, and returned directly,
// so a caller can key telemetry by it immediately.
func (r *Runner) StartAdvice(model, depth string, findingsJSON []byte) (string, error) {
	return r.StartScopedAdvice(model, depth, findingsJSON, AdviceScope{Configurations: []EvidenceProfile{{Directory: r.cfg.ProfileDir}}}, "")
}

func (r *Runner) StartScopedAdvice(model, depth string, findingsJSON []byte, scope AdviceScope, executionProfile string) (string, error) {
	if !ValidModel(model) {
		return "", ErrBadModel
	}
	if depth == "" {
		depth = "standard"
	}
	if !Depths[depth] {
		return "", ErrBadDepth
	}
	if len(findingsJSON) > MaxFindingsBytes {
		return "", ErrFindingsTooLarge
	}
	if (scope.AccountKey != "" || scope.Profile != "") && scope.Profiles == nil {
		return "", fmt.Errorf("scoped advice requires an explicit profile selection")
	}
	if scope.Profile != "" && (len(scope.Profiles) != 1 || scope.Profiles[0] != scope.Profile) {
		return "", fmt.Errorf("advice profile and evidence selection do not match")
	}

	if err := r.acquire(); err != nil {
		return "", err
	}

	evidenceDir, err := r.buildAdviceEvidence(scope, depth)
	if err != nil {
		r.release()
		return "", err
	}
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		os.RemoveAll(evidenceDir)
		r.release()
		return "", err
	}
	sessionID := uuid.NewString()
	key := scope.Key()
	statusPath := filepath.Join(AdviceReportDir(r.cfg.DataDir, scope), "status.json")
	if err := r.dualStatus("advice", key, statusPath, map[string]any{
		"scope_json": string(scopeJSON), "execution_profile": executionProfile,
		"provider": r.provider(), "profile": r.cfg.ProfileID, "source_provider": scope.Provider, "source_profile": scope.Profile, "source_account_key": scope.AccountKey,
		"state": "running", "model": model, "depth": depth,
		"started_at": r.cfg.Now(), "finished_at": nil, "error": nil,
		"session_id": sessionID, "subagents": nil,
	}); err != nil {
		r.release()
		os.RemoveAll(evidenceDir)
		return "", fmt.Errorf("advice: start dispatch: %w", err)
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.release()
		defer os.RemoveAll(evidenceDir)
		r.runAdvice(model, depth, findingsJSON, sessionID, statusPath, key, evidenceDir)
	}()
	return sessionID, nil
}

// AdviceStatus reads the single global advice slot's status, preferring the
// db and falling back to the status.json when no row exists.
func (r *Runner) AdviceStatus() (*db.AnalysisRun, bool, error) {
	return r.status("advice", "advice", filepath.Join(r.cfg.DataDir, "advice", "status.json"))
}

// runAdvice mirrors run_advice: fill the depth fragment into {DEPTH_BRIEF}
// first, then the frame's own placeholders (so a fragment's own
// {PROFILE_DIR}-style placeholders still resolve), spawn claude with
// --session-id, and dual-write the ending status. Subagents are counted
// only on success, and merged in only when known.
func (r *Runner) runAdvice(model, depth string, findingsJSON []byte, sessionID, statusPath, key, evidenceDir string) {
	fail := func(msg string) {
		r.dualStatus("advice", key, statusPath, map[string]any{
			"state": "error", "finished_at": r.cfg.Now(), "error": lastRunes(msg, 500),
		})
	}

	indexInput := filepath.Join(evidenceDir, "evidence.json")
	if depth == "shallow" {
		data, err := os.ReadFile(indexInput)
		if err != nil {
			fail(err.Error())
			return
		}
		indexInput = string(data)
	}
	prompt, err := advicePrompt(depth, findingsJSON, indexInput,
		filepath.Join(evidenceDir, "configurations.json"), evidenceDir)
	if err != nil {
		fail(err.Error())
		return
	}

	timeout := r.cfg.AnalysisTimeout
	if depth == "evidence" {
		timeout = r.cfg.EvidenceTimeout
	}

	args := []string{
		"-p", prompt, "--model", model,
		"--allowedTools", AdviceAllowedTools[depth],
		"--disallowedTools", AdviceDisallowedTools[depth],
		"--session-id", sessionID,
	}
	out, errOut, runErr, timedOut, spawnErr := r.spawn(args, timeout)
	if spawnErr != nil {
		fail(spawnErr.Error())
		return
	}
	if timedOut {
		r.dualStatus("advice", key, statusPath, map[string]any{
			"state": "error", "finished_at": r.cfg.Now(),
			"error": fmt.Sprintf("timed out after %ds", int(timeout.Seconds())),
		})
		return
	}
	if runErr == nil && strings.TrimSpace(out) != "" {
		if err := writeReport(filepath.Dir(statusPath), out); err != nil {
			fail(err.Error())
			return
		}
		fields := map[string]any{"state": "done", "finished_at": r.cfg.Now(), "error": nil}
		if n := r.subagentCount(sessionID); n != nil {
			fields["subagents"] = *n
		}
		if err := r.dualStatus("advice", key, statusPath, fields); err != nil {
			fmt.Fprintf(os.Stderr, "advice: final status write failed for %s: %v\n", sessionID, err)
		}
		return
	}
	if err := r.dualStatus("advice", key, statusPath, map[string]any{
		"state": "error", "finished_at": r.cfg.Now(), "error": errorTail(errOut),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "advice: final status write failed for %s: %v\n", sessionID, err)
	}
}

// indentFindings mirrors json.dumps(findings, indent=1): findingsJSON is
// already-serialized JSON, so this just re-indents it for the prompt.
func indentFindings(raw []byte) string {
	if len(raw) == 0 {
		return "[]"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", " "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// --- spawn -----------------------------------------------------------------

// spawn runs claudeBin with args, capturing stdout/stderr, honoring
// timeout. It mirrors Popen + communicate(timeout=...)/TimeoutExpired: on
// expiry it kills the process and still reaps it (draining the Wait
// goroutine) so it never leaves a zombie.
func (r *Runner) spawn(args []string, timeout time.Duration) (stdout, stderr string, runErr error, timedOut bool, spawnErr error) {
	return r.spawnInDir(args, timeout, r.cfg.WorkDir)
}

func (r *Runner) spawnInDir(args []string, timeout time.Duration, workDir string) (stdout, stderr string, runErr error, timedOut bool, spawnErr error) {
	cmd, readOutput, cleanup, err := r.command(args)
	if err != nil {
		return "", "", nil, false, err
	}
	defer cleanup()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if workDir != "" {
		cmd.Dir = workDir
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return "", "", nil, false, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case waitErr := <-done:
		if hookErr := report.ExecutionError(outBuf.Bytes()); hookErr != nil {
			return "", hookErr.Error(), hookErr, false, nil
		}
		output := outBuf.String()
		if waitErr == nil && readOutput != nil {
			output, waitErr = readOutput()
		}
		if waitErr == nil && r.provider() == "opencode" {
			output, waitErr = openCodeOutput(output)
		}
		if waitErr != nil && errBuf.Len() == 0 {
			errBuf.WriteString(waitErr.Error())
		}
		return output, errBuf.String(), waitErr, false, nil
	case <-time.After(timeout):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done // reap: always drain so the process never becomes a zombie
		return outBuf.String(), errBuf.String(), nil, true, nil
	}
}

// mergeEnv returns base with any existing key=... entries removed and a
// fresh key=val appended. Appending without removing the old entry first is
// not safe: most getenv implementations return the FIRST match for a
// duplicate key, so a naive append would silently lose to the original.
func mergeEnv(base []string, key, val string) []string {
	prefix := key + "="
	out := make([]string, 0, len(base)+1)
	for _, e := range base {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return append(out, prefix+val)
}

// dropEnv returns base with every key=... entry for key removed.
func dropEnv(base []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(base))
	for _, e := range base {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// IsDefaultProfile reports whether dir is the default profile, $HOME/.claude.
//
// The default profile is the one case where CLAUDE_CONFIG_DIR must be absent
// rather than set. Claude Code keeps the default profile's config at
// ~/.claude.json, in $HOME, and only moves it to <dir>/.claude.json when
// CLAUDE_CONFIG_DIR names <dir>. So exporting CLAUDE_CONFIG_DIR=$HOME/.claude
// does not describe the default profile, it creates a second one beside it:
// the spawned run would read different settings and a different account cache
// from the profile the operator meant to instrument. bin/limits-poller.sh
// makes the same distinction for the same reason.
// Delegates to internal/usage so the Go side has one implementation rather
// than two: the usage probe needs the same rule, and two copies of it drifting
// would send one of them at a config file nothing reads.
func IsDefaultProfile(dir string) bool { return usage.IsDefaultProfile(dir) }

// spawnEnv points a spawned claude at profileDir: by setting
// CLAUDE_CONFIG_DIR for a named profile, and by removing it for the default
// one. Removing rather than leaving it alone matters, because this process
// inherits launchd's environment and the operator's shell may have exported a
// CLAUDE_CONFIG_DIR of its own.
func spawnEnv(base []string, profileDir string) []string {
	if IsDefaultProfile(profileDir) {
		return dropEnv(base, "CLAUDE_CONFIG_DIR")
	}
	return mergeEnv(base, "CLAUDE_CONFIG_DIR", profileDir)
}

// --- count_subagents ---------------------------------------------------

// countSubagents mirrors count_subagents: POST a LogsQL query counting
// subagent_completed events against sessionID, 5s timeout, first nonempty
// NDJSON line's "n" field. Any failure at all - network, non-2xx, a
// malformed line - returns nil (unknown), never zero: an unknown count
// reported as zero would read as a run that obeyed a cap it may well have
// blown.
func countSubagents(vlURL, sessionID string) *int64 {
	query := fmt.Sprintf(`_time:7d event.name:subagent_completed session.id:"%s" | stats count() n`, sessionID)
	form := url.Values{"query": {query}, "limit": {"10"}}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.PostForm(vlURL+"/select/logsql/query", form)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row struct {
			N json.RawMessage `json:"n"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil
		}
		n, ok := parseCount(row.N)
		if !ok {
			return nil
		}
		return &n
	}
	return nil
}

// parseCount accepts "n" as either a JSON string ("7", VictoriaLogs' usual
// shape) or a JSON number, since either can come back.
func parseCount(raw json.RawMessage) (int64, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f), true
	}
	return 0, false
}

// --- status: merge, dual-write, fallback read -----------------------------

// mergeStatus builds one status: a fixed base of six keys (so analyse-kind
// status files never grow session_id/subagents keys), overlaid with whatever
// the existing file on disk holds, overlaid with fields for this call. A nil
// value in fields (Go's untyped nil) marshals to JSON null, which is how
// "explicitly unset" is spelled on the wire.
func mergeStatus(path string, fields map[string]any) map[string]any {
	status := map[string]any{
		"state": "running", "model": nil, "depth": nil,
		"started_at": nil, "finished_at": nil, "error": nil,
	}
	if data, err := os.ReadFile(path); err == nil {
		var existing map[string]any
		if json.Unmarshal(data, &existing) == nil {
			for k, v := range existing {
				status[k] = v
			}
		}
	}
	for k, v := range fields {
		status[k] = v
	}
	return status
}

// dualStatus merges fields onto path's existing status (or the defaults),
// writes the result atomically to path, and upserts the equivalent full row
// into the db (when configured). Both writes always describe the same
// merged status, so the file and the db row can never disagree.
func (r *Runner) dualStatus(kind, key, path string, fields map[string]any) error {
	status := mergeStatus(path, fields)
	if _, err := os.Stat(path); os.IsNotExist(err) && r.cfg.DB != nil {
		if row, ok, err := db.GetAnalysisRun(r.cfg.DB, kind, key); err != nil {
			return err
		} else if ok {
			status = map[string]any{"state": row.State, "model": row.Model, "depth": row.Depth, "started_at": row.StartedAt, "finished_at": row.FinishedAt, "error": row.Error, "session_id": row.RunSessionID, "scope_json": row.ScopeJSON, "execution_profile": row.ExecutionProfile}
			if row.Subagents != nil {
				status["subagents"] = *row.Subagents
			}
			for k, v := range fields {
				status[k] = v
			}
		}
	}
	if err := writeJSONAtomic(path, status); err != nil {
		return err
	}
	if r.cfg.DB == nil {
		return nil
	}
	return db.PutAnalysisRun(r.cfg.DB, statusToRow(kind, key, status))
}

// status reads one status: the db row when DB is configured and a row
// exists, else the status.json (a file-only Runner, or a run whose db row
// was lost). ok is false with a nil error when neither exists.
func (r *Runner) status(kind, key, path string) (*db.AnalysisRun, bool, error) {
	if r.cfg.DB != nil {
		row, ok, err := db.GetAnalysisRun(r.cfg.DB, kind, key)
		if err != nil {
			return nil, false, err
		}
		if ok {
			return row, true, nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("analysis: read %s: %w", path, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false, fmt.Errorf("analysis: parse %s: %w", path, err)
	}
	row := statusToRow(kind, key, raw)
	return &row, true, nil
}

// statusToRow converts a merged status map into a db.AnalysisRun, used both
// to build the row dualStatus upserts and to parse a status.json read back
// off disk. A missing or null value maps to a zero Go value (empty
// string, 0), matching what db.AnalysisRun's fields hold for "never set".
func statusToRow(kind, key string, status map[string]any) db.AnalysisRun {
	return db.AnalysisRun{
		Kind:             kind,
		Key:              key,
		State:            stringOf(status["state"]),
		Model:            stringOf(status["model"]),
		Depth:            stringOf(status["depth"]),
		StartedAt:        int64Of(status["started_at"]),
		FinishedAt:       int64Of(status["finished_at"]),
		Error:            stringOf(status["error"]),
		RunSessionID:     stringOf(status["session_id"]),
		Subagents:        int64PtrOf(status["subagents"]),
		ScopeJSON:        stringOf(status["scope_json"]),
		ExecutionProfile: stringOf(status["execution_profile"]),
		Provider:         stringOf(status["provider"]), Profile: stringOf(status["profile"]), SourceProvider: stringOf(status["source_provider"]), SourceProfile: stringOf(status["source_profile"]), SourceAccountKey: stringOf(status["source_account_key"]),
	}
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func int64Of(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	default:
		return 0
	}
}

func int64PtrOf(v any) *int64 {
	if v == nil {
		return nil
	}
	n := int64Of(v)
	return &n
}

// --- small helpers ---------------------------------------------------------

// errorTail mirrors `(err or "claude produced an empty report").strip()[-500:]`:
// an empty stderr capture falls back to a placeholder, then both are
// trimmed and truncated to the last 500 characters.
func errorTail(stderr string) string {
	raw := stderr
	if raw == "" {
		raw = "analysis provider produced an empty report"
	}
	return lastRunes(strings.TrimSpace(raw), 500)
}

// lastRunes returns the last n runes of s: code points, not bytes, so a
// truncated error message never ends mid-character.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// --- report files ----------------------------------------------------------

// writeReport lands one run's output in dir. The model is asked for a single
// JSON object, so parseable output is written as report.json and is what the
// dashboard renders. Output that will not parse is still worth keeping: it
// goes to report.md verbatim, the run still ends "done", and the dashboard
// explains that its format cannot be displayed. Hook rejection diagnostics
// are execution errors, never reports. There is no retry: the
// money is already spent, and a silent second run would spend it again.
//
// Whichever file this run did not write is removed. The server prefers
// report.json, so a stale one left beside a fresh report.md would quietly
// serve the previous run's report as though it were this one's.
func writeReport(dir, out string) error {
	if err := report.ExecutionError([]byte(out)); err != nil {
		return err
	}
	rep, err := report.Parse([]byte(out))
	if err != nil {
		fmt.Fprintf(os.Stderr, "analysis: report did not parse (%v), keeping the raw output as report.md\n", err)
		if werr := writeTextAtomic(filepath.Join(dir, "report.md"), out); werr != nil {
			return werr
		}
		return removeIfExists(filepath.Join(dir, "report.json"))
	}
	data, err := report.Marshal(rep)
	if err != nil {
		return fmt.Errorf("marshal report for %s: %w", dir, err)
	}
	if werr := writeAtomic(filepath.Join(dir, "report.json"), data); werr != nil {
		return werr
	}
	return removeIfExists(filepath.Join(dir, "report.md"))
}

// removeIfExists deletes path, treating "already gone" as success.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// --- atomic file writes ----------------------------------------------------

// writeJSONAtomic and writeTextAtomic write to a temp file in the target's
// directory, then rename over it, so a reader never observes a partial
// write.
func writeJSONAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	return writeAtomic(path, data)
}

func writeTextAtomic(path, text string) error {
	return writeAtomic(path, []byte(text))
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp for %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp for %s: %w", path, err)
	}
	return nil
}
