package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rejectedRollout() string {
	return `{"type":"session_meta","payload":{"id":"rejected"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"turn_context","payload":{"turn_id":"turn","model":"unsupported-test-model"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"PRIVATE"}]}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage"}}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn","duration_ms":100,"error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The 'unsupported-test-model' model is not supported when using Codex with a ChatGPT account.\"}}"}}}
`
}

func TestRejectedRequestsDoNotInventMissingTokenUsage(t *testing.T) {
	p, err := parseRollout(context.Background(), strings.NewReader(rejectedRollout()))
	if err != nil || !p.UsageKnown || !p.RejectedOnly || len(p.Requests) != 0 || !p.ToolsKnown || p.Errors != 1 || !p.ErrorsKnown || p.DurationMS != 100 {
		t.Fatalf("unsupported request should have known zero model usage: %+v, %v", p, err)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "PRIVATE") || strings.Contains(string(b), "not supported") {
		t.Fatal("raw messages escaped the parser")
	}
}

func TestRejectionCannotProveZeroForAmbiguousOrIncompleteSessions(t *testing.T) {
	base := rejectedRollout()
	for name, text := range map[string]string{
		"interrupted":           strings.Replace(base, `"type":"task_complete"`, `"type":"turn_aborted"`, 1),
		"unfinished later turn": base + "{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\",\"turn_id\":\"next\"}}\n",
		"assistant response":    base + "{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\"}}\n",
		"tool call":             base + "{\"type\":\"response_item\",\"payload\":{\"type\":\"function_call\",\"name\":\"shell\"}}\n",
		"reasoning":             base + "{\"type\":\"event_msg\",\"payload\":{\"type\":\"agent_reasoning\"}}\n",
		"other error":           strings.Replace(base, "not supported", "temporarily unavailable", 1),
		"transport error":       strings.Replace(base, `\"status\":400`, `\"status\":500`, 1),
		"missing header":        strings.Join(strings.Split(base, "\n")[1:], "\n"),
		"malformed line":        base + "broken\n",
		"partial tail":          base + `{"type":`,
		"missing final newline": strings.TrimSuffix(base, "\n"),
		"forked history":        strings.Replace(base, `"id":"rejected"`, `"id":"rejected","forked_from_id":"parent"`, 1),
		"completed with output": strings.Replace(base, `"duration_ms":100`, `"duration_ms":100,"last_agent_message":"output"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			p, err := parseRollout(context.Background(), strings.NewReader(text))
			if err != nil || p.UsageKnown || p.RejectedOnly || p.UsageUnavailableReason == "" {
				t.Fatalf("ambiguous history must remain unknown: %+v, %v", p, err)
			}
			if name == "interrupted" && p.UsageUnavailableReason != "interrupted" {
				t.Fatalf("interruption reason lost: %+v", p)
			}
		})
	}
}

func TestRejectedPrefixCannotHideUsageInAnotherCopy(t *testing.T) {
	rejected, _ := parseRollout(context.Background(), strings.NewReader(rejectedRollout()))
	unknown := parsed{ID: "rejected", UsageUnavailableReason: "interrupted"}
	for _, pair := range [][2]parsed{{rejected, unknown}, {unknown, rejected}} {
		got := mergeParsed(pair[0], pair[1])
		if got.UsageKnown || got.RejectedOnly || got.UsageUnavailableReason != "interrupted" {
			t.Fatalf("rejected prefix concealed unknown continuation: %+v", got)
		}
	}
}

func TestHistoryAPIActivityOverridesRejectedLocalPrefix(t *testing.T) {
	for _, activity := range []bool{false, true} {
		home := fixtureHome(t)
		if err := os.WriteFile(filepath.Join(home, "sessions", "rejected.jsonl"), []byte(rejectedRollout()), 0600); err != nil {
			t.Fatal(err)
		}
		rpc := &fakeRPC{call: func(method string, params map[string]any) (any, error) {
			switch method {
			case "thread/list":
				return map[string]any{"data": []any{}}, nil
			case "thread/turns/list":
				items := []any{map[string]any{"id": "user", "type": "userMessage"}}
				if activity {
					items = append(items, map[string]any{"id": "reply", "type": "agentMessage"})
				}
				return map[string]any{"data": []any{map[string]any{"id": "turn", "status": "failed", "items": items}}}, nil
			}
			t.Fatalf("unexpected method %s", method)
			return nil, nil
		}}
		res, err := Collect(context.Background(), Options{Home: home, ProfileID: "test", Connect: func(context.Context, Options) (RPC, error) { return rpc, nil }})
		if err != nil || len(res.Sessions) != 1 || res.Sessions[0].UsageKnown == activity {
			t.Fatalf("history disagreement ignored: %+v, %v", res, err)
		}
		if len(res.Warnings) != 0 {
			t.Fatalf("token gaps should be structured, not duplicate warnings: %v", res.Warnings)
		}
	}
}
