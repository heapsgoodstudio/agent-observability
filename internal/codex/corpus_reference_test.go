package codex

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
)

// Optional independent reference comparison. Inputs must be numeric, sanitized
// rollouts and per-thread expectations; no credentials or conversation bodies.
func TestSanitizedReferenceCorpus(t *testing.T) {
	home, reference := os.Getenv("CODEX_OBS_SANITIZED_HOME"), os.Getenv("CODEX_OBS_REFERENCE_ROWS")
	if home == "" || reference == "" {
		t.Skip("requires sanitized numeric corpus and independent reference")
	}
	var expected []struct {
		ID       string  `json:"id"`
		Tokens   counts  `json:"tokens"`
		Requests int     `json:"requests"`
		Cost     float64 `json:"cost"`
	}
	b, err := os.ReadFile(reference)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &expected); err != nil {
		t.Fatal(err)
	}
	result, err := Collect(context.Background(), Options{Home: home, ProfileID: "reference", LocalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, r := range result.Sessions {
		byID[r.NativeID] = i
	}
	var input, cached, output, reasoning int64
	requests := 0
	cost := 0.0
	for _, want := range expected {
		i, ok := byID[want.ID]
		if !ok {
			t.Errorf("missing reference thread %s", want.ID)
			continue
		}
		row := result.Sessions[i]
		got := []int64{row.Tokens.Input + row.Tokens.CacheRead + row.Tokens.CacheCreation, row.Tokens.CacheRead, row.Tokens.Output, row.ReasoningTokens}
		input += got[0]
		cached += got[1]
		output += got[2]
		reasoning += got[3]
		requests += row.Requests
		cost += row.CostUSD
		if got[0] != want.Tokens.Input || got[1] != want.Tokens.Cached || got[2] != want.Tokens.Output || got[3] != want.Tokens.Reasoning || row.Tokens.CacheCreation != want.Tokens.Write || row.Requests != want.Requests || math.Abs(row.CostUSD-want.Cost) > 0.00001 {
			t.Errorf("thread %s: got tokens=%v requests=%d cost=%.6f; want tokens=%v requests=%d cost=%.6f", want.ID, got, row.Requests, row.CostUSD, want.Tokens, want.Requests, want.Cost)
		}
	}
	t.Logf("threads=%d requests=%d input=%d cached=%d output=%d reasoning=%d cost=%.6f warning_kinds=%d", len(result.Sessions), requests, input, cached, output, reasoning, cost, len(result.Warnings))
}
