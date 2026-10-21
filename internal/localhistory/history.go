// Package localhistory provides disposable transcript exports for analysis.
package localhistory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
)

func openDB(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	return sql.Open("sqlite", u.String())
}

// ExportOpenCode creates a bounded, chronological JSONL transcript for analysis.
// It is a disposable evidence file, never another authority for session stats.
func ExportOpenCode(ctx context.Context, path, nativeID, destination string) error {
	d, err := openDB(path)
	if err != nil {
		return err
	}
	defer d.Close()
	rows, err := d.QueryContext(ctx, `SELECT kind,id,data FROM (SELECT 'message' kind,id,data,time_created FROM message WHERE session_id=? UNION ALL SELECT 'part' kind,id,data,time_created FROM part WHERE session_id=?) ORDER BY time_created,id`, nativeID, nativeID)
	if err != nil {
		return err
	}
	defer rows.Close()
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	total := 0
	count := 0
	for rows.Next() {
		var kind, id string
		var raw []byte
		if err := rows.Scan(&kind, &id, &raw); err != nil {
			return err
		}
		count++
		total += len(raw)
		if total > 64<<20 {
			return fmt.Errorf("OpenCode transcript exceeds 64 MiB evidence limit")
		}
		if err := enc.Encode(map[string]any{"type": kind, "id": id, "data": json.RawMessage(raw)}); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("OpenCode session has no local transcript events")
	}
	return nil
}
