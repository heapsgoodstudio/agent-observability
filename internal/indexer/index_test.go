package indexer

import (
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/workspace"
)

// fixturePath resolves internal/testdata/fixture-session.jsonl relative to
// this file, the same convention internal/transcript/parse_test.go uses, so
// `go test ./...` finds it from any working directory.
func fixturePath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve caller for fixture path")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "testdata", "fixture-session.jsonl")
}

func almostEqual(t *testing.T, name string, got, want float64) {
	t.Helper()
	const eps = 1e-9
	if math.Abs(got-want) > eps {
		t.Errorf("%s = %.9f, want %.9f", name, got, want)
	}
}

// testEnv is one indexing fixture: a temp projects root with one project dir
// holding the fixture transcript under a fixed session id, a temp out root,
// and a fresh migrated db. run() indexes against those fixed roots; rows()
// and detail() read the results back out of the sessions table.
type testEnv struct {
	t          *testing.T
	projects   string
	out        string
	db         *sql.DB
	sid        string
	transcript string
	// resolver, when set, replaces the git-backed workspace resolver, so a
	// case can name directories without building repositories on disk. The
	// fixture transcript records no cwd at all, so the default nil never
	// reaches git.
	resolver workspace.Resolver
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "projects", "-Users-test-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "data")

	d, err := db.Open(filepath.Join(tmp, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}

	sid := "11111111-1111-4111-8111-111111111111"
	transcript := filepath.Join(proj, sid+".jsonl")
	data, err := os.ReadFile(fixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, data, 0o644); err != nil {
		t.Fatal(err)
	}

	return &testEnv{
		t: t, projects: filepath.Join(tmp, "projects"), out: out,
		db: d, sid: sid, transcript: transcript,
	}
}

func (e *testEnv) run() Stats {
	e.t.Helper()
	stats, err := RunIndex(IndexOpts{
		ProjectsRoot: e.projects, OutRoot: e.out, DB: e.db,
		Now:       func() int64 { return 1755300000 },
		Workspace: e.resolver,
	})
	if err != nil {
		e.t.Fatalf("RunIndex: %v", err)
	}
	return stats
}

// rows reads the sessions table back via db.ListSessions, cost_usd
// descending.
func (e *testEnv) rows() []db.SessionRow {
	e.t.Helper()
	rows, err := db.ListSessions(e.db, "")
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

// detail reads one session's full row back via db.GetSession.
func (e *testEnv) detail(sid string) db.SessionRow {
	e.t.Helper()
	row, ok, err := db.GetSession(e.db, sid)
	if err != nil {
		e.t.Fatal(err)
	}
	if !ok {
		e.t.Fatalf("session %s not found in the sessions table", sid)
	}
	return *row
}

func TestWritesIndexAndDetail(t *testing.T) {
	e := newTestEnv(t)
	stats := e.run()
	want := Stats{Sessions: 1, Parsed: 1, Unpriced: []string{}}
	if diff := cmp.Diff(want, stats); diff != "" {
		t.Errorf("stats diff (-want +got):\n%s", diff)
	}

	rows := e.rows()
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.ID != e.sid {
		t.Errorf("id = %v, want %v", row.ID, e.sid)
	}
	if row.Project != "-Users-test-proj" {
		t.Errorf("project = %v, want -Users-test-proj", row.Project)
	}
	if !row.HasTranscript {
		t.Errorf("has_transcript = %v, want true", row.HasTranscript)
	}
	if row.Analysed {
		t.Errorf("analysed = %v, want false", row.Analysed)
	}

	detail := e.detail(e.sid)
	wantTools := map[string]int{
		"Bash": 1, "Read": 1, "Skill": 1,
		"mcp__plugin_linear_linear__get_issue": 1,
	}
	if diff := cmp.Diff(wantTools, detail.Tools); diff != "" {
		t.Errorf("tools diff (-want +got):\n%s", diff)
	}
	if len(detail.Trajectory) != 3 {
		t.Errorf("trajectory = %v, want 3 points", detail.Trajectory)
	}
}

func TestIncrementalSkip(t *testing.T) {
	e := newTestEnv(t)
	e.run()
	if stats := e.run(); stats.Parsed != 0 {
		t.Errorf("Parsed = %d, want 0", stats.Parsed)
	}
}

func TestReparseOnChange(t *testing.T) {
	e := newTestEnv(t)
	e.run()

	f, err := os.OpenFile(e.transcript, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if stats := e.run(); stats.Parsed != 1 {
		t.Errorf("Parsed = %d, want 1", stats.Parsed)
	}
}

func TestRowsSurviveTranscriptDeletion(t *testing.T) {
	e := newTestEnv(t)
	e.run()
	if err := os.Remove(e.transcript); err != nil {
		t.Fatal(err)
	}
	e.run()

	row := e.rows()[0]
	if row.HasTranscript {
		t.Errorf("has_transcript = %v, want false", row.HasTranscript)
	}
	if row.Requests != 3 {
		t.Errorf("requests = %v, want 3", row.Requests)
	}
}

func TestAnalysedFlag(t *testing.T) {
	e := newTestEnv(t)
	dir := filepath.Join(e.out, "analyses", e.sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# r"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.run()
	if !e.rows()[0].Analysed {
		t.Errorf("analysed = %v, want true", e.rows()[0].Analysed)
	}
}

// A run that produced a structured report writes report.json and no
// report.md, so a check for report.md alone would silently stop counting every
// new analysis.
func TestAnalysedFlagFromStructuredReport(t *testing.T) {
	e := newTestEnv(t)
	dir := filepath.Join(e.out, "analyses", e.sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"),
		[]byte(`{"schema":1,"headline":"H","summary":"S"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.run()
	if !e.rows()[0].Analysed {
		t.Errorf("analysed = %v, want true for a session with only report.json", e.rows()[0].Analysed)
	}
}

func TestEmptyTranscriptNotReparsed(t *testing.T) {
	e := newTestEnv(t)
	empty := filepath.Join(e.projects, "-Users-test-proj", "44444444-4444-4444-8444-444444444444.jsonl")
	if err := os.WriteFile(empty, []byte(`{"type":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if stats := e.run(); stats.Parsed != 2 {
		t.Errorf("first Parsed = %d, want 2", stats.Parsed)
	}
	if stats := e.run(); stats.Parsed != 0 {
		t.Errorf("second Parsed = %d, want 0", stats.Parsed)
	}
}

func TestSubagentTranscriptsMerge(t *testing.T) {
	e := newTestEnv(t)
	sub := filepath.Join(e.projects, "-Users-test-proj", e.sid, "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"user","isSidechain":true,"message":{"role":"user","content":"do subagent things"},"timestamp":"2026-08-01T10:00:12.000Z"}
{"type":"assistant","isSidechain":true,"requestId":"req_n1","message":{"id":"msg_n1","model":"claude-haiku-4-5-20251001","role":"assistant","content":[{"type":"tool_use","id":"toolu_n1","name":"Grep","input":{"pattern":"x"}}],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}},"timestamp":"2026-08-01T10:00:15.000Z"}
`
	if err := os.WriteFile(filepath.Join(sub, "agent-x1.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	e.run()
	row := e.rows()[0]
	if row.Requests != 4 {
		t.Errorf("requests = %v, want 4", row.Requests)
	}
	almostEqual(t, "cost_usd", row.CostUSD, 0.002805)
	almostEqual(t, "sidechain_cost_usd", row.SidechainCostUSD, 0.00022)
	if row.UserTurns != 2 {
		t.Errorf("user_turns = %v, want 2", row.UserTurns)
	}

	detail := e.detail(e.sid)
	if detail.Tools["Grep"] != 1 {
		t.Errorf("tools[Grep] = %v, want 1", detail.Tools["Grep"])
	}
}

func TestSubagentChangeTriggersReparse(t *testing.T) {
	e := newTestEnv(t)
	sub := filepath.Join(e.projects, "-Users-test-proj", e.sid, "subagents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "agent-x1.jsonl")
	content := `{"type":"user","isSidechain":true,"message":{"role":"user","content":"hi"},"timestamp":"2026-08-01T10:00:12.000Z"}
`
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	e.run()
	if stats := e.run(); stats.Parsed != 0 {
		t.Errorf("Parsed after no change = %d, want 0", stats.Parsed)
	}

	fh, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}

	if stats := e.run(); stats.Parsed != 1 {
		t.Errorf("Parsed after subagent change = %d, want 1", stats.Parsed)
	}
}

func TestUnpricedModelReported(t *testing.T) {
	e := newTestEnv(t)
	p := filepath.Join(e.projects, "-Users-test-proj", "55555555-5555-4555-8555-555555555555.jsonl")
	content := `{"type":"assistant","isSidechain":false,"requestId":"req_u1","message":{"id":"msg_u1","model":"claude-future-9","role":"assistant","content":[],"usage":{"input_tokens":5,"output_tokens":5}},"timestamp":"2026-08-01T10:00:00.000Z"}
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	stats := e.run()
	if diff := cmp.Diff([]string{"claude-future-9"}, stats.Unpriced); diff != "" {
		t.Errorf("Unpriced diff (-want +got):\n%s", diff)
	}

	byID := map[string]db.SessionRow{}
	for _, r := range e.rows() {
		byID[r.ID] = r
	}
	if byID["55555555-5555-4555-8555-555555555555"].CostUSD != 0.0 {
		t.Errorf("cost_usd = %v, want 0", byID["55555555-5555-4555-8555-555555555555"].CostUSD)
	}
}

// TestDetailSkillsEmptyNotNull guards a bug found in review: a session with
// no Skill tool_use yields a nil Skills slice, since transcript.Parse builds
// it via append([]string(nil), sc.Skills...), which returns nil (not an
// empty non-nil slice) when there is nothing to append. That once serialized
// as JSON null rather than [].
// db.UpsertSession normalizes nil collections to non-nil-empty
// before storing them, so this guards that RunIndex's freshly parsed
// row (handed straight to UpsertSession, never having round-tripped through
// the db before) still comes back non-nil through db.GetSession. The shared
// fixture used by newTestEnv always has a Skill call, which is why none of
// the tests above this one exercise the no-skill path.
func TestDetailSkillsEmptyNotNull(t *testing.T) {
	e := newTestEnv(t)
	sid := "66666666-6666-4666-8666-666666666666"
	p := filepath.Join(e.projects, "-Users-test-proj", sid+".jsonl")
	content := `{"type":"assistant","isSidechain":false,"requestId":"req_ns1","message":{"id":"msg_ns1","model":"claude-sonnet-5-20250929","role":"assistant","content":[{"type":"tool_use","id":"toolu_ns1","name":"Bash","input":{"command":"ls"}}],"usage":{"input_tokens":10,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}},"timestamp":"2026-08-01T10:00:00.000Z"}
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	e.run()

	detail := e.detail(sid)
	if detail.Skills == nil {
		t.Fatal("Skills is nil, want a non-nil empty slice")
	}
	if len(detail.Skills) != 0 {
		t.Errorf("Skills = %v, want empty", detail.Skills)
	}

	if detail.MCPServers == nil {
		t.Fatal("MCPServers is nil, want a non-nil empty slice")
	}
	if len(detail.MCPServers) != 0 {
		t.Errorf("MCPServers = %v, want empty", detail.MCPServers)
	}
}

// TestSchemaVersionChangeForcesReparse tampers with the meta table's
// sessions_schema value directly, which is the gate RunIndex checks: a
// mismatch must clear transcript_state and force every transcript through
// the parser again.
func TestSchemaVersionChangeForcesReparse(t *testing.T) {
	tmp := t.TempDir()
	proj := filepath.Join(tmp, "projects", "-p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	sid := "11111111-1111-4111-8111-111111111111"
	if err := os.WriteFile(filepath.Join(proj, sid+".jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := db.Open(filepath.Join(tmp, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}

	opts := IndexOpts{
		ProjectsRoot: filepath.Join(tmp, "projects"), OutRoot: filepath.Join(tmp, "out"), DB: d,
		Now: func() int64 { return 1755300000 },
	}

	first, err := RunIndex(opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Parsed != 1 {
		t.Errorf("first Parsed = %d, want 1", first.Parsed)
	}

	second, err := RunIndex(opts)
	if err != nil {
		t.Fatal(err)
	}
	if second.Parsed != 0 {
		t.Errorf("second Parsed = %d, want 0", second.Parsed)
	}

	if err := db.MetaSet(d, "sessions_schema", "go-1"); err != nil {
		t.Fatal(err)
	}

	third, err := RunIndex(opts)
	if err != nil {
		t.Fatal(err)
	}
	if third.Parsed != 1 {
		t.Errorf("third Parsed after stale schema = %d, want 1", third.Parsed)
	}
}

// TestNoJSONWritten pins that RunIndex never creates sessions/index.json or
// sessions/<id>.json. The sessions table in sqlite is the only place a
// session's data lives, on a fresh run and a steady-state one alike.
func TestNoJSONWritten(t *testing.T) {
	e := newTestEnv(t)
	e.run()
	e.run() // a steady-state (no-op) pass must not conjure the files either

	sessionsDir := filepath.Join(e.out, "sessions")
	if _, err := os.Stat(filepath.Join(sessionsDir, "index.json")); !os.IsNotExist(err) {
		t.Errorf("sessions/index.json exists (stat err=%v), want ENOENT", err)
	}
	if _, err := os.Stat(filepath.Join(sessionsDir, e.sid+".json")); !os.IsNotExist(err) {
		t.Errorf("sessions/%s.json exists (stat err=%v), want ENOENT", e.sid, err)
	}
}

// mapResolver answers from maps, mirroring internal/workspace's own test
// double. A directory absent from tops is not a repository.
type mapResolver struct {
	tops map[string]string
	urls map[string]string
}

// noRepos is the zero mapResolver: nothing is a repository, so a name falls
// back to the path or the project key without ever reaching git. It is what
// the backfill tests want, since their fixture's directories do not exist.
var noRepos = mapResolver{}

func (m mapResolver) Toplevel(dir string) (string, bool) {
	top, ok := m.tops[dir]
	return top, ok
}

func (m mapResolver) OriginURL(top string) (string, bool) {
	url, ok := m.urls[top]
	return url, ok
}

// appendCwd rewrites the transcript so every record carries cwd, which the
// fixture itself does not record.
func (e *testEnv) appendCwd(cwd string) {
	e.t.Helper()
	data, err := os.ReadFile(e.transcript)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []byte
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			e.t.Fatal(err)
		}
		rec["cwd"] = cwd
		enc, err := json.Marshal(rec)
		if err != nil {
			e.t.Fatal(err)
		}
		out = append(out, enc...)
		out = append(out, '\n')
	}
	if err := os.WriteFile(e.transcript, out, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func TestWorkspaceFromRepo(t *testing.T) {
	e := newTestEnv(t)
	e.appendCwd("/w/acme/api")
	e.resolver = mapResolver{
		tops: map[string]string{"/w/acme/api": "/w/acme/api"},
		urls: map[string]string{"/w/acme/api": "git@github.com:acme/api.git"},
	}
	e.run()

	row := e.detail(e.sid)
	if row.Workspace != "acme/api" {
		t.Errorf("workspace = %q, want acme/api", row.Workspace)
	}
	// The join key spend attribution runs through is untouched by the
	// descriptive name sitting beside it.
	if row.Project != "-Users-test-proj" {
		t.Errorf("project = %q, want -Users-test-proj", row.Project)
	}
}

// A transcript with no cwd at all reads exactly as it did before the column
// existed, rather than going blank.
func TestWorkspaceFallsBackToProject(t *testing.T) {
	e := newTestEnv(t)
	e.run()

	row := e.detail(e.sid)
	if row.Workspace != "proj" {
		t.Errorf("workspace = %q, want proj", row.Workspace)
	}
}

// --- several profiles --------------------------------------------------

// writeProfileRoot builds one profile's projects root holding the fixture
// transcript under sid, and returns the root.
func writeProfileRoot(t *testing.T, base, project, sid string) string {
	t.Helper()
	root := filepath.Join(base, "projects")
	proj := filepath.Join(root, project)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, sid+".jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func migratedDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestRunIndexAttributesEachProfile: two profiles index into one table, each
// row saying which profile it came from.
//
// Session ids are uuids, so the profiles cannot collide on the primary key
// and the rows would merge correctly without the column at all. What they
// could not do is say where they came from, which is the whole point of it.
func TestRunIndexAttributesEachProfile(t *testing.T) {
	tmp := t.TempDir()
	d := migratedDB(t, filepath.Join(tmp, "state.db"))

	const sidA = "11111111-1111-4111-8111-111111111111"
	const sidB = "22222222-2222-4222-8222-222222222222"
	rootA := writeProfileRoot(t, filepath.Join(tmp, "a"), "-proj-a", sidA)
	rootB := writeProfileRoot(t, filepath.Join(tmp, "b"), "-proj-b", sidB)

	if _, err := RunIndex(IndexOpts{
		Profiles: []ProfileRoot{
			{Name: "alpha", ProjectsRoot: rootA},
			{Name: "beta", ProjectsRoot: rootB},
		},
		OutRoot: filepath.Join(tmp, "out"),
		DB:      d,
		Now:     func() int64 { return 1755300000 },
	}); err != nil {
		t.Fatalf("RunIndex: %v", err)
	}

	rows, err := db.ListSessions(d, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.ID] = r.Profile
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d: %v", len(got), got)
	}
	if got[sidA] != "alpha" {
		t.Errorf("session A attributed to %q, want alpha", got[sidA])
	}
	if got[sidB] != "beta" {
		t.Errorf("session B attributed to %q, want beta", got[sidB])
	}
}

// TestRunIndexFallsBackToTheSingleRoot keeps a caller that predates profiles
// working: ProjectsRoot alone still indexes, attributed to no profile rather
// than to a guessed one.
func TestRunIndexFallsBackToTheSingleRoot(t *testing.T) {
	tmp := t.TempDir()
	d := migratedDB(t, filepath.Join(tmp, "state.db"))
	const sid = "33333333-3333-4333-8333-333333333333"
	root := writeProfileRoot(t, filepath.Join(tmp, "p"), "-proj", sid)

	if _, err := RunIndex(IndexOpts{
		ProjectsRoot: root,
		OutRoot:      filepath.Join(tmp, "out"),
		DB:           d,
		Now:          func() int64 { return 1755300000 },
	}); err != nil {
		t.Fatalf("RunIndex: %v", err)
	}
	rows, err := db.ListSessions(d, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Profile != "" {
		t.Errorf("profile = %q, want empty for a single-root caller", rows[0].Profile)
	}
}
