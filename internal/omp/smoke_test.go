package omp

import (
	"context"
	"os"
	"sort"
	"testing"
)

// TestSmokeRealHome runs a real, read-only Collect against a real omp home,
// for hand verification against the actual on-disk data. It is skipped
// unless OMP_SMOKE_HOME is set, e.g.:
//
//	OMP_SMOKE_HOME=$HOME/.omp/agent go test ./internal/omp/... -run TestSmokeRealHome -v
func TestSmokeRealHome(t *testing.T) {
	home := os.Getenv("OMP_SMOKE_HOME")
	if home == "" {
		t.Skip("OMP_SMOKE_HOME not set")
	}
	r, err := Collect(context.Background(), Options{Home: home, ProfileID: "smoke"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sessions: %d", len(r.Sessions))
	t.Logf("complete: %v", r.Complete)
	for _, w := range r.Warnings {
		t.Logf("warning: %s", w)
	}
	var total float64
	byModel := map[string]float64{}
	for _, row := range r.Sessions {
		total += row.CostUSD
		for _, m := range row.Models {
			byModel[m.Model] += m.CostUSD
		}
	}
	t.Logf("total cost: $%.4f", total)
	names := make([]string, 0, len(byModel))
	for name := range byModel {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.Logf("  %-40s $%.4f", name, byModel[name])
	}
}
