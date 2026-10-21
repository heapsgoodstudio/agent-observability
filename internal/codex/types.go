// Package codex reads Codex history and the documented App Server protocol.
// It never starts/resumes a thread, executes a turn, or exports message bodies.
package codex

import (
	"context"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
)

// RPC is the small, read-only transport boundary, also used by fixture tests.
// Implementations must not log params or response bodies.
type RPC interface {
	Call(context.Context, string, any, any) error
	Close() error
}

type Options struct {
	Home, ProfileID, Binary string
	// Connect optionally supplies an already authenticated App Server transport.
	// The default starts a bounded stdio client in a temporary working directory.
	Connect func(context.Context, Options) (RPC, error)
	// LocalOnly skips App Server and reports that metadata coverage is partial.
	LocalOnly bool
	// RequestTimeout defaults to 15 seconds. Context cancellation always wins.
	RequestTimeout time.Duration
	// MaxThreads bounds one collection, defaults to 10000. Hitting the bound
	// returns partial coverage and must never delete previously indexed history.
	MaxThreads int
}

type Capabilities struct {
	AppServer, ThreadList, ThreadRead, TurnsList, ItemsList bool
	LegacyHistory, PaginatedHistory, LocalUsage             bool
	Version                                                 string
}

type Result struct {
	Sessions     []db.SessionRow
	Capabilities Capabilities
	Warnings     []string
	// Complete means all discovered metadata/history sources were traversed.
	// A session may independently have UsageKnown=false (no token detail).
	Complete bool
	// Details holds additional source/pricing coverage keyed by stable session ID.
	Details map[string]Detail
}

type Detail struct {
	TranscriptPath                         string
	UsageUnavailableReason                 string
	RejectedOnly                           bool
	HistoryMode, ForkedFromID              string
	UnpricedModels                         []string
	Models                                 []ModelUsage
	ErrorsKnown, DurationKnown, ToolsKnown bool
	Billing                                BillingDetail
}

type ModelUsage struct {
	Model                                             string
	Input, Output, CachedInput, CacheWrite, Reasoning int64
	Requests                                          int
	EstimatedUSD                                      float64
	CostKnown                                         bool
}

// BillingDetail classifies only requests for which the source explicitly
// identifies the billing route. Current Codex rollout formats do not provide
// that evidence: their rate-limit snapshots cannot classify individual
// requests. Unknown requests must never be priced as subscription overage.
// OutsideEstimateUSD is nil when no demonstrably outside-allowance usage has
// been priced; nil is not a measured zero-dollar bill.
type BillingDetail struct {
	UsageKnown, ClassificationAvailable                bool
	IncludedRequests, OutsideRequests, UnknownRequests int
	EstimatedOutsideRequests, UnpricedOutsideRequests  int
	OutsideEstimateUSD                                 *float64
	Models                                             []BillingModel
}

// BillingModel contains token activity without an all-usage dollar estimate.
// Input encompasses CachedInput and CacheWrite; Output encompasses Reasoning.
type BillingModel struct {
	Model                                              string
	Input, Output, CachedInput, CacheWrite, Reasoning  int64
	IncludedRequests, OutsideRequests, UnknownRequests int
	EstimatedOutsideRequests, UnpricedOutsideRequests  int
	OutsideEstimateUSD                                 *float64
}

// Usage is a live quota snapshot, never a bill or token-cost estimate. A failed
// read returns an error, not zero utilization. Callers retain older snapshots
// with their original FetchedAt to communicate staleness.
type Usage struct {
	FetchedAt time.Time
	// AccountID is populated only when the server explicitly returns one.
	// Empty is unknown; a profile ID must not stand in for account identity.
	AccountID, PlanType string
	Windows             []Window
	// Nil permission is unknown. Percentages and reset times cannot establish
	// permission to resume ordinary included usage.
	OrdinaryUsageAllowed *bool
	Buckets              []Bucket
	// Reset credits replenish quota; they are distinct from purchased credits.
	ResetCreditsAvailable *int64
}

// Window is the compatibility projection of a bucket's primary/secondary
// window. The Known flags distinguish unavailable duration/reset from zero.
type Window struct {
	LimitID, Name, Kind               string
	UsedPercent                       float64
	WindowMinutes, ResetsAt           int64
	WindowMinutesKnown, ResetsAtKnown bool
}

// Bucket is one independently metered allowance. Buckets must not be summed.
type Bucket struct {
	LimitID              string             `json:"limitId"`
	Name                 string             `json:"limitName"`
	PlanType             string             `json:"planType"`
	NormalModelSlug      string             `json:"normalModelSlug"`
	Primary              *QuotaWindow       `json:"primary"`
	Secondary            *QuotaWindow       `json:"secondary"`
	Credits              *Credits           `json:"credits"`
	IndividualLimit      *SpendControlLimit `json:"individualLimit"`
	SpendControlReached  *bool              `json:"spendControlReached"`
	RateLimitReachedType *string            `json:"rateLimitReachedType"`
}

type QuotaWindow struct {
	UsedPercent   float64 `json:"usedPercent"`
	WindowMinutes *int64  `json:"windowDurationMins"`
	ResetsAt      *int64  `json:"resetsAt"`
}

// Credits preserves the backend decimal string. The protocol does not state
// its currency or purchase price; balance must never be formatted as USD.
type Credits struct {
	HasCredits bool    `json:"hasCredits"`
	Unlimited  bool    `json:"unlimited"`
	Balance    *string `json:"balance"`
}

// SpendControlLimit preserves backend amounts without inferring their units.
type SpendControlLimit struct {
	Limit            string `json:"limit"`
	Used             string `json:"used"`
	RemainingPercent int32  `json:"remainingPercent"`
	ResetsAt         int64  `json:"resetsAt"`
}

func SessionID(profileID, threadID string) string { return "codex:" + profileID + ":" + threadID }
