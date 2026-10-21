// Package omp reads omp (oh-my-pi, @oh-my-pi/pi-coding-agent) session
// transcripts under <home>/sessions/ and turns them into db.SessionRow
// values plus per-day/provider/model usage buckets. omp talks to many
// pay-as-you-go inference providers (Baseten, Fireworks, LM Studio and
// others); the cost it records is its own estimate from the provider's
// token counts times its bundled rate card, never a bill. A missing
// cost means omp had no price for the model, so that request's cost is
// unknown rather than zero. See docs/omp-provider.md for the on-disk
// format and the parsing rules this package implements.
//
// Collect never mutates or deletes anything under Home; it only reads.
package omp

import (
	"context"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/payg"
)

// Options selects which omp home and profile identity to collect.
type Options struct {
	Home      string // omp agent home, e.g. ~/.omp/agent (sessions live in Home/sessions)
	ProfileID string // stable registry profile id
}

// Result is one collection pass over every session transcript found under
// Options.Home. Sessions is emitted even for a transcript with zero counted
// requests; Usage aggregates every counted request regardless of which
// session it belongs to.
type Result struct {
	Sources  map[string]db.SessionSource
	Sessions []db.SessionRow
	Usage    []payg.Bucket // per local day x inference provider x canonical model
	Warnings []string
	Complete bool // false if any file could not be read or any line was unparseable-but-complete
}

// SessionID builds the stable, provider-prefixed row identity a session's
// native id is stored under.
func SessionID(profileID, sessionID string) string { return "omp:" + profileID + ":" + sessionID }

// Collect runs a one-shot collection with a fresh, empty Collector.
func Collect(ctx context.Context, opts Options) (Result, error) {
	return NewCollector().Collect(ctx, opts)
}
