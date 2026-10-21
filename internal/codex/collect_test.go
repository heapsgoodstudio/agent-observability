package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRPC struct {
	call    func(string, map[string]any) (any, error)
	methods []string
}

func (f *fakeRPC) Close() error { return nil }
func (f *fakeRPC) Call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.methods = append(f.methods, method)
	v, err := f.call(method, params.(map[string]any))
	if err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, result)
}
func fixtureHome(t *testing.T, names ...string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(home, "sessions", name+".jsonl"), []byte(fixture(t, name)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestCollectMetadataPaginationArchivedAndUnknownUsage(t *testing.T) {
	home := fixtureHome(t, "mixed")
	archivedSeen := false
	pages := 0
	rpc := &fakeRPC{call: func(method string, p map[string]any) (any, error) {
		switch method {
		case "thread/list":
			if p["useStateDbOnly"] != true || len(p["sourceKinds"].([]string)) != len(allSources) {
				t.Fatal("unsafe/incomplete list filters")
			}
			if p["archived"] == true {
				archivedSeen = true
				return map[string]any{"data": []any{map[string]any{"id": "archived", "historyMode": "legacy"}}}, nil
			}
			pages++
			if p["cursor"] == "second" {
				return map[string]any{"data": []any{map[string]any{"id": "missing-usage", "historyMode": "paginated", "model": "gpt-6-astra"}}}, nil
			}
			return map[string]any{"data": []any{map[string]any{"id": "thread-main", "historyMode": "paginated", "updatedAt": 10}}, "nextCursor": "second"}, nil
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": p["threadId"]}}, nil
		case "thread/items/list":
			return map[string]any{"data": []any{}}, nil
		case "thread/turns/list":
			return map[string]any{"data": []any{map[string]any{"id": "turn", "status": "failed", "durationMs": 1200, "items": []any{map[string]any{"type": "userMessage", "id": "user", "content": "PRIVATE"}, map[string]any{"type": "commandExecution", "id": "tool", "status": "failed", "command": "SECRET"}}}}}, nil
		default:
			return nil, fmt.Errorf("unexpected %s", method)
		}
	}}
	r, err := Collect(context.Background(), Options{Home: home, ProfileID: "profile-a", Connect: func(context.Context, Options) (RPC, error) { return rpc, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !archivedSeen || pages != 2 || len(r.Sessions) != 3 || r.Complete {
		t.Fatalf("coverage: %+v", r)
	}
	if !r.Capabilities.ItemsList || !r.Capabilities.TurnsList || !r.Capabilities.ThreadRead || !r.Capabilities.PaginatedHistory || !r.Capabilities.LegacyHistory {
		t.Fatalf("caps: %+v", r.Capabilities)
	}
	for _, row := range r.Sessions {
		if row.Provider != "codex" || row.Profile != "profile-a" || row.ID != SessionID("profile-a", row.NativeID) {
			t.Fatalf("identity: %+v", row)
		}
		if row.NativeID == "thread-main" {
			// Codex input already includes cache reads and writes: do not add
			// either subset again, or use cumulative thread usage as context.
			if len(row.Trajectory) != 2 || row.Trajectory[0][2] != 100 || row.Trajectory[1][2] != 200 {
				t.Fatalf("context: %+v", row.Trajectory)
			}
			if row.Tokens.Input != 160 || row.Tokens.CacheRead != 120 || row.Tokens.CacheCreation != 20 || row.Tokens.Output != 50 || row.ReasoningTokens != 25 || !row.CostKnown || row.ToolCalls != 1 || row.ErrorCount != 2 || row.DurationMS != 1200 {
				t.Fatalf("row: %+v", row)
			}
		} else if row.UsageKnown || row.CostKnown {
			t.Fatalf("missing usage displayed as free: %+v", row)
		}
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "PRIVATE") || strings.Contains(string(b), "SECRET") {
		t.Fatal("conversation data escaped")
	}
}

func TestLegacyAPIFallbackAndRepeatedCursor(t *testing.T) {
	rpc := &fakeRPC{call: func(m string, p map[string]any) (any, error) {
		if m == "thread/turns/list" {
			return nil, &RPCError{Code: -32601, Method: m}
		}
		if m == "thread/read" && p["includeTurns"] == true {
			return map[string]any{"thread": map[string]any{"turns": []any{map[string]any{"id": "one", "status": "completed", "items": []any{map[string]any{"id": "tool", "type": "mcpToolCall", "server": "local", "tool": "read", "status": "completed"}}}}}}, nil
		}
		return nil, fmt.Errorf("unexpected %s", m)
	}}
	var caps Capabilities
	h, e := readHistory(context.Background(), rpc, thread{ID: "old"}, &caps)
	if e != nil || !caps.ThreadRead || h.Tools["mcp__local__read"] != 1 {
		t.Fatalf("fallback: %+v %v", h, e)
	}
	rpc.call = func(string, map[string]any) (any, error) {
		return map[string]any{"data": []any{}, "nextCursor": "same"}, nil
	}
	_, e = listThreads(context.Background(), rpc, 100)
	if e == nil || !strings.Contains(e.Error(), "repeated") {
		t.Fatalf("cursor loop not stopped: %v", e)
	}
}

func TestCollectorIncrementalReplacementAndPartialTail(t *testing.T) {
	home := fixtureHome(t, "mixed")
	opts := Options{Home: home, ProfileID: "stable", LocalOnly: true}
	c := NewCollector()
	r, e := c.Collect(context.Background(), opts)
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Sessions) != 1 || r.Sessions[0].Requests != 2 {
		t.Fatal(r)
	}
	r.Sessions[0].Tools["exec_command"] = 999
	r, e = c.Collect(context.Background(), opts)
	if e != nil || r.Sessions[0].ToolCalls != 1 {
		t.Fatalf("caller mutated cache: %+v %v", r, e)
	}
	path := filepath.Join(home, "sessions", "mixed.jsonl")
	partial := `{"type":"token_usage_record","payload":{"thread_id":"thread-main","response_id":"tail","usage":{"input_tokens":10,"output_tokens":1}}}`
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString(partial)
	_ = f.Close()
	r, e = c.Collect(context.Background(), opts)
	if e != nil || r.Sessions[0].Requests != 2 {
		t.Fatalf("partial indexed: %+v %v", r, e)
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString("\n")
	_ = f.Close()
	r, e = c.Collect(context.Background(), opts)
	if e != nil || r.Sessions[0].Requests != 3 {
		t.Fatalf("tail not retried: %+v %v", r, e)
	}
	if err := os.WriteFile(path, []byte(fixture(t, "legacy")), 0600); err != nil {
		t.Fatal(err)
	}
	r, e = c.Collect(context.Background(), opts)
	if e != nil || len(r.Sessions) != 1 || r.Sessions[0].NativeID != "thread-legacy" || r.Sessions[0].CostKnown || r.Sessions[0].CostUSD <= 0 {
		t.Fatalf("replacement/partial pricing: %+v %v", r, e)
	}
}

func TestReadUsageNoHistoricalQuotaAndUnknownAccount(t *testing.T) {
	rpc := &fakeRPC{call: func(m string, p map[string]any) (any, error) {
		if m != "account/rateLimits/read" {
			t.Fatalf("unexpected method %s", m)
		}
		return map[string]any{"rateLimits": map[string]any{"limitId": "codex", "primary": map[string]any{"usedPercent": 99, "windowDurationMins": 300}}, "rateLimitsByLimitId": map[string]any{"codex": map[string]any{"planType": "pro", "primary": map[string]any{"usedPercent": 0, "windowDurationMins": 300, "resetsAt": 123}, "secondary": map[string]any{"usedPercent": 42, "windowDurationMins": 10080, "resetsAt": 456}}}}, nil
	}}
	opts := Options{Home: t.TempDir(), Connect: func(context.Context, Options) (RPC, error) { return rpc, nil }}
	u, e := ReadUsage(context.Background(), opts)
	if e != nil || u.AccountID != "" || u.PlanType != "pro" || len(u.Windows) != 2 || u.Windows[0].UsedPercent != 0 || time.Since(u.FetchedAt) > time.Second {
		t.Fatalf("usage: %+v %v", u, e)
	}
	rpc.call = func(string, map[string]any) (any, error) { return map[string]any{}, nil }
	u, e = ReadUsage(context.Background(), opts)
	if e == nil || !u.FetchedAt.IsZero() {
		t.Fatalf("unavailable limits become zero: %+v %v", u, e)
	}
}

func TestCanceledCollectionAndUnsafePath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, e := Collect(ctx, Options{Home: fixtureHome(t, "mixed"), ProfileID: "p", LocalOnly: true})
	if e == nil {
		t.Fatal("canceled collection succeeded")
	}
	if _, ok := safePath(t.TempDir(), t.TempDir()); ok {
		t.Fatal("outside path allowed")
	}
}

func TestMetadataCancellationRetainsEveryCompletedLocalSummary(t *testing.T) {
	home := fixtureHome(t, "mixed", "legacy")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	rpc := &fakeRPC{call: func(m string, p map[string]any) (any, error) {
		if m != "thread/list" {
			return nil, fmt.Errorf("unexpected %s", m)
		}
		calls++
		if calls == 3 {
			cancel()
			return nil, context.Canceled
		}
		id := "thread-main"
		if calls == 2 {
			id = "thread-legacy"
		}
		return map[string]any{"data": []any{map[string]any{"id": id}}, "nextCursor": fmt.Sprint(calls)}, nil
	}}
	r, err := Collect(ctx, Options{Home: home, ProfileID: "p", Connect: func(context.Context, Options) (RPC, error) { return rpc, nil }})
	if err == nil || r.Complete || len(r.Sessions) != 2 {
		t.Fatalf("cancellation lost local results: %+v, %v", r, err)
	}
	for _, row := range r.Sessions {
		if !row.UsageKnown || row.Requests == 0 || row.Tokens.Input == 0 {
			t.Fatalf("local totals lost: %+v", row)
		}
	}
}

func TestSubagentRowsCountOwnUsageOnce(t *testing.T) {
	r, e := Collect(context.Background(), Options{Home: fixtureHome(t, "mixed", "subagent"), ProfileID: "p", LocalOnly: true})
	if e != nil {
		t.Fatal(e)
	}
	input := int64(0)
	for _, row := range r.Sessions {
		input += row.Tokens.Input + row.Tokens.CacheRead + row.Tokens.CacheCreation
		if row.NativeID == "thread-child" && row.ParentID != "codex:p:thread-main" {
			t.Fatalf("parent: %+v", row)
		}
	}
	if len(r.Sessions) != 2 || input != 325 {
		t.Fatalf("subagent usage duplicated: %+v", r.Sessions)
	}
}

func TestCompactionTotalsDoNotTriggerLongContextPricing(t *testing.T) {
	r, err := Collect(context.Background(), Options{Home: fixtureHome(t, "compaction-offset"), ProfileID: "p", LocalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sessions) != 1 {
		t.Fatalf("sessions=%d", len(r.Sessions))
	}
	row := r.Sessions[0]
	// Every real response has <=100k input tokens. Legacy cumulative totals
	// exceed 272k after compaction; these must not become long-context calls.
	want := (55900.0*4 + 375100*.4 + 4520*20) / 1e6
	if row.Requests != 7 || row.Tokens.Input != 55900 || row.Tokens.CacheRead != 375100 || !row.CostKnown || math.Abs(row.CostUSD-want) > 1e-10 {
		t.Fatalf("fake long-context request priced: %+v want_cost=%.8f", row, want)
	}
}
