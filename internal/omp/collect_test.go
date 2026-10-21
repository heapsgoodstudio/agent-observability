package omp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/payg"
)

/* fixture helpers ------------------------------------------------------- */

func mainPath(home, project, stem string) string {
	return filepath.Join(home, "sessions", project, stem+".jsonl")
}

func nestedPath(home, project, stem, sub string) string {
	return filepath.Join(home, "sessions", project, stem, sub+".jsonl")
}

// writeLines marshals each entry as one JSON line and writes them to path,
// creating any parent directories a nested transcript needs.
func writeLines(t *testing.T, path string, entries ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func header(id, ts, cwd string) map[string]any {
	return map[string]any{"type": "session", "id": id, "timestamp": ts, "cwd": cwd}
}

func headerFork(id, ts, cwd, parentSession string) map[string]any {
	m := header(id, ts, cwd)
	m["parentSession"] = parentSession
	return m
}

func userMsg(id, parentID, ts string, synthetic *bool) map[string]any {
	msg := map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}}
	if synthetic != nil {
		msg["synthetic"] = *synthetic
	}
	return map[string]any{"type": "message", "id": id, "parentId": parentID, "timestamp": ts, "message": msg}
}

func toolResultMsg(id, parentID, ts string) map[string]any {
	return map[string]any{"type": "message", "id": id, "parentId": parentID, "timestamp": ts, "message": map[string]any{"role": "toolResult", "isError": false}}
}

type asstOpt func(map[string]any)

func withResponseID(id string) asstOpt { return func(m map[string]any) { m["responseId"] = id } }

func withStopReason(r string) asstOpt { return func(m map[string]any) { m["stopReason"] = r } }

func withMessageTimestampMS(ms int64) asstOpt { return func(m map[string]any) { m["timestamp"] = ms } }

func withToolCalls(names ...string) asstOpt {
	return func(m map[string]any) {
		content := []any{}
		for _, n := range names {
			content = append(content, map[string]any{"type": "toolCall", "id": "tc-" + n, "name": n})
		}
		m["content"] = content
	}
}

// assistant builds an assistant message entry. usage may be nil to build an
// assistant message with no usage object at all (not a counted record).
func assistant(id, parentID, ts, provider, model string, usage map[string]any, opts ...asstOpt) map[string]any {
	msg := map[string]any{
		"role":       "assistant",
		"provider":   provider,
		"model":      model,
		"stopReason": "stop",
		"content":    []any{},
	}
	if usage != nil {
		msg["usage"] = usage
	}
	for _, o := range opts {
		o(msg)
	}
	return map[string]any{"type": "message", "id": id, "parentId": parentID, "timestamp": ts, "message": msg}
}

// usage builds a usage object. cost is nil to omit the "cost" key entirely
// (an unpriced request), matching docs/omp-provider.md's "missing cost ->
// unknown" rule.
func usage(input, output, cacheRead, cacheWrite, reasoning int, cost *float64) map[string]any {
	u := map[string]any{
		"input": input, "output": output, "cacheRead": cacheRead, "cacheWrite": cacheWrite,
		"totalTokens": input + output + cacheRead + cacheWrite, "reasoningTokens": reasoning,
	}
	if cost != nil {
		u["cost"] = map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": *cost}
	}
	return u
}

func modelUsageEntry(id, ts, provider, model string, u map[string]any) map[string]any {
	return map[string]any{"type": "model_usage", "id": id, "timestamp": ts, "provider": provider, "model": model, "usage": u}
}

func costPtr(v float64) *float64 { return &v }

func boolPtr(v bool) *bool { return &v }

func homeDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

/* tests ------------------------------------------------------------------ */

