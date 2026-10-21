// Package payg holds what every pay-as-you-go harness shares: the per day,
// provider and model spend buckets a harness collector produces, the
// canonical provider and model names they are keyed on, and the rule for
// merging a fresh collection into the one already stored. Oh My Pi
// (internal/omp) and opencode (internal/opencode) both write these, so the
// dashboard reads one shape whichever harness made the requests.
package payg

import "sort"

// Bucket is one local day's requests to one model of one inference provider.
// The JSON tags are the stored form in each harness's snapshot and must not
// change: a stored snapshot is decoded with them after an upgrade.
type Bucket struct {
	Day        string  `json:"day"`      // local calendar date YYYY-MM-DD of the request timestamp
	Provider   string  `json:"provider"` // CanonicalProvider of the request's provider
	Model      string  `json:"model"`    // CanonicalModel of the request's model
	Requests   int64   `json:"requests"`
	Input      int64   `json:"input"`  // excludes cache reads and writes
	Output     int64   `json:"output"` // includes reasoning
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Reasoning  int64   `json:"reasoning"`
	CostUSD    float64 `json:"cost_usd"`   // sum of known costs only
	CostKnown  bool    `json:"cost_known"` // false if any request in the bucket had no price
}

// Merge keeps, per day, provider and model, whichever reading counted more
// requests, the fresh one on a tie. A bucket only grows while its source data
// exists, so a smaller one means logs were deleted or unreadable, and that
// cannot un-spend money.
func Merge(old, fresh []Bucket) []Bucket {
	type key struct{ day, provider, model string }
	byKey := map[key]Bucket{}
	var order []key
	for _, set := range [][]Bucket{old, fresh} {
		for _, b := range set {
			k := key{b.Day, b.Provider, b.Model}
			prev, ok := byKey[k]
			if !ok {
				order = append(order, k)
			}
			if !ok || b.Requests >= prev.Requests {
				byKey[k] = b
			}
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.day != b.day {
			return a.day < b.day
		}
		if a.provider != b.provider {
			return a.provider < b.provider
		}
		return a.model < b.model
	})
	out := make([]Bucket, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}
