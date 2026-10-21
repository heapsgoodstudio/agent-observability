package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func parseFixture(t *testing.T, name string) parsed {
	t.Helper()
	p, e := parseRollout(context.Background(), strings.NewReader(fixture(t, name)))
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func sumRequests(p parsed) counts {
	var c counts
	for _, r := range p.Requests {
		c.Input += r.Usage.Input
		c.Output += r.Usage.Output
		c.Cached += r.Usage.Cached
		c.Write += r.Usage.Write
		c.Reasoning += r.Usage.Reasoning
	}
	return c
}

func TestOverlappingUsageRecordsBothOrders(t *testing.T) {
	p := parseFixture(t, "mixed")
	if len(p.Requests) != 2 || sumRequests(p) != (counts{Input: 300, Cached: 120, Write: 20, Output: 50, Reasoning: 25}) {
		t.Fatalf("overlap counted twice: %+v", p)
	}
	if !p.Requests[0].Modern || p.Tools["exec_command"] != 1 || p.UserTurns != 1 || p.DurationMS != 6000 {
		t.Fatalf("normalization: %+v", p)
	}
}

func TestLegacyMirrorOmitsCacheWriteField(t *testing.T) {
	lines := strings.Split(fixture(t, "mixed"), "\n")
	for i, l := range lines {
		if strings.Contains(l, `"type":"token_count"`) {
			lines[i] = strings.ReplaceAll(l, `"cache_write_input_tokens":20,`, "")
		}
	}
	p, e := parseRollout(context.Background(), strings.NewReader(strings.Join(lines, "\n")))
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Requests) != 2 || sumRequests(p).Input != 300 || sumRequests(p).Write != 20 {
		t.Fatalf("legacy mirror duplicated modern usage: %+v", p.Requests)
	}
}
func TestLegacyCumulativeResetAndModelChanges(t *testing.T) {
	p := parseFixture(t, "legacy")
	if len(p.Requests) != 3 || sumRequests(p).Input != 180 || sumRequests(p).Output != 32 {
		t.Fatalf("legacy delta/reset: %+v", p.Requests)
	}
	if p.Requests[0].Model != "gpt-5-codex" || p.Requests[1].Model != "unpriced-custom" {
		t.Fatalf("model attribution: %+v", p.Requests)
	}
}
func TestSubagentInheritedMetadataAndUsage(t *testing.T) {
	p := parseFixture(t, "subagent")
	if p.ID != "thread-child" || p.ParentID != "thread-main" || p.HistoryMode != "paginated" || len(p.Requests) != 1 || sumRequests(p).Input != 25 || len(p.Tools) != 0 {
		t.Fatalf("inherited data attributed to child: %+v", p)
	}
}
func TestPartialAndMalformedRecords(t *testing.T) {
	s := fixture(t, "mixed") + "{broken}\n" + `{"type":"token_usage_record","payload":{"usage":{"input_tokens":999}}}`
	p, e := parseRollout(context.Background(), strings.NewReader(s))
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Warnings) != 1 || sumRequests(p).Input != 300 {
		t.Fatalf("partial tail ingested: %+v", p)
	}
}
func TestForeignUsageAndMirroredLegacyIgnored(t *testing.T) {
	s := `{"type":"session_meta","payload":{"id":"fork","forked_from_id":"parent"}}
{"type":"token_usage_record","payload":{"thread_id":"parent","response_id":"old","usage":{"input_tokens":100,"output_tokens":10},"thread_token_usage":{"input_tokens":100,"output_tokens":10}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10},"last_token_usage":{"input_tokens":100,"output_tokens":10}}}}
{"type":"token_usage_record","payload":{"thread_id":"fork","response_id":"new","usage":{"input_tokens":20,"output_tokens":2},"thread_token_usage":{"input_tokens":120,"output_tokens":12}}}
`
	p, e := parseRollout(context.Background(), strings.NewReader(s))
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Requests) != 1 || sumRequests(p).Input != 20 {
		t.Fatalf("copied usage: %+v", p)
	}
}

func TestResponseReplacementUpdatesMirroredCheckpoint(t *testing.T) {
	s := `{"type":"session_meta","payload":{"id":"s"}}
{"type":"token_usage_record","payload":{"thread_id":"s","response_id":"r","usage":{"input_tokens":10,"output_tokens":1},"thread_token_usage":{"input_tokens":10,"output_tokens":1}}}
{"type":"token_usage_record","payload":{"thread_id":"s","response_id":"r","usage":{"input_tokens":20,"output_tokens":2},"thread_token_usage":{"input_tokens":20,"output_tokens":2}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":20,"output_tokens":2},"last_token_usage":{"input_tokens":20,"output_tokens":2}}}}
`
	p, e := parseRollout(context.Background(), strings.NewReader(s))
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Requests) != 1 || sumRequests(p).Input != 20 {
		t.Fatalf("response replacement double counted: %+v", p.Requests)
	}
}

