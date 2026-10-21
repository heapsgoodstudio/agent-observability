package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// migrationCount reports how many migration files are embedded. The three
// migration tests count against it rather than against a literal, so adding a
// migration does not mean editing an unrelated assertion in three files: a
// bumped literal is indistinguishable from a test that stopped checking
// anything, and these are the tests that catch a migration failing to apply.
func migrationCount(t *testing.T) int {
	t.Helper()
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestOpenMigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-obs.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var mode string
	if err := d.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}

	n, err := Migrate(d)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh db applies every migration in this first call.
	// TestMigrateAppliesSessionsSchema in sessions_test.go and
	// TestMigrateAppliesAnalysisRunsSchema in analysis_test.go cover the more
	// realistic case of a later migration landing on a db that already has
	// the earlier ones applied.
	if want := migrationCount(t); n != want {
		t.Fatalf("applied = %d, want %d", n, want)
	}
	n, err = Migrate(d)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("second run applied = %d, want 0", n)
	}

	if _, err := d.Exec(`INSERT INTO meta(key, value) VALUES('schema', 'v2')`); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := d.QueryRow(`SELECT value FROM meta WHERE key = 'schema'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != "v2" {
		t.Fatalf("meta value = %q", v)
	}
}

func TestPragmasApplyToEveryConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-obs.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	c1, err := d.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := d.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	for i, c := range []*sql.Conn{c1, c2} {
		var timeout int
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		if timeout != 5000 {
			t.Fatalf("conn %d busy_timeout = %d, want 5000", i, timeout)
		}
		var fk int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Fatalf("conn %d foreign_keys = %d, want 1", i, fk)
		}
	}
}
