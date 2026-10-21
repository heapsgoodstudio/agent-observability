package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
)

// openMigrated opens a fresh temp db and runs every migration, the setUp
// every test in this file needs.
func openMigrated(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-obs.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := Migrate(d); err != nil {
		t.Fatal(err)
	}
	return d
}

// fullRow is a SessionRow with every field populated, used as the
// round-trip fixture for UpsertSession/GetSession/ListSessions.
func fullRow(id string, cost float64) SessionRow {
	return SessionRow{
		ID:        id,
		Project:   "my-project",
		Workspace: "acme/my-project",
		Start:     1755000000,
		End:       1755000600,
		CostUSD:   cost,
		Requests:  3,
		Tokens: transcript.Tokens{
			Input: 115, Output: 225, CacheRead: 1000, CacheCreation: 50,
		},
		SidechainCostUSD: 0.00011,
		ToolCalls:        4,
		UserTurns:        2,
		Models: []transcript.ModelCost{
			{Model: "claude-sonnet-5", CostUSD: 0.002585},
			{Model: "claude-haiku-4-5", CostUSD: 0.00011},
		},
		Skills:     []string{"usage-report"},
		MCPServers: []string{"linear"},
		Tools: map[string]int{
			"Bash": 1, "Read": 1, "Skill": 1,
		},
		Trajectory: [][3]float64{
			{1755000000, 0.001, 100},
			{1755000600, 0.002695, 115},
		},
		HasTranscript: true,
		Analysed:      false,
	}
}

// TestMigrateAppliesSessionsSchema exercises the realistic upgrade path: an
// existing database that already has migration 0001 applied picks up every
// later migration when Migrate runs again, since none of them is yet recorded
// in schema_migrations.
func TestMigrateAppliesSessionsSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-obs.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.Exec(`CREATE TABLE schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO schema_migrations(version) VALUES('0001_meta.sql')`); err != nil {
		t.Fatal(err)
	}

	n, err := Migrate(d)
	if err != nil {
		t.Fatal(err)
	}
	if want := migrationCount(t) - 1; n != want {
		t.Fatalf("applied = %d, want %d", n, want)
	}

	for _, table := range []string{"sessions", "transcript_state", "analysis_runs"} {
		var name string
		if err := d.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s missing after migrate: %v", table, err)
		}
	}
}

func TestUpsertGetRoundTrip(t *testing.T) {
	d := openMigrated(t)
	want := fullRow("sess-1", 0.002695)

	if err := UpsertSession(d, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := GetSession(d, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetSession ok = false, want true")
	}
	if diff := cmp.Diff(want, *got); diff != "" {
		t.Errorf("round trip diff (-want +got):\n%s", diff)
	}
}

func TestUpsertOverwritesExisting(t *testing.T) {
	d := openMigrated(t)
	first := fullRow("sess-1", 0.5)
	if err := UpsertSession(d, first); err != nil {
		t.Fatal(err)
	}
	second := fullRow("sess-1", 1.5)
	second.Requests = 9
	if err := UpsertSession(d, second); err != nil {
		t.Fatal(err)
	}
	got, ok, err := GetSession(d, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetSession ok = false, want true")
	}
	if diff := cmp.Diff(second, *got); diff != "" {
		t.Errorf("overwrite diff (-want +got):\n%s", diff)
	}
}

// TestUpsertGetEmptyCollections pins the round-trip convention for
// collection fields: nil in, non-nil empty out, never a null column.
func TestUpsertGetEmptyCollections(t *testing.T) {
	d := openMigrated(t)
	row := SessionRow{
		ID:            "sess-empty",
		Project:       "p",
		Start:         1,
		End:           2,
		CostUSD:       0,
		Requests:      0,
		HasTranscript: true,
	}
	if err := UpsertSession(d, row); err != nil {
		t.Fatal(err)
	}
	got, ok, err := GetSession(d, "sess-empty")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetSession ok = false, want true")
	}
	if got.Models == nil || len(got.Models) != 0 {
		t.Errorf("Models = %#v, want non-nil empty", got.Models)
	}
	if got.Skills == nil || len(got.Skills) != 0 {
		t.Errorf("Skills = %#v, want non-nil empty", got.Skills)
	}
	if got.MCPServers == nil || len(got.MCPServers) != 0 {
		t.Errorf("MCPServers = %#v, want non-nil empty", got.MCPServers)
	}
	if got.Tools == nil || len(got.Tools) != 0 {
		t.Errorf("Tools = %#v, want non-nil empty", got.Tools)
	}
	if got.Trajectory == nil || len(got.Trajectory) != 0 {
		t.Errorf("Trajectory = %#v, want non-nil empty", got.Trajectory)
	}
}

