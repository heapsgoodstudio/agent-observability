package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestHistoricalQuotaSnapshotsCannotClassifyOrPriceRequests(t *testing.T) {
	for name, quota := range map[string]string{
		"below_cap":          `{"primary":{"used_percent":20,"resets_at":2000000000}}`,
		"threshold_crossing": `{"primary":{"used_percent":100,"resets_at":2000000000}}`,
		"over_cap":           `{"primary":{"used_percent":120,"resets_at":2000000000},"rate_limit_reached_type":"rate_limit_reached"}`,
		"past_reset":         `{"primary":{"used_percent":100,"resets_at":1}}`,
		"reset_to_zero":      `{"primary":{"used_percent":0,"resets_at":2000000000}}`,
		"pooled_credits":     `{"credits":{"has_credits":true,"unlimited":false,"balance":"100"},"plan_type":"business"}`,
		"blocked":            `{"ordinary_usage_allowed":false,"spend_control_reached":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if err := os.Mkdir(filepath.Join(home, "sessions"), 0700); err != nil {
				t.Fatal(err)
			}
			body := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"billing-fixture\"}}\n" +
				"{\"type\":\"turn_context\",\"payload\":{\"model\":\"gpt-5.6-sol\"}}\n" +
				"{\"type\":\"token_usage_record\",\"payload\":{\"response_id\":\"response-1\",\"usage\":{\"input_tokens\":1000,\"cached_input_tokens\":600,\"output_tokens\":10,\"reasoning_output_tokens\":5}}}\n" +
				fmt.Sprintf("{\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"total_token_usage\":{\"input_tokens\":1000,\"cached_input_tokens\":600,\"output_tokens\":10,\"reasoning_output_tokens\":5},\"last_token_usage\":{\"input_tokens\":1000,\"cached_input_tokens\":600,\"output_tokens\":10,\"reasoning_output_tokens\":5}},\"rate_limits\":%s}}\n", quota)
			if err := os.WriteFile(filepath.Join(home, "sessions", "fixture.jsonl"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := Collect(context.Background(), Options{Home: home, ProfileID: "billing", LocalOnly: true})
			if err != nil || len(result.Sessions) != 1 {
				t.Fatalf("collect: %+v %v", result, err)
			}
			row := result.Sessions[0]
			billing := result.Details[row.ID].Billing
			if row.Requests != 1 || !billing.UsageKnown || billing.ClassificationAvailable || billing.UnknownRequests != 1 || billing.IncludedRequests != 0 || billing.OutsideRequests != 0 || billing.EstimatedOutsideRequests != 0 || billing.UnpricedOutsideRequests != 0 || billing.OutsideEstimateUSD != nil {
				t.Fatalf("quota snapshot classified a request: %+v", billing)
			}
			if len(billing.Models) != 1 || billing.Models[0].UnknownRequests != 1 || billing.Models[0].Input != 1000 || billing.Models[0].CachedInput != 600 || billing.Models[0].Output != 10 || billing.Models[0].Reasoning != 5 || billing.Models[0].OutsideEstimateUSD != nil {
				t.Fatalf("token-only billing activity: %+v", billing.Models)
			}
		})
	}
}

func TestUnavailableTokenUsageCannotProveZeroBilling(t *testing.T) {
	billing := billingDetail(false, nil)
	if billing.UsageKnown || billing.ClassificationAvailable || billing.OutsideEstimateUSD != nil {
		t.Fatalf("missing usage became a known zero bill: %+v", billing)
	}
}
