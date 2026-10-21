package codex

// billingDetail deliberately does not accept quota snapshots, timestamps, or
// current account settings. None identify a historical request's billing
// route. A request can finish after a cap is reached, snapshots can be stale,
// and allowance may be shared with other clients. Credit balance changes also
// cannot attribute a charge to a particular local request.
//
// The supported rollout formats contain no explicit included/outside-allowance
// classification. Keep every request unknown rather than invent a classifier
// for fields the provider does not supply. Adding a future supported billing
// source requires evidence linked to the individual request and its account.
func billingDetail(usageKnown bool, models []ModelUsage) BillingDetail {
	result := BillingDetail{UsageKnown: usageKnown}
	for _, m := range models {
		result.UnknownRequests += m.Requests
		result.Models = append(result.Models, BillingModel{
			Model: m.Model, Input: m.Input, Output: m.Output,
			CachedInput: m.CachedInput, CacheWrite: m.CacheWrite, Reasoning: m.Reasoning,
			UnknownRequests: m.Requests,
		})
	}
	return result
}
