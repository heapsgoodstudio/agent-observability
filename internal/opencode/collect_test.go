package opencode

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

/* fixture helpers --------------------------------------------------------

newFixtureDB creates opencode.db under dir with the real schema (abbreviated
per the spec: only the columns this package reads or the tests need to
satisfy NOT NULL constraints), leaving the returned *sql.DB open so a test
can either insert more rows or hold it open to simulate opencode's own
writer connection. */

func newFixtureDB(t *testing.T, dir string) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(dir, "opencode.db")
	d, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	schema := []string{
		`CREATE TABLE session (id text PRIMARY KEY, project_id text NOT NULL, parent_id text, slug text NOT NULL, directory text NOT NULL, title text NOT NULL, version text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, time_archived integer)`,
		`CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL)`,
		`CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL)`,
	}
	for _, s := range schema {
		if _, err := d.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	return d, path
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func insertSession(t *testing.T, d *sql.DB, id, parentID, directory string, created, updated int64) {
	t.Helper()
	_, err := d.Exec(
		`INSERT INTO session (id, project_id, parent_id, slug, directory, title, version, time_created, time_updated, time_archived) VALUES (?, 'proj', ?, 'slug', ?, 'title', 'v1', ?, ?, NULL)`,
		id, nullable(parentID), directory, created, updated)
	if err != nil {
		t.Fatal(err)
	}
}

func insertMessage(t *testing.T, d *sql.DB, id, sessionID string, created int64, data map[string]any) {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		id, sessionID, created, created, string(b))
	if err != nil {
		t.Fatal(err)
	}
}

// insertRawMessage writes data verbatim, unlike insertMessage, so a test can
// exercise a row whose JSON does not parse.
func insertRawMessage(t *testing.T, d *sql.DB, id, sessionID string, created int64, data string) {
	t.Helper()
	_, err := d.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		id, sessionID, created, created, data)
	if err != nil {
		t.Fatal(err)
	}
}

func insertPart(t *testing.T, d *sql.DB, id, messageID, sessionID string, created int64, data map[string]any) {
	t.Helper()
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
		id, messageID, sessionID, created, created, string(b))
	if err != nil {
		t.Fatal(err)
	}
}

// assistantMsg builds an assistant message body. cost is nil to omit the
// "cost" key entirely (a missing cost, per the pricing rule always unknown);
// costPtr(0) keeps the key present with value 0, the case the rest of the
// rule distinguishes.
func assistantMsg(providerID, modelID string, cost *float64, input, output, reasoning, cacheRead, cacheWrite int64, errored bool) map[string]any {
	m := map[string]any{
		"role":       "assistant",
		"providerID": providerID,
		"modelID":    modelID,
		"tokens": map[string]any{
			"input":     input,
			"output":    output,
			"reasoning": reasoning,
			"cache":     map[string]any{"read": cacheRead, "write": cacheWrite},
		},
	}
	if cost != nil {
		m["cost"] = *cost
	}
	if errored {
		m["error"] = map[string]any{"name": "APIError", "data": map[string]any{}}
	}
	return m
}

func userMsg() map[string]any { return map[string]any{"role": "user"} }

func toolPart(tool string) map[string]any {
	return map[string]any{"type": "tool", "tool": tool, "callID": "c-" + tool, "state": map[string]any{"status": "completed"}}
}

func costPtr(v float64) *float64 { return &v }

/* tests ------------------------------------------------------------------ */