func TestListSessionsOrdersByCostDesc(t *testing.T) {
	d := openMigrated(t)
	for _, r := range []SessionRow{
		fullRow("cheap", 0.1),
		fullRow("expensive", 9.9),
		fullRow("mid", 3.3),
	} {
		if err := UpsertSession(d, r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := ListSessions(d, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}
	gotOrder := []string{rows[0].ID, rows[1].ID, rows[2].ID}
	want := []string{"expensive", "mid", "cheap"}
	if diff := cmp.Diff(want, gotOrder); diff != "" {
		t.Errorf("order diff (-want +got):\n%s", diff)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	d := openMigrated(t)
	got, ok, err := GetSession(d, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("ok = true, want false")
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil", got)
	}
}

// TestMarkMissing pins the permanence rule: rows whose id is not in seenIDs
// get has_transcript flipped to false and analysed refreshed from the
// analysed map (default false when absent); everything else, including
// numeric fields, is untouched. Rows in seenIDs are untouched entirely.
func TestMarkMissing(t *testing.T) {
	d := openMigrated(t)
	seen := fullRow("seen", 1.0)
	seen.HasTranscript = true
	seen.Analysed = true
	gone := fullRow("gone", 2.0)
	gone.HasTranscript = true
	gone.Analysed = false
	goneAnalysed := fullRow("gone-analysed", 3.0)
	goneAnalysed.HasTranscript = true
	goneAnalysed.Analysed = false

	for _, r := range []SessionRow{seen, gone, goneAnalysed} {
		if err := UpsertSession(d, r); err != nil {
			t.Fatal(err)
		}
	}

	err := MarkMissing(d,
		map[string]bool{"seen": true},
		map[string]bool{"gone-analysed": true},
	)
	if err != nil {
		t.Fatal(err)
	}

	gotSeen, _, err := GetSession(d, "seen")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(seen, *gotSeen); diff != "" {
		t.Errorf("seen row changed unexpectedly (-want +got):\n%s", diff)
	}

	gotGone, _, err := GetSession(d, "gone")
	if err != nil {
		t.Fatal(err)
	}
	wantGone := gone
	wantGone.HasTranscript = false
	wantGone.Analysed = false
	if diff := cmp.Diff(wantGone, *gotGone); diff != "" {
		t.Errorf("gone row diff (-want +got):\n%s", diff)
	}

	gotGoneAnalysed, _, err := GetSession(d, "gone-analysed")
	if err != nil {
		t.Fatal(err)
	}
	wantGoneAnalysed := goneAnalysed
	wantGoneAnalysed.HasTranscript = false
	wantGoneAnalysed.Analysed = true
	if diff := cmp.Diff(wantGoneAnalysed, *gotGoneAnalysed); diff != "" {
		t.Errorf("gone-analysed row diff (-want +got):\n%s", diff)
	}
}

func TestStateRoundTripAndPrune(t *testing.T) {
	d := openMigrated(t)
	a := TranscriptState{Path: "/a.jsonl", Mtime: 100.5, Size: 200, FileCount: 1, Status: "ok"}
	b := TranscriptState{Path: "/b.jsonl", Mtime: 50.25, Size: 10, FileCount: 2, Status: "empty"}

	if err := StatePut(d, a); err != nil {
		t.Fatal(err)
	}
	if err := StatePut(d, b); err != nil {
		t.Fatal(err)
	}

	gotA, ok, err := StateGet(d, "/a.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("StateGet(/a.jsonl) ok = false, want true")
	}
	if diff := cmp.Diff(&a, gotA); diff != "" {
		t.Errorf("state a diff (-want +got):\n%s", diff)
	}

	// StatePut again on the same path overwrites rather than duplicating.
	a2 := a
	a2.Size = 999
	if err := StatePut(d, a2); err != nil {
		t.Fatal(err)
	}
	gotA2, _, err := StateGet(d, "/a.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(&a2, gotA2); diff != "" {
		t.Errorf("state a2 diff (-want +got):\n%s", diff)
	}

	if err := StatePruneMissing(d, map[string]bool{"/a.jsonl": true}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := StateGet(d, "/a.jsonl"); err != nil || !ok {
		t.Fatalf("StateGet(/a.jsonl) after prune: ok=%v err=%v, want ok=true", ok, err)
	}
	if _, ok, err := StateGet(d, "/b.jsonl"); err != nil || ok {
		t.Fatalf("StateGet(/b.jsonl) after prune: ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestStateGetMissing(t *testing.T) {
	d := openMigrated(t)
	got, ok, err := StateGet(d, "/missing.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("ok = true, want false")
	}
	if got != nil {
		t.Fatalf("got = %+v, want nil", got)
	}
}

func TestMetaGetSet(t *testing.T) {
	d := openMigrated(t)
	if _, ok, err := MetaGet(d, "sessions_schema"); err != nil || ok {
		t.Fatalf("MetaGet before set: ok=%v err=%v, want ok=false", ok, err)
	}
	if err := MetaSet(d, "sessions_schema", "go-2"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := MetaGet(d, "sessions_schema")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || v != "go-2" {
		t.Fatalf("MetaGet = %q, %v, want %q, true", v, ok, "go-2")
	}
	if err := MetaSet(d, "sessions_schema", "go-3"); err != nil {
		t.Fatal(err)
	}
	v, ok, err = MetaGet(d, "sessions_schema")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || v != "go-3" {
		t.Fatalf("MetaGet after update = %q, %v, want %q, true", v, ok, "go-3")
	}
}