func TestCompactionSeparatesCumulativeLedgersAndKeepsLegacySegments(t *testing.T) {
	p := parseFixture(t, "compaction-offset")
	want := counts{Input: 431000, Cached: 375100, Output: 4520, Reasoning: 2005}
	if len(p.Requests) != 7 || sumRequests(p) != want || len(p.Warnings) != 0 {
		t.Fatalf("compaction reconciliation: requests=%d totals=%+v warnings=%v", len(p.Requests), sumRequests(p), p.Warnings)
	}
	for _, r := range p.Requests {
		if r.Usage.Input > 100000 {
			t.Fatalf("cumulative snapshot became request usage: %+v", r)
		}
	}
	if p.Requests[0].Modern || p.Requests[len(p.Requests)-1].Modern {
		t.Fatal("legacy-only prefix or suffix dropped")
	}
}

func TestCompactionMirrorCanPrecedeModernRecord(t *testing.T) {
	lines := strings.Split(strings.TrimSuffix(fixture(t, "compaction-offset"), "\n"), "\n")
	for i := 0; i+1 < len(lines); i++ {
		if strings.Contains(lines[i], `"type":"token_usage_record"`) && strings.Contains(lines[i+1], `"type":"token_count"`) {
			lines[i], lines[i+1] = lines[i+1], lines[i]
			i++
		}
	}
	p, e := parseRollout(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"))
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Requests) != 7 || sumRequests(p).Input != 431000 || len(p.Warnings) != 0 {
		t.Fatalf("reversed compaction mirror: requests=%d total=%+v warnings=%v", len(p.Requests), sumRequests(p), p.Warnings)
	}
}

func TestLegacyMirrorMayOmitReasoningSubset(t *testing.T) {
	lines := strings.Split(fixture(t, "mixed"), "\n")
	for i, l := range lines {
		if strings.Contains(l, `"type":"token_count"`) {
			for _, n := range []string{"10", "15", "25"} {
				l = strings.ReplaceAll(l, `,"reasoning_output_tokens":`+n, "")
			}
			lines[i] = l
		}
	}
	p, e := parseRollout(context.Background(), strings.NewReader(strings.Join(lines, "\n")))
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Requests) != 2 || sumRequests(p).Input != 300 || sumRequests(p).Reasoning != 25 {
		t.Fatalf("missing subset caused cumulative restart: %+v", p.Requests)
	}
}

func parseLegacyValues(t *testing.T, totals, last []counts) parsed {
	t.Helper()
	var b strings.Builder
	b.WriteString("{\"type\":\"session_meta\",\"payload\":{\"id\":\"legacy\"}}\n")
	b.WriteString("{\"type\":\"turn_context\",\"payload\":{\"turn_id\":\"t\",\"model\":\"gpt-5.6-sol\"}}\n")
	for i, total := range totals {
		var usage *counts
		if i < len(last) {
			usage = &last[i]
		}
		r := map[string]any{"timestamp": fmt.Sprintf("2026-09-22T10:00:%02dZ", i), "type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": total, "last_token_usage": usage}}}
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	p, e := parseRollout(context.Background(), strings.NewReader(b.String()))
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func TestSubsetCorrectionNeverImportsLifetimeTotal(t *testing.T) {
	first := counts{Input: 1000, Cached: 900, Output: 100, Reasoning: 50}
	p := parseLegacyValues(t, []counts{first, {Input: 1010, Cached: 915, Output: 110, Reasoning: 55}}, []counts{first, {Input: 10, Output: 10, Reasoning: 5}})
	if sumRequests(p).Input != 1010 || len(p.Requests) != 2 || len(p.Warnings) == 0 {
		t.Fatalf("subset correction treated as reset: %+v warnings=%v", p.Requests, p.Warnings)
	}
	p = parseLegacyValues(t, []counts{first, {Input: 1000, Cached: 915, Output: 100, Reasoning: 50}}, []counts{first, first})
	if sumRequests(p).Input != 1000 || len(p.Requests) != 1 || len(p.Warnings) == 0 {
		t.Fatalf("subset-only correction counted as response: %+v", p.Requests)
	}
}

func TestStaleLegacyCheckpointDoesNotRestartLifetime(t *testing.T) {
	a := counts{Input: 1000, Cached: 900, Output: 100, Reasoning: 50}
	u := counts{Input: 100, Cached: 50, Output: 20, Reasoning: 10}
	p := parseLegacyValues(t, []counts{a, {Input: 1100, Cached: 950, Output: 120, Reasoning: 60}, a, {Input: 1200, Cached: 1000, Output: 140, Reasoning: 70}}, []counts{a, u, a, u})
	if sumRequests(p).Input != 1200 || len(p.Requests) != 3 || len(p.Warnings) == 0 {
		t.Fatalf("stale snapshot inflated usage: %+v warnings=%v", p.Requests, p.Warnings)
	}
}

func TestLegacySnapshotRequiresExplicitResponseUsage(t *testing.T) {
	total := counts{Input: 1000000, Cached: 900000, Output: 10000}
	p := parseLegacyValues(t, []counts{total}, []counts{{Input: 100, Output: 10}})
	if sumRequests(p).Input != 100 || len(p.Warnings) == 0 {
		t.Fatalf("starting snapshot counted as usage: %+v", p)
	}
	p = parseLegacyValues(t, []counts{total}, nil)
	if len(p.Requests) != 0 || p.UsageKnown || len(p.Warnings) == 0 {
		t.Fatalf("missing response usage inferred from total: %+v", p)
	}
}