// 1. Basic: one session, two assistant messages (fireworks-ai priced) plus
// one user message plus two tool parts.
func TestBasicSession(t *testing.T) {
	dir := t.TempDir()
	d, _ := newFixtureDB(t, dir)

	insertSession(t, d, "s1", "", "/Users/x/project/repo", 1000000, 1005000)
	insertMessage(t, d, "u1", "s1", 1001000, userMsg())
	insertMessage(t, d, "a1", "s1", 1002000,
		assistantMsg("fireworks-ai", "accounts/fireworks/models/glm-5p3-flash", costPtr(0.00109515), 7261, 3, 9, 0, 0, false))
	insertMessage(t, d, "a2", "s1", 1010000,
		assistantMsg("fireworks-ai", "accounts/fireworks/models/glm-5p3-flash", costPtr(0.001), 1000, 10, 5, 100, 50, false))
	insertPart(t, d, "p1", "a1", "s1", 1002500, toolPart("read"))
	insertPart(t, d, "p2", "a1", "s1", 1002600, toolPart("write"))

	r, err := Collect(context.Background(), Options{Dir: dir, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sessions) != 1 {
		t.Fatalf("sessions=%d", len(r.Sessions))
	}
	row := r.Sessions[0]

	if row.ID != "opencode:p:s1" || row.NativeID != "s1" || row.Provider != "opencode" || row.Profile != "p" {
		t.Fatalf("identity: %+v", row)
	}
	if row.Requests != 2 {
		t.Fatalf("requests: %+v", row)
	}
	// output includes reasoning: (3+9) + (10+5) = 27.
	if row.Tokens.Input != 8261 || row.Tokens.Output != 27 || row.Tokens.CacheRead != 100 || row.Tokens.CacheCreation != 50 || row.ReasoningTokens != 14 {
		t.Fatalf("tokens: %+v", row.Tokens)
	}
	if len(row.Trajectory) != 2 || row.Trajectory[0][2] != 7261 || row.Trajectory[1][2] != 1150 {
		t.Fatalf("context must include cache writes and preserve decreases: %+v", row.Trajectory)
	}
	if !row.CostKnown || row.CostUSD != round6(0.00109515+0.001) {
		t.Fatalf("cost: known=%v usd=%v", row.CostKnown, row.CostUSD)
	}
	if row.ToolCalls != 2 || row.Tools["read"] != 1 || row.Tools["write"] != 1 {
		t.Fatalf("tools: calls=%d tools=%+v", row.ToolCalls, row.Tools)
	}
	if row.UserTurns != 1 {
		t.Fatalf("user turns: %d", row.UserTurns)
	}
	if row.Start != 1000 {
		t.Fatalf("start: %d", row.Start)
	}
	// End is max(time_updated, latest message time_created)/1000: the
	// session's own time_updated (1005) is older than the last message (1010).
	if row.End != 1010 {
		t.Fatalf("end: %d", row.End)
	}
	if row.DurationMS != 10000 {
		t.Fatalf("duration: %d", row.DurationMS)
	}
	if row.Workspace != "project/repo" {
		t.Fatalf("workspace: %q", row.Workspace)
	}
	if len(row.Models) != 1 || row.Models[0].Model != "fireworks/glm-5.3-flash" {
		t.Fatalf("models: %+v", row.Models)
	}
	if !row.UsageKnown || !row.ToolsKnown || !row.ErrorsKnown || !row.DurationKnown || !row.HasTranscript {
		t.Fatalf("known flags: %+v", row)
	}
}

// 2. Pricing rule: an unpriced provider's cost-0 requests are always known
// and free; a priced provider's cost-0 request is unknown only when it used
// tokens.
func TestPricingRule(t *testing.T) {
	dir := t.TempDir()
	d, _ := newFixtureDB(t, dir)

	// lms never prices anything: known-free regardless of tokens.
	insertSession(t, d, "lms-sess", "", "/x/lms", 1000000, 1000000)
	insertMessage(t, d, "lm1", "lms-sess", 1000000, assistantMsg("lms", "some-model", costPtr(0), 40, 4, 0, 0, 0, false))

	// baseten prices at least once, via this session's first message.
	insertSession(t, d, "baseten-sess", "", "/x/bt", 1000000, 1000000)
	insertMessage(t, d, "bt1", "baseten-sess", 1000000,
		assistantMsg("baseten", "meta-llama/Llama-3-70b", costPtr(0.02), 100, 10, 0, 0, 0, false))
	insertMessage(t, d, "bt2", "baseten-sess", 1001000,
		assistantMsg("baseten", "meta-llama/Llama-3-70b", costPtr(0), 50, 5, 0, 0, 0, false))

	// A baseten cost-0 request that used no tokens at all, on a different
	// model so its bucket is not muddied by bt1/bt2's unknown-cost bucket.
	insertSession(t, d, "baseten-zero-sess", "", "/x/bt0", 1000000, 1000000)
	insertMessage(t, d, "bt3", "baseten-zero-sess", 1000000,
		assistantMsg("baseten", "other-model", costPtr(0), 0, 0, 0, 0, 0, true))

	r, err := Collect(context.Background(), Options{Dir: dir, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}

	rowsByID := map[string]int{}
	for i, row := range r.Sessions {
		rowsByID[row.NativeID] = i
	}

	lmsRow := r.Sessions[rowsByID["lms-sess"]]
	if !lmsRow.CostKnown || lmsRow.CostUSD != 0 {
		t.Fatalf("lms session: known=%v usd=%v", lmsRow.CostKnown, lmsRow.CostUSD)
	}

	btRow := r.Sessions[rowsByID["baseten-sess"]]
	if btRow.CostKnown {
		t.Fatalf("baseten session expected unknown: %+v", btRow)
	}
	if btRow.CostUSD != round6(0.02) {
		t.Fatalf("baseten session cost subtotal: %v", btRow.CostUSD)
	}

	btZeroRow := r.Sessions[rowsByID["baseten-zero-sess"]]
	if !btZeroRow.CostKnown || btZeroRow.CostUSD != 0 {
		t.Fatalf("baseten zero-token session: known=%v usd=%v", btZeroRow.CostKnown, btZeroRow.CostUSD)
	}
	if btZeroRow.ErrorCount != 1 {
		t.Fatalf("baseten zero-token session error count: %d", btZeroRow.ErrorCount)
	}

	buckets := map[[2]string]*struct {
		known bool
		usd   float64
	}{}
	for i := range r.Usage {
		b := r.Usage[i]
		k := [2]string{b.Provider, b.Model}
		buckets[k] = &struct {
			known bool
			usd   float64
		}{b.CostKnown, b.CostUSD}
	}

	if b := buckets[[2]string{"lms", "some-model"}]; b == nil || !b.known || b.usd != 0 {
		t.Fatalf("lms bucket: %+v", b)
	}
	if b := buckets[[2]string{"baseten", "llama-3-70b"}]; b == nil || b.known || b.usd != round6(0.02) {
		t.Fatalf("baseten llama bucket: %+v", b)
	}
	if b := buckets[[2]string{"baseten", "other-model"}]; b == nil || !b.known || b.usd != 0 {
		t.Fatalf("baseten other-model bucket: %+v", b)
	}
}

// 3. Subagent: a child session's parent_id resolves to the parent's row ID.
func TestSubagentParentID(t *testing.T) {
	dir := t.TempDir()
	d, _ := newFixtureDB(t, dir)

	insertSession(t, d, "parent", "", "/x/y", 1000000, 1000000)
	insertSession(t, d, "child", "parent", "/x/y", 1001000, 1001000)

	r, err := Collect(context.Background(), Options{Dir: dir, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	var parentID, childParentID string
	for _, row := range r.Sessions {
		switch row.NativeID {
		case "parent":
			parentID = row.ID
		case "child":
			childParentID = row.ParentID
		}
	}
	if parentID == "" || childParentID != parentID {
		t.Fatalf("child parent id: got %q want %q", childParentID, parentID)
	}
}

// 4. An errored assistant message still counts as a request and increments
// ErrorCount.
func TestErroredMessageCountsAsRequest(t *testing.T) {
	dir := t.TempDir()
	d, _ := newFixtureDB(t, dir)

	insertSession(t, d, "s1", "", "/x/y", 1000000, 1000000)
	insertMessage(t, d, "a1", "s1", 1000000, assistantMsg("baseten", "model-a", costPtr(0.001), 10, 1, 0, 0, 0, true))

	r, err := Collect(context.Background(), Options{Dir: dir, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	row := r.Sessions[0]
	if row.Requests != 1 {
		t.Fatalf("requests: %d", row.Requests)
	}
	if row.ErrorCount != 1 {
		t.Fatalf("error count: %d", row.ErrorCount)
	}
}

// 5. A malformed message row is skipped, with one warning and Complete=false,
// while the rest of the session collects normally.
func TestMalformedMessageSkipped(t *testing.T) {
	dir := t.TempDir()
	d, _ := newFixtureDB(t, dir)

	insertSession(t, d, "s1", "", "/x/y", 1000000, 1000000)
	insertMessage(t, d, "a1", "s1", 1000000, assistantMsg("baseten", "model-a", costPtr(0.001), 10, 1, 0, 0, 0, false))
	insertRawMessage(t, d, "broken", "s1", 1001000, `{"role":"assistant",`)

	r, err := Collect(context.Background(), Options{Dir: dir, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Complete {
		t.Fatal("expected Complete=false")
	}
	if len(r.Warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %v", r.Warnings)
	}
	if r.Sessions[0].Requests != 1 {
		t.Fatalf("requests: %+v", r.Sessions[0])
	}
}

// 6. Collect never writes: the database's bytes and mtime are unchanged
// afterward, including while another connection holds it open in WAL mode.
func TestReadOnlyLeavesTheDatabaseUntouched(t *testing.T) {
	dir := t.TempDir()
	d, path := newFixtureDB(t, dir)

	insertSession(t, d, "s1", "", "/x/y", 1000000, 1000000)
	insertMessage(t, d, "a1", "s1", 1000000, assistantMsg("baseten", "model-a", costPtr(0.001), 10, 1, 0, 0, 0, false))
	if _, err := d.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// d stays open in WAL mode for the whole call, standing in for
	// opencode's own writer connection.
	if _, err := Collect(context.Background(), Options{Dir: dir, ProfileID: "p"}); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("database bytes changed after Collect")
	}
	if !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf("database mtime changed: %v -> %v", beforeInfo.ModTime(), afterInfo.ModTime())
	}
}

// 7. Usage buckets aggregate within a local day and split across days.
func TestUsageBucketsAggregateByLocalDay(t *testing.T) {
	dir := t.TempDir()
	d, _ := newFixtureDB(t, dir)

	base := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	sameDay := base.Add(2 * time.Hour)
	otherDay := base.Add(48 * time.Hour)

	insertSession(t, d, "s1", "", "/x/y", base.UnixMilli(), base.UnixMilli())
	insertMessage(t, d, "a1", "s1", base.UnixMilli(), assistantMsg("baseten", "model-a", costPtr(0.01), 100, 10, 0, 0, 0, false))

	insertSession(t, d, "s2", "", "/x/y", sameDay.UnixMilli(), sameDay.UnixMilli())
	insertMessage(t, d, "a2", "s2", sameDay.UnixMilli(), assistantMsg("baseten", "model-a", costPtr(0.005), 50, 5, 0, 0, 0, false))

	insertSession(t, d, "s3", "", "/x/y", otherDay.UnixMilli(), otherDay.UnixMilli())
	insertMessage(t, d, "a3", "s3", otherDay.UnixMilli(), assistantMsg("baseten", "model-a", costPtr(0.002), 20, 2, 0, 0, 0, false))

	r, err := Collect(context.Background(), Options{Dir: dir, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}

	dayA := base.In(time.Local).Format("2006-01-02")
	dayC := otherDay.In(time.Local).Format("2006-01-02")
	if len(r.Usage) != 2 {
		t.Fatalf("buckets: %+v", r.Usage)
	}
	var gotA, gotC *int64
	var costA, costC *float64
	for i := range r.Usage {
		switch r.Usage[i].Day {
		case dayA:
			req := r.Usage[i].Requests
			cost := r.Usage[i].CostUSD
			gotA, costA = &req, &cost
		case dayC:
			req := r.Usage[i].Requests
			cost := r.Usage[i].CostUSD
			gotC, costC = &req, &cost
		}
	}
	if gotA == nil || *gotA != 2 {
		t.Fatalf("day A requests: %+v", r.Usage)
	}
	if costA == nil || *costA != round6(0.01+0.005) {
		t.Fatalf("day A cost: %+v", r.Usage)
	}
	if gotC == nil || *gotC != 1 {
		t.Fatalf("day C requests: %+v", r.Usage)
	}
	if costC == nil || *costC != round6(0.002) {
		t.Fatalf("day C cost: %+v", r.Usage)
	}
}

// 8. Missing database and empty identity fields are refused.
func TestCollectErrors(t *testing.T) {
	if _, err := Collect(context.Background(), Options{Dir: t.TempDir(), ProfileID: "p"}); err == nil {
		t.Fatal("expected error for a directory with no opencode.db")
	}
	if _, err := Collect(context.Background(), Options{Dir: "", ProfileID: "p"}); err == nil {
		t.Fatal("expected error for empty dir")
	}

	dir := t.TempDir()
	newFixtureDB(t, dir)
	if _, err := Collect(context.Background(), Options{Dir: dir, ProfileID: ""}); err == nil {
		t.Fatal("expected error for empty profile id")
	}
}

// 9. The Collector's cache returns an identical Result until opencode.db (or
// its -wal file) changes, at which point a new request is picked up.
func TestCacheHoldsUntilFilesChange(t *testing.T) {
	dir := t.TempDir()
	d, path := newFixtureDB(t, dir)

	insertSession(t, d, "s1", "", "/x/y", 1000000, 1000000)
	insertMessage(t, d, "a1", "s1", 1000000, assistantMsg("baseten", "model-a", costPtr(0.01), 10, 1, 0, 0, 0, false))

	c := NewCollector()
	r1, err := c.Collect(context.Background(), Options{Dir: dir, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := c.Collect(context.Background(), Options{Dir: dir, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r1, r2) {
		t.Fatalf("expected identical cached result:\n%+v\nvs\n%+v", r1, r2)
	}
	if r1.Sessions[0].Requests != 1 {
		t.Fatalf("precondition: requests=%d", r1.Sessions[0].Requests)
	}

	insertMessage(t, d, "a2", "s1", 1001000, assistantMsg("baseten", "model-a", costPtr(0.02), 20, 2, 0, 0, 0, false))
	// A change may land within the same mtime second; bump it explicitly so
	// the cache is deterministically invalidated rather than relying on
	// wall-clock drift or a file-size change the insert may not produce.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime().Add(time.Second), info.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	r3, err := c.Collect(context.Background(), Options{Dir: dir, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if r3.Sessions[0].Requests != 2 {
		t.Fatalf("expected the new request to appear after the file changed: %+v", r3.Sessions[0])
	}
}
