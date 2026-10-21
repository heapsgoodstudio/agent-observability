package db

import (
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// fullAnalysisRun is an AnalysisRun with every field populated, used as the
// round-trip fixture for PutAnalysisRun/GetAnalysisRun. subagents lets tests
// exercise both the set and nil (unknown) cases.
func fullAnalysisRun(kind, key string, subagents *int64) AnalysisRun {
	return AnalysisRun{
		Kind:             kind,
		Key:              key,
		State:            "done",
		Model:            "claude-opus-5",
		Depth:            "standard",
		StartedAt:        1755000000,
		FinishedAt:       1755000600,
		Error:            "",
		RunSessionID:     "run-abc",
		Subagents:        subagents,
		ScopeJSON:        `{"provider":"claude","account_key":"claude:one"}`,
		ExecutionProfile: "executor",
	}
}

// TestMigrateAppliesAnalysisRunsSchema exercises the realistic upgrade path:
// an existing database that already has migrations 0001 and 0002 applied picks
// up every later migration when Migrate runs again, and ends up with the
// analysis_runs table 0003 creates.
func TestMigrateAppliesAnalysisRunsSchema(t *testing.T) {
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
	if _, err := d.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TABLE transcript_state (path TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"0001_meta.sql", "0002_sessions.sql"} {
		if _, err := d.Exec(`INSERT INTO schema_migrations(version) VALUES(?)`, v); err != nil {
			t.Fatal(err)
		}
	}

	n, err := Migrate(d)
	if err != nil {
		t.Fatal(err)
	}
	if want := migrationCount(t) - 2; n != want {
		t.Fatalf("applied = %d, want %d", n, want)
	}

	var name string
	if err := d.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'analysis_runs'`).Scan(&name); err != nil {
		t.Fatalf("table analysis_runs missing after migrate: %v", err)
	}
}

func TestPutGetAnalysisRunRoundTrip(t *testing.T) {
	d := openMigrated(t)
	subagents := int64(7)
	want := fullAnalysisRun("analysis", "sess-1", &subagents)

	if err := PutAnalysisRun(d, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := GetAnalysisRun(d, "analysis", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetAnalysisRun ok = false, want true")
	}
	if diff := cmp.Diff(want, *got); diff != "" {
		t.Errorf("round trip diff (-want +got):\n%s", diff)
	}
}

// TestPutGetAdviceRunRoundTrip pins the "advice" kind's single global slot:
// key is the fixed string "advice", mirroring the single advice/status.json.
func TestPutGetAdviceRunRoundTrip(t *testing.T) {
	d := openMigrated(t)
	want := fullAnalysisRun("advice", "advice", nil)

	if err := PutAnalysisRun(d, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := GetAnalysisRun(d, "advice", "advice")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetAnalysisRun ok = false, want true")
	}
	if diff := cmp.Diff(want, *got); diff != "" {
		t.Errorf("round trip diff (-want +got):\n%s", diff)
	}
}

// TestPutGetAnalysisRunNilSubagents pins nil Subagents round-tripping as
// nil, mirroring subagents being omitted entirely from status.json (as
// opposed to the other fields, which are written as explicit nulls) while a
// run is still in progress and hasn't counted its subagents yet.
func TestPutGetAnalysisRunNilSubagents(t *testing.T) {
	d := openMigrated(t)
	want := fullAnalysisRun("analysis", "sess-running", nil)

	if err := PutAnalysisRun(d, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := GetAnalysisRun(d, "analysis", "sess-running")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetAnalysisRun ok = false, want true")
	}
	if got.Subagents != nil {
		t.Errorf("Subagents = %v, want nil", *got.Subagents)
	}
	if diff := cmp.Diff(want, *got); diff != "" {
		t.Errorf("round trip diff (-want +got):\n%s", diff)
	}
}

func TestPutAnalysisRunOverwritesExisting(t *testing.T) {
	d := openMigrated(t)
	subagents := int64(3)
	first := fullAnalysisRun("analysis", "sess-1", &subagents)
	if err := PutAnalysisRun(d, first); err != nil {
		t.Fatal(err)
	}

	second := first
	second.State = "error"
	second.Error = "boom"
	second.FinishedAt = 1755000900
	second.Subagents = nil
	if err := PutAnalysisRun(d, second); err != nil {
		t.Fatal(err)
	}

	got, ok, err := GetAnalysisRun(d, "analysis", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("GetAnalysisRun ok = false, want true")
	}
	if diff := cmp.Diff(second, *got); diff != "" {
		t.Errorf("overwrite diff (-want +got):\n%s", diff)
	}
}

// A fresh serve process owns no runs, so any row still in "running" is a run
// that died with a previous process. This is how it finds them.
func TestRunningAnalysisRunsReturnsOnlyInFlightRows(t *testing.T) {
	d := openMigrated(t)

	done := fullAnalysisRun("analysis", "finished", nil)
	inFlight := fullAnalysisRun("analysis", "orphan", nil)
	inFlight.State = "running"
	inFlight.FinishedAt = 0
	advice := fullAnalysisRun("advice", "advice", nil)
	advice.State = "running"
	advice.FinishedAt = 0
	for _, r := range []AnalysisRun{done, inFlight, advice} {
		if err := PutAnalysisRun(d, r); err != nil {
			t.Fatal(err)
		}
	}

	got, err := RunningAnalysisRuns(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d running rows, want 2: %+v", len(got), got)
	}
	// Ordered by kind then key, so advice sorts ahead of analysis.
	if got[0].Kind != "advice" || got[1].Key != "orphan" {
		t.Errorf("rows = %+v, want advice first then the orphaned analysis", got)
	}
	if got[1].Model != "claude-opus-5" {
		t.Errorf("model = %q, want the row's other columns to come back too", got[1].Model)
	}
}

func TestRunningAnalysisRunsEmpty(t *testing.T) {
	got, err := RunningAnalysisRuns(openMigrated(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want no rows", got)
	}
}

func TestGetAnalysisRunNotFound(t *testing.T) {
	d := openMigrated(t)
	got, ok, err := GetAnalysisRun(d, "analysis", "nope")
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
