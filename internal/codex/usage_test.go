package codex

import (
	"context"
	"encoding/json"
	"testing"
)

func usageResponse(t *testing.T, body string) (Usage, error) {
	t.Helper()
	rpc := &fakeRPC{call: func(method string, params map[string]any) (any, error) {
		if method != "account/rateLimits/read" || len(params) != 1 || params["excludeResetCreditDetails"] != true {
			t.Fatalf("unexpected monitoring request: %s %+v", method, params)
		}
		return json.RawMessage(body), nil
	}}
	return ReadUsage(context.Background(), Options{Home: t.TempDir(), Connect: func(context.Context, Options) (RPC, error) { return rpc, nil }})
}

func TestReadUsagePreservesCreditAndSpendControlOnlySnapshot(t *testing.T) {
	u, err := usageResponse(t, `{"accountId":"account-fixture","ordinaryUsageAllowed":false,"rateLimitResetCredits":{"availableCount":2},"rateLimits":{"limitId":"codex","planType":"pro","credits":{"hasCredits":true,"unlimited":false,"balance":"12.3456789"},"individualLimit":{"limit":"100.500","used":"100.500","remainingPercent":0,"resetsAt":1000},"spendControlReached":true,"rateLimitReachedType":"workspace_member_usage_limit_reached"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Windows) != 0 || len(u.Buckets) != 1 || u.AccountID != "account-fixture" || u.PlanType != "pro" || u.FetchedAt.IsZero() {
		t.Fatalf("credit-only snapshot discarded: %+v", u)
	}
	b := u.Buckets[0]
	if u.OrdinaryUsageAllowed == nil || *u.OrdinaryUsageAllowed || u.ResetCreditsAvailable == nil || *u.ResetCreditsAvailable != 2 || b.Credits == nil || !b.Credits.HasCredits || b.Credits.Unlimited || b.Credits.Balance == nil || *b.Credits.Balance != "12.3456789" {
		t.Fatalf("lost permission or decimal credit data: %+v %+v", u, b)
	}
	if b.IndividualLimit == nil || b.IndividualLimit.Limit != "100.500" || b.IndividualLimit.Used != "100.500" || b.IndividualLimit.RemainingPercent != 0 || b.IndividualLimit.ResetsAt != 1000 || b.SpendControlReached == nil || !*b.SpendControlReached || b.RateLimitReachedType == nil || *b.RateLimitReachedType != "workspace_member_usage_limit_reached" {
		t.Fatalf("lost spend controls: %+v", b)
	}
}

func TestReadUsageUnknownFieldsAndOverHundredRemainDistinct(t *testing.T) {
	u, err := usageResponse(t, `{"ordinaryUsageAllowed":null,"rateLimits":{},"rateLimitsByLimitId":{"model-reserve":{"limitId":"wrong-alias","normalModelSlug":"gpt-5.6-luna","planType":"pro","primary":{"usedPercent":125.5,"windowDurationMins":null,"resetsAt":null},"credits":{"hasCredits":true,"unlimited":true,"balance":null},"spendControlReached":null},"codex":{"planType":"pro","primary":{"usedPercent":0,"windowDurationMins":300,"resetsAt":2000},"secondary":{"usedPercent":42,"windowDurationMins":10080,"resetsAt":3000}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if u.AccountID != "" || u.OrdinaryUsageAllowed != nil || u.ResetCreditsAvailable != nil || len(u.Buckets) != 2 || len(u.Windows) != 3 {
		t.Fatalf("null became known or buckets lost: %+v", u)
	}
	reserve := u.Buckets[1]
	if reserve.LimitID != "model-reserve" || reserve.NormalModelSlug != "gpt-5.6-luna" || reserve.Primary.UsedPercent != 125.5 || reserve.Primary.WindowMinutes != nil || reserve.Primary.ResetsAt != nil || reserve.Credits.Balance != nil || reserve.SpendControlReached != nil {
		t.Fatalf("nullable reserve bucket: %+v", reserve)
	}
	if u.Windows[2].WindowMinutesKnown || u.Windows[2].ResetsAtKnown || u.Windows[2].UsedPercent != 125.5 {
		t.Fatalf("unknown window projection: %+v", u.Windows[2])
	}
	if !u.Windows[0].WindowMinutesKnown || !u.Windows[0].ResetsAtKnown || u.Windows[0].WindowMinutes != 300 || u.Windows[0].ResetsAt != 2000 {
		t.Fatalf("known window projection: %+v", u.Windows[0])
	}
}

func TestReadUsagePermissionsNeverInferredFromResetOrPercent(t *testing.T) {
	for _, permission := range []string{"null", "false", "true"} {
		t.Run(permission, func(t *testing.T) {
			u, err := usageResponse(t, `{"ordinaryUsageAllowed":`+permission+`,"rateLimits":{"primary":{"usedPercent":0,"resetsAt":1},"spendControlReached":null}}`)
			if err != nil || len(u.Windows) != 1 {
				t.Fatalf("response: %+v %v", u, err)
			}
			if permission == "null" {
				if u.OrdinaryUsageAllowed != nil {
					t.Fatal("reset/zero usage inferred permission")
				}
			} else if u.OrdinaryUsageAllowed == nil || *u.OrdinaryUsageAllowed != (permission == "true") {
				t.Fatal("backend permission changed")
			}
		})
	}
}

func TestReadUsageNoQuotaDoesNotBecomeZero(t *testing.T) {
	for _, body := range []string{`{}`, `{"rateLimits":null}`, `{"rateLimits":{"primary":{"usedPercent":null}}}`, `{"rateLimits":{"primary":{}}}`, `{"rateLimits":{"planType":"pro","credits":null,"spendControlReached":null}}`} {
		if u, err := usageResponse(t, body); err == nil || !u.FetchedAt.IsZero() {
			t.Fatalf("missing data became a snapshot: %+v %v", u, err)
		}
	}
	for _, body := range []string{`{"rateLimits":{},"ordinaryUsageAllowed":false}`, `{"rateLimits":{},"rateLimitResetCredits":{"availableCount":0}}`, `{"rateLimits":{"spendControlReached":false}}`} {
		if u, err := usageResponse(t, body); err != nil || u.FetchedAt.IsZero() {
			t.Fatalf("explicit control/zero became unavailable: %+v %v", u, err)
		}
	}
}

func TestReadUsageCompatiblePollParameterFallback(t *testing.T) {
	calls := 0
	rpc := &fakeRPC{call: func(method string, params map[string]any) (any, error) {
		calls++
		if method != "account/rateLimits/read" {
			t.Fatalf("unexpected method: %s", method)
		}
		if calls == 1 {
			if params["excludeResetCreditDetails"] != true {
				t.Fatal("reset detail lookup not skipped")
			}
			return nil, &RPCError{Code: -32602, Method: method}
		}
		if len(params) != 0 {
			t.Fatalf("fallback capabilities: %+v", params)
		}
		return json.RawMessage(`{"rateLimits":{"primary":{"usedPercent":25}}}`), nil
	}}
	u, err := ReadUsage(context.Background(), Options{Home: t.TempDir(), Connect: func(context.Context, Options) (RPC, error) { return rpc, nil }})
	if err != nil || calls != 2 || len(u.Windows) != 1 {
		t.Fatalf("older server fallback: %+v %v calls=%d", u, err, calls)
	}
}

func TestReadUsageConflictingBucketPlansStayUnknown(t *testing.T) {
	u, err := usageResponse(t, `{"rateLimits":{"planType":"pro"},"rateLimitsByLimitId":{"a":{"planType":"plus","primary":{"usedPercent":10}},"b":{"planType":"pro","primary":{"usedPercent":20}}}}`)
	if err != nil || u.PlanType != "" || len(u.Buckets) != 2 || u.Buckets[0].PlanType != "plus" || u.Buckets[1].PlanType != "pro" {
		t.Fatalf("bucket plans flattened arbitrarily: %+v %v", u, err)
	}
}
