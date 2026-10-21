package rules

import (
	"strings"
	"testing"
)

func TestIndexedRulesPreserveMissingEvidence(t *testing.T) {
	for _, f := range RunIndexed(nil) {
		if f.Severity != "insufficient" {
			t.Fatalf("empty history: %+v", f)
		}
	}
	partial := []IndexedSession{{ID: "missing", Requests: 999, Errors: 50}}
	for _, f := range RunIndexed(partial) {
		if f.Severity != "insufficient" {
			t.Fatalf("unknown data became a judgment: %+v", f)
		}
	}
	full := []IndexedSession{{ID: "known", Requests: 10, UsageKnown: true, ErrorsKnown: true, HasTranscript: true}}
	for _, f := range RunIndexed(full) {
		if f.Severity != "clean" {
			t.Fatalf("clean observation: %+v", f)
		}
	}
	full[0].Errors = 3
	full[0].Requests = 120
	for _, rule := range []IndexedRule{RecordedFailures, ReviewLargestSession} {
		f := rule(full)
		if f.Severity != "info" || !strings.Contains(f.Evidence, "known") {
			t.Fatalf("signal should identify evidence without asserting waste: %+v", f)
		}
	}
}
