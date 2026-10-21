package server

import (
	"fmt"
	"math"
	"strconv"

	pb "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
)

// Codex spends credits only once a member's included allowance is used up
// (OpenAI's Codex pricing: "After you reach your included limits, available
// credits let you continue working"). The individual credit limit's used
// amount is therefore the account's overage for the limit's current period,
// in credits. It says nothing about which recorded requests spent them, so
// request-level billing classification stays unknown.
const codexCreditNote = "Credits Codex reports as used under this member's individual credit limit. Codex spends credits only once the included allowance is used up."

const codexNoCreditNote = "Codex reported no individual credit limit for this account, so its credit use is unavailable. Codex does not report billed dollars; included and unclassified usage have no monetary value."

// codexCreditOverage reads the credits used under the individual credit
// limit and when that limit's period resets. The "codex" bucket is the
// account's own meter, so it wins over any other bucket carrying a limit;
// buckets are never summed, since one limit can be mirrored in several.
func codexCreditOverage(buckets []*pb.CodexLimitBucket) (credits, resetsAt float64, ok bool) {
	var picked *pb.CodexIndividualLimit
	for _, b := range buckets {
		if b.GetIndividualLimit() == nil {
			continue
		}
		if picked == nil || b.LimitId == "codex" {
			picked = b.IndividualLimit
		}
		if b.LimitId == "codex" {
			break
		}
	}
	if picked == nil {
		return 0, 0, false
	}
	used, err := strconv.ParseFloat(picked.Used, 64)
	if err != nil || used < 0 || math.IsInf(used, 0) || math.IsNaN(used) {
		return 0, 0, false
	}
	return used, picked.ResetsAt, true
}

// creditPriceNote says how the dollar figure was reached, or how to get one.
func creditPriceNote(price float64) string {
	if price <= 0 {
		return " Set CODEX_CREDIT_USD in config.env to estimate dollars."
	}
	return fmt.Sprintf(" The dollar figure prices each credit at $%s (CODEX_CREDIT_USD): an estimate, not an invoice.", strconv.FormatFloat(price, 'f', -1, 64))
}

// fillCodexCreditBilling reports a Codex account's credit overage, and its
// dollar estimate when a price per credit is configured. An account whose
// limit was not reported keeps the unavailable billing it already has.
func fillCodexCreditBilling(a *pb.AccountSnapshot, price float64) {
	credits, resetsAt, ok := codexCreditOverage(a.CodexBuckets)
	if !ok {
		return
	}
	b := a.Billing
	b.ReportedOverageCredits = number(credits)
	if resetsAt > 0 {
		b.PeriodResetsAt = number(resetsAt)
	}
	b.Period = "Current credit period at observation"
	b.ObservedAt = a.ObservedAt
	b.State = a.State
	b.Note = codexCreditNote + creditPriceNote(price)
	if price > 0 {
		b.EstimatedOverageUsd = number(credits * price)
		b.CreditPriceUsd = number(price)
		b.Currency = "USD"
		b.PricingSource = "CODEX_CREDIT_USD in config.env"
	}
}

// fillCodexCreditSummary adds the credit overage of every account the
// monitoring view shows, each counted once, to its billing summary.
func fillCodexCreditSummary(s *pb.CodexBillingSummary, accounts []*pb.CodexAccountUsage, price float64) {
	total, any := 0.0, false
	for _, a := range accounts {
		if credits, _, ok := codexCreditOverage(a.Buckets); ok {
			total += credits
			any = true
		}
	}
	if !any {
		return
	}
	s.ReportedOverageCredits = number(total)
	s.Note = "Credits Codex reports as used under each account's individual credit limit. Codex spends credits only once the included allowance is used up, but does not say which recorded requests spent them." + creditPriceNote(price)
	if price > 0 {
		s.EstimatedOverageUsd = number(total * price)
		s.CreditPriceUsd = number(price)
	}
}
