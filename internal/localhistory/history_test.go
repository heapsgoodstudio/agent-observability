package localhistory

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportOpenCodeScopesAndPreservesSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	d, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`CREATE TABLE message (id TEXT, session_id TEXT, data TEXT, time_created INTEGER)`,
		`CREATE TABLE part (id TEXT, session_id TEXT, data TEXT, time_created INTEGER)`,
		`INSERT INTO message VALUES ('m1','chosen','{"role":"user"}',1),('m2','other','{"text":"private-other-session"}',2)`,
		`INSERT INTO part VALUES ('p1','chosen','{"type":"text","text":"chosen evidence"}',3)`,
	} {
		if _, err = d.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	before, _ := os.ReadFile(path)
	destination := filepath.Join(dir, "evidence.jsonl")
	if err := ExportOpenCode(context.Background(), path, "chosen", destination); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(destination)
	if !strings.Contains(string(data), "chosen evidence") || strings.Contains(string(data), "private-other-session") {
		t.Fatalf("scope leak: %s", data)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("source database changed")
	}
	if err := ExportOpenCode(context.Background(), path, "missing", destination); err == nil {
		t.Fatal("empty session accepted")
	}
	absent := filepath.Join(dir, "missing.db")
	if err := ExportOpenCode(context.Background(), absent, "chosen", destination); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatal("created a source database")
	}
}
