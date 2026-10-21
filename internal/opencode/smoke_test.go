package opencode

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestSmokeRealDir runs a real, read-only Collect against a real opencode
// data directory, for hand verification against the actual on-disk data. It
// is skipped unless OPENCODE_SMOKE_DIR is set, e.g.:
//
//	OPENCODE_SMOKE_DIR=$HOME/.local/share/opencode go test ./internal/opencode/... -run TestSmokeRealDir -v
func TestSmokeRealDir(t *testing.T) {
	dir := os.Getenv("OPENCODE_SMOKE_DIR")
	if dir == "" {
		t.Skip("OPENCODE_SMOKE_DIR not set")
	}
	dbPath := filepath.Join(dir, "opencode.db")
	before, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	r, err := Collect(context.Background(), Options{Dir: dir, ProfileID: "smoke"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sessions: %d", len(r.Sessions))
	t.Logf("complete: %v", r.Complete)
	for _, w := range r.Warnings {
		t.Logf("warning: %s", w)
	}

	type key struct{ provider, model string }
	requests := map[key]int64{}
	cost := map[key]float64{}
	costKnown := map[key]bool{}
	seen := map[key]bool{}
	for _, b := range r.Usage {
		k := key{b.Provider, b.Model}
		requests[k] += b.Requests
		cost[k] += b.CostUSD
		if !seen[k] {
			costKnown[k] = b.CostKnown
			seen[k] = true
		} else {
			costKnown[k] = costKnown[k] && b.CostKnown
		}
	}
	keys := make([]key, 0, len(requests))
	for k := range requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].provider != keys[j].provider {
			return keys[i].provider < keys[j].provider
		}
		return keys[i].model < keys[j].model
	})
	for _, k := range keys {
		t.Logf("  %-12s %-30s requests=%-5d cost=$%.4f known=%v", k.provider, k.model, requests[k], cost[k], costKnown[k])
	}

	after, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("database mtime changed: %v -> %v", before.ModTime(), after.ModTime())
	}
}
