package rules

import "fmt"

// IndexedSession is the provider-neutral subset whose coverage is explicit.
type IndexedSession struct {
	ID                                     string
	Requests, Errors                       int
	UsageKnown, ErrorsKnown, HasTranscript bool
}

type IndexedRule func([]IndexedSession) Finding

var indexedRules = []IndexedRule{HistoryCoverage, RecordedFailures, ReviewLargestSession}

func RunIndexed(rows []IndexedSession) []Finding {
	out := make([]Finding, 0, len(indexedRules))
	for _, rule := range indexedRules {
		out = append(out, rule(rows))
	}
	return out
}
func HistoryCoverage(rows []IndexedSession) Finding {
	if len(rows) == 0 {
		return finding("history-coverage", "insufficient", "No sessions in this selection", "No indexed session evidence is available.", "Import history in Settings.", "indexed history", 0)
	}
	missingUsage, missingTranscripts := 0, 0
	for _, r := range rows {
		if !r.UsageKnown {
			missingUsage++
		}
		if !r.HasTranscript {
			missingTranscripts++
		}
	}
	severity := "clean"
	if missingUsage > 0 || missingTranscripts > 0 {
		severity = "insufficient"
	}
	return finding("history-coverage", severity, "Evidence coverage", fmt.Sprintf("%d sessions; %d without complete token observations; %d without local transcripts.", len(rows), missingUsage, missingTranscripts), "Treat missing evidence as unknown. Session analysis reports what it actually reads.", "indexed history", 0)
}
func RecordedFailures(rows []IndexedSession) Finding {
	known, total := 0, 0
	worst := IndexedSession{}
	for _, r := range rows {
		if !r.ErrorsKnown {
			continue
		}
		known++
		total += r.Errors
		if r.Errors > worst.Errors {
			worst = r
		}
	}
	if known == 0 {
		return finding("recorded-errors", "insufficient", "Recorded failures", "This selection has no complete error observations.", "Use transcript analysis to review failure recovery.", "indexed history", 0)
	}
	if total == 0 {
		return finding("recorded-errors", "clean", "No recorded failures", fmt.Sprintf("No errors in %d sessions with error coverage; %d sessions remain unassessed.", known, len(rows)-known), "Keep verification steps that demonstrate successful outcomes.", "indexed history", 0)
	}
	return finding("recorded-errors", "info", "Review failure recovery", fmt.Sprintf("%d recorded errors across %d sessions with error coverage; %s records %d. Errors alone do not establish wasted work.", total, known, worst.ID, worst.Errors), "Analyse the session and check whether retries changed the approach and verified recovery.", "indexed history", 0)
}
func ReviewLargestSession(rows []IndexedSession) Finding {
	known := 0
	largest := IndexedSession{}
	for _, r := range rows {
		if !r.UsageKnown {
			continue
		}
		known++
		if r.Requests > largest.Requests {
			largest = r
		}
	}
	if known == 0 {
		return finding("review-largest-session", "insufficient", "Session request counts", "No complete request observations in this selection.", "Import available history before comparing session activity.", "indexed history", 0)
	}
	if largest.Requests < 100 {
		return finding("review-largest-session", "clean", "Session request counts", fmt.Sprintf("No session with complete usage exceeds the 100-request review threshold; %d sessions are covered.", known), "Evaluate correctness and completion before optimising request counts.", "indexed history", 0)
	}
	return finding("review-largest-session", "info", "A session worth reviewing", fmt.Sprintf("%s has %d recorded model requests. Task complexity and completion are not established by this count.", largest.ID, largest.Requests), "Review task completion, corrections, repeated reads and verification before changing the workflow.", "indexed history", 0)
}