// 1. Basic main session.
func TestBasicMainSession(t *testing.T) {
	home := homeDir(t)
	path := mainPath(home, "myproj", "s1")
	writeLines(t, path,
		header("s1", "2026-09-01T10:00:00.000Z", "/Users/x/project/repo"),
		userMsg("u1", "s1", "2026-09-01T10:00:05.000Z", nil),
		assistant("a1", "u1", "2026-09-01T10:00:06.000Z", "baseten", "zai-org/GLM-5.3",
			usage(1000, 50, 200, 50, 5, costPtr(0.01)),
			withResponseID("r1"), withToolCalls("read", "glob")),
		toolResultMsg("tr1", "a1", "2026-09-01T10:00:07.000Z"),
		// A synthetic follow-up user turn must not count.
		userMsg("u2", "tr1", "2026-09-01T10:00:08.000Z", boolPtr(true)),
		assistant("a2", "u2", "2026-09-01T10:00:10.000Z", "baseten", "zai-org/GLM-5.3",
			usage(500, 20, 100, 0, 2, costPtr(0.005)),
			withResponseID("r2")),
	)
	r, err := Collect(context.Background(), Options{Home: home, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sessions) != 1 {
		t.Fatalf("sessions=%d", len(r.Sessions))
	}
	row := r.Sessions[0]
	if row.ID != "omp:p:s1" || row.NativeID != "s1" || row.Provider != "omp" || row.Profile != "p" {
		t.Fatalf("identity: %+v", row)
	}
	if row.Requests != 2 || row.Tokens.Input != 1500 || row.Tokens.Output != 70 || row.Tokens.CacheRead != 300 || row.Tokens.CacheCreation != 50 || row.ReasoningTokens != 7 {
		t.Fatalf("tokens: %+v", row)
	}
	if len(row.Trajectory) != 2 || row.Trajectory[0][2] != 1250 || row.Trajectory[1][2] != 600 {
		t.Fatalf("context must include cache writes and preserve decreases: %+v", row.Trajectory)
	}
	if !row.CostKnown || row.CostUSD != 0.015 {
		t.Fatalf("cost: %+v", row)
	}
	if row.ToolCalls != 2 || row.Tools["read"] != 1 || row.Tools["glob"] != 1 {
		t.Fatalf("tools: %+v", row)
	}
	if row.UserTurns != 1 {
		t.Fatalf("user turns: %+v", row)
	}
	wantStart, _ := parseISO("2026-09-01T10:00:00.000Z")
	wantEnd, _ := parseISO("2026-09-01T10:00:10.000Z")
	if row.Start != wantStart || row.End != wantEnd {
		t.Fatalf("start/end: got %d/%d want %d/%d", row.Start, row.End, wantStart, wantEnd)
	}
	if row.Workspace != "project/repo" {
		t.Fatalf("workspace: %q", row.Workspace)
	}
	if len(row.Models) != 1 || row.Models[0].Model != "baseten/glm-5.3" || row.Models[0].CostUSD != 0.015 {
		t.Fatalf("models: %+v", row.Models)
	}
	if !row.UsageKnown || !row.ToolsKnown || !row.ErrorsKnown || !row.DurationKnown || !row.HasTranscript {
		t.Fatalf("known flags: %+v", row)
	}
}

// 2. Unknown cost.
func TestUnknownCost(t *testing.T) {
	home := homeDir(t)
	path := mainPath(home, "p1", "s1")
	ts := "2026-09-02T10:00:00.000Z"
	writeLines(t, path,
		header("s1", ts, "/Users/x/proj"),
		assistant("a1", "s1", "2026-09-02T10:00:01.000Z", "baseten", "zai-org/GLM-5.3", usage(100, 10, 0, 0, 0, costPtr(0.002)), withResponseID("r1")),
		assistant("a2", "a1", "2026-09-02T10:00:02.000Z", "baseten", "zai-org/GLM-5.3", usage(200, 20, 0, 0, 0, nil), withResponseID("r2")),
	)
	r, err := Collect(context.Background(), Options{Home: home, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	row := r.Sessions[0]
	if row.CostKnown {
		t.Fatalf("expected unknown cost: %+v", row)
	}
	if row.CostUSD != 0.002 {
		t.Fatalf("cost subtotal: %v", row.CostUSD)
	}
	if len(r.Usage) != 1 || r.Usage[0].CostKnown {
		t.Fatalf("bucket cost known: %+v", r.Usage)
	}
	if r.Usage[0].CostUSD != 0.002 {
		t.Fatalf("bucket cost: %+v", r.Usage[0])
	}
}

// 3. Free local model.
func TestFreeLocalModel(t *testing.T) {
	home := homeDir(t)
	path := mainPath(home, "p1", "s1")
	writeLines(t, path,
		header("s1", "2026-09-03T10:00:00.000Z", "/Users/x/proj"),
		assistant("a1", "s1", "2026-09-03T10:00:01.000Z", "lm-studio", "local-model", usage(50, 5, 0, 0, 0, costPtr(0)), withResponseID("r1")),
	)
	r, err := Collect(context.Background(), Options{Home: home, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	row := r.Sessions[0]
	if !row.CostKnown || row.CostUSD != 0 {
		t.Fatalf("free local model: %+v", row)
	}
}

// 4. Fork dedup.
func TestForkDedup(t *testing.T) {
	home := homeDir(t)
	pathA := mainPath(home, "p1", "a")
	pathB := mainPath(home, "p1", "b")
	writeLines(t, pathA,
		header("sid-a", "2026-09-04T09:00:00.000Z", "/x/proj"),
		assistant("a1", "sid-a", "2026-09-04T09:00:01.000Z", "baseten", "zai-org/GLM-5.3", usage(100, 10, 0, 0, 0, costPtr(0.01)), withResponseID("shared-1")),
		assistant("a2", "a1", "2026-09-04T09:00:02.000Z", "baseten", "zai-org/GLM-5.3", usage(200, 20, 0, 0, 0, costPtr(0.02)), withResponseID("shared-2")),
	)
	writeLines(t, pathB,
		headerFork("sid-b", "2026-09-04T09:30:00.000Z", "/x/proj", pathA),
		assistant("b1", "sid-b", "2026-09-04T09:00:01.000Z", "baseten", "zai-org/GLM-5.3", usage(100, 10, 0, 0, 0, costPtr(0.01)), withResponseID("shared-1")),
		assistant("b2", "b1", "2026-09-04T09:00:02.000Z", "baseten", "zai-org/GLM-5.3", usage(200, 20, 0, 0, 0, costPtr(0.02)), withResponseID("shared-2")),
		assistant("b3", "b2", "2026-09-04T09:30:01.000Z", "baseten", "zai-org/GLM-5.3", usage(50, 5, 0, 0, 0, costPtr(0.005)), withResponseID("new-resp")),
	)
	r, err := Collect(context.Background(), Options{Home: home, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	a, b := -1, -1
	for i, row := range r.Sessions {
		if row.NativeID == "sid-a" {
			a = i
		}
		if row.NativeID == "sid-b" {
			b = i
		}
	}
	if a == -1 || b == -1 {
		t.Fatalf("missing rows: %+v", r.Sessions)
	}
	if r.Sessions[a].Requests != 2 {
		t.Fatalf("A requests: %+v", r.Sessions[a])
	}
	if r.Sessions[b].Requests != 1 {
		t.Fatalf("B requests: %+v", r.Sessions[b])
	}
	if r.Sessions[b].ParentID != r.Sessions[a].ID {
		t.Fatalf("B parent: got %q want %q", r.Sessions[b].ParentID, r.Sessions[a].ID)
	}
}

// 5. Nested subagent file without its own header.
func TestNestedSubagentWithoutHeader(t *testing.T) {
	home := homeDir(t)
	main := mainPath(home, "p1", "main")
	sub := nestedPath(home, "p1", "main", "sub1")
	writeLines(t, main,
		header("parent-id", "2026-09-05T09:00:00.000Z", "/x/y"),
		assistant("a1", "parent-id", "2026-09-05T09:00:01.000Z", "baseten", "zai-org/GLM-5.3", usage(10, 1, 0, 0, 0, costPtr(0.001)), withResponseID("main-1")),
	)
	writeLines(t, sub,
		assistant("s1", "", "2026-09-05T09:05:00.000Z", "baseten", "zai-org/GLM-5.3", usage(20, 2, 0, 0, 0, costPtr(0.002)), withResponseID("sub-1")),
	)
	r, err := Collect(context.Background(), Options{Home: home, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	var subRow, parentRow int = -1, -1
	for i, row := range r.Sessions {
		if row.NativeID == "parent-id/sub1" {
			subRow = i
		}
		if row.NativeID == "parent-id" {
			parentRow = i
		}
	}
	if subRow == -1 || parentRow == -1 {
		t.Fatalf("rows: %+v", r.Sessions)
	}
	wantID := "omp:p:parent-id/sub1"
	if r.Sessions[subRow].ID != wantID {
		t.Fatalf("nested id: %q want %q", r.Sessions[subRow].ID, wantID)
	}
	if r.Sessions[subRow].ParentID != r.Sessions[parentRow].ID {
		t.Fatalf("nested parent: %q want %q", r.Sessions[subRow].ParentID, r.Sessions[parentRow].ID)
	}
	if r.Sessions[subRow].Project != "/x/y" {
		t.Fatalf("nested cwd inherit: %+v", r.Sessions[subRow])
	}
}

// 6. model_usage entry counted.
func TestModelUsageEntryCounted(t *testing.T) {
	home := homeDir(t)
	path := mainPath(home, "p1", "s1")
	writeLines(t, path,
		header("s1", "2026-09-06T09:00:00.000Z", "/x/y"),
		modelUsageEntry("mu1", "2026-09-06T09:00:05.000Z", "fireworks-ai", "accounts/fireworks/models/glm-5p3-flash", usage(30, 3, 0, 0, 0, costPtr(0.0035))),
	)
	r, err := Collect(context.Background(), Options{Home: home, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	row := r.Sessions[0]
	if row.Requests != 1 || row.Tokens.Input != 30 || row.Tokens.Output != 3 || !row.CostKnown || row.CostUSD != 0.0035 {
		t.Fatalf("model_usage row: %+v", row)
	}
	if len(row.Models) != 1 || row.Models[0].Model != "fireworks/glm-5.3-flash" {
		t.Fatalf("model_usage models: %+v", row.Models)
	}
}

// 7. Malformed complete line vs. trailing partial line.
func TestMalformedAndPartialLines(t *testing.T) {
	home := homeDir(t)
	path := mainPath(home, "p1", "s1")
	writeLines(t, path,
		header("s1", "2026-09-07T09:00:00.000Z", "/x/y"),
		assistant("a1", "s1", "2026-09-07T09:00:01.000Z", "baseten", "zai-org/GLM-5.3", usage(10, 1, 0, 0, 0, costPtr(0.001)), withResponseID("r1")),
	)
	// Append one malformed complete line, then a trailing partial (no newline) line.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","id":"broken",` + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","id":"partial","timestamp":"2026-09-07T09`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Collect(context.Background(), Options{Home: home, ProfileID: "p"})
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

// 8. See canonical_test.go for the canonical name table.

// 9. Usage buckets aggregate same-day and split across days.
func TestUsageBucketsAggregateByLocalDay(t *testing.T) {
	home := homeDir(t)
	base := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	sameDay := base.Add(2 * time.Hour)
	otherDay := base.Add(48 * time.Hour)

	writeLines(t, mainPath(home, "p1", "s1"),
		header("s1", base.Format(time.RFC3339), "/x/y"),
		assistant("a1", "s1", base.Format(time.RFC3339), "baseten", "zai-org/GLM-5.3", usage(100, 10, 0, 0, 0, costPtr(0.01)), withResponseID("r1")),
	)
	writeLines(t, mainPath(home, "p1", "s2"),
		header("s2", sameDay.Format(time.RFC3339), "/x/y"),
		assistant("a1", "s2", sameDay.Format(time.RFC3339), "baseten", "zai-org/GLM-5.3", usage(50, 5, 0, 0, 0, costPtr(0.005)), withResponseID("r2")),
	)
	writeLines(t, mainPath(home, "p1", "s3"),
		header("s3", otherDay.Format(time.RFC3339), "/x/y"),
		assistant("a1", "s3", otherDay.Format(time.RFC3339), "baseten", "zai-org/GLM-5.3", usage(20, 2, 0, 0, 0, costPtr(0.002)), withResponseID("r3")),
	)
	r, err := Collect(context.Background(), Options{Home: home, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	dayA := base.In(time.Local).Format("2006-01-02")
	dayC := otherDay.In(time.Local).Format("2006-01-02")
	if len(r.Usage) != 2 {
		t.Fatalf("buckets: %+v", r.Usage)
	}
	var gotA, gotC *payg.Bucket
	for i := range r.Usage {
		switch r.Usage[i].Day {
		case dayA:
			gotA = &r.Usage[i]
		case dayC:
			gotC = &r.Usage[i]
		}
	}
	if gotA == nil || gotC == nil {
		t.Fatalf("days: got %+v want %q,%q", r.Usage, dayA, dayC)
	}
	if gotA.Requests != 2 || gotA.Input != 150 || gotA.Output != 15 {
		t.Fatalf("day A bucket: %+v", gotA)
	}
	if gotC.Requests != 1 || gotC.Input != 20 {
		t.Fatalf("day C bucket: %+v", gotC)
	}
}

// 10. Cache: unchanged files are not reparsed; only a changed file updates.
func TestCacheUnchangedFilesReturnIdenticalResult(t *testing.T) {
	home := homeDir(t)
	path1 := mainPath(home, "p1", "s1")
	path2 := mainPath(home, "p1", "s2")
	writeLines(t, path1,
		header("s1", "2026-09-11T09:00:00.000Z", "/x/y"),
		assistant("a1", "s1", "2026-09-11T09:00:01.000Z", "baseten", "zai-org/GLM-5.3", usage(10, 1, 0, 0, 0, costPtr(0.001)), withResponseID("r1")),
	)
	writeLines(t, path2,
		header("s2", "2026-09-11T09:10:00.000Z", "/x/y"),
		assistant("a1", "s2", "2026-09-11T09:10:01.000Z", "baseten", "zai-org/GLM-5.3", usage(10, 1, 0, 0, 0, costPtr(0.001)), withResponseID("r2")),
	)
	c := NewCollector()
	opts := Options{Home: home, ProfileID: "p"}
	r1, err := c.Collect(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := c.Collect(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Sessions) != 2 || len(r2.Sessions) != 2 {
		t.Fatalf("sessions: %d %d", len(r1.Sessions), len(r2.Sessions))
	}
	for i := range r1.Sessions {
		if r1.Sessions[i].Requests != r2.Sessions[i].Requests || r1.Sessions[i].CostUSD != r2.Sessions[i].CostUSD || r1.Sessions[i].ID != r2.Sessions[i].ID {
			t.Fatalf("unchanged collect diverged: %+v vs %+v", r1.Sessions[i], r2.Sessions[i])
		}
	}

	f, err := os.OpenFile(path2, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(assistant("a2", "a1", "2026-09-11T09:10:02.000Z", "baseten", "zai-org/GLM-5.3", usage(5, 1, 0, 0, 0, costPtr(0.0005)), withResponseID("r3")))
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	r3, err := c.Collect(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	var got1, got2 int
	for _, row := range r3.Sessions {
		if row.NativeID == "s1" {
			got1 = row.Requests
		}
		if row.NativeID == "s2" {
			got2 = row.Requests
		}
	}
	if got1 != 1 {
		t.Fatalf("unchanged file s1 changed: got %d requests", got1)
	}
	if got2 != 2 {
		t.Fatalf("changed file s2 not updated: got %d requests", got2)
	}
}

// ErrorCount, and message.timestamp taking priority over the entry's own
// ISO timestamp for a request's day bucket.
func TestErrorCountAndMessageTimestampPriority(t *testing.T) {
	home := homeDir(t)
	path := mainPath(home, "p1", "s1")
	entryDay := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	msgDay := entryDay.Add(72 * time.Hour)
	writeLines(t, path,
		header("s1", entryDay.Format(time.RFC3339), "/x/y"),
		assistant("a1", "s1", entryDay.Format(time.RFC3339), "baseten", "zai-org/GLM-5.3",
			usage(0, 0, 0, 0, 0, costPtr(0)),
			withStopReason("error"), withMessageTimestampMS(msgDay.UnixMilli())),
	)
	r, err := Collect(context.Background(), Options{Home: home, ProfileID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	row := r.Sessions[0]
	if row.ErrorCount != 1 {
		t.Fatalf("error count: %+v", row)
	}
	if row.Requests != 1 {
		t.Fatalf("requests: %+v", row)
	}
	wantDay := msgDay.In(time.Local).Format("2006-01-02")
	if len(r.Usage) != 1 || r.Usage[0].Day != wantDay {
		t.Fatalf("bucket day: got %+v want %q", r.Usage, wantDay)
	}
}

// 11. Missing Home/sessions and empty ProfileID both error.
func TestCollectErrors(t *testing.T) {
	if _, err := Collect(context.Background(), Options{Home: t.TempDir(), ProfileID: "p"}); err == nil {
		t.Fatal("expected error for missing sessions dir")
	}
	home := homeDir(t)
	if _, err := Collect(context.Background(), Options{Home: home, ProfileID: ""}); err == nil {
		t.Fatal("expected error for empty profile id")
	}
}
