// Package opencode reads opencode's own sqlite database (opencode.db, under
// a data directory such as ~/.local/share/opencode) and turns its sessions
// and messages into db.SessionRow values plus per-day/provider/model usage
// buckets. opencode talks to many pay-as-you-go inference providers through
// its own bundled rate card; the cost it records is its own estimate from
// provider token counts, never a bill.
//
// opencode records cost 0 both for a genuinely free model and for a paid
// model it has no price for, and the two are indistinguishable from a
// single request. Collect resolves that ambiguity by first finding every
// provider that has priced at least one request in the database, then
// treating a same-provider cost-0 request that used tokens as unknown
// rather than free; see collect.go's classifyCost for the rule in full.
//
// Collect opens opencode.db read-only and never writes, checkpoints, or
// vacuums it. opencode keeps the database in WAL mode and may be writing to
// it concurrently while Collect reads.
package opencode

import (
	"context"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/payg"
)

// Options selects which opencode data directory and profile identity to
// collect.
type Options struct {
	Dir       string // opencode data directory holding opencode.db, e.g. ~/.local/share/opencode
	ProfileID string // stable registry profile id
}

// Result is one collection pass over opencode.db. Sessions is emitted even
// for a session with zero counted requests; Usage aggregates every counted
// request regardless of which session it belongs to, including a request
// whose session_id names no row in the session table.
type Result struct {
	Sources  map[string]db.SessionSource
	Sessions []db.SessionRow
	Usage    []payg.Bucket // per local day x canonical provider x canonical model
	Warnings []string
	Complete bool // false if any row could not be read or any JSON was malformed
}

// SessionID builds the stable, provider-prefixed row identity a session's
// native id is stored under.
func SessionID(profileID, sessionID string) string { return "opencode:" + profileID + ":" + sessionID }

// Collect runs a one-shot collection with a fresh, empty Collector.
func Collect(ctx context.Context, opts Options) (Result, error) {
	return NewCollector().Collect(ctx, opts)
}
