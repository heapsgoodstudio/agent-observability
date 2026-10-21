package indexer

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// The guard on --reset-cost-series.
//
// The flag deletes the two cost series and rebuilds them by re-parsing
// transcripts, which is only safe while the transcripts that produced the
// samples still exist. Claude Code ages transcripts out after roughly 30
// days, so a series that has been accumulating for longer than that outlives
// its own source. Measured on the machine this was written against: the cost
// history ran 2026-07-16 to 2026-08-17 while the surviving transcripts began
// 2026-08-19. The two did not overlap by a single day, so a reset would have
// deleted that whole month of cost history and re-imported nothing for that
// period.
//
// Nothing about the flag said so. It reported a successful run.

// ResetSafety is what CheckResetSafety found.
type ResetSafety struct {
	// HistoryStart and OldestTranscript are Unix seconds, zero when unknown.
	HistoryStart     int64
	OldestTranscript int64
	// Unreparseable is the span the reset would delete and be unable to
	// rebuild, because no transcript covering it survives.
	Unreparseable time.Duration
	AtRisk        float64 // dollars in that span
}

// Safe reports whether a reset can rebuild everything it would delete.
func (r ResetSafety) Safe() bool { return r.Unreparseable <= 0 }

// Explain is the refusal message, written to be actionable rather than
// merely alarming: it names the span, the money, and what to do instead.
func (r ResetSafety) Explain() string {
	if r.Safe() {
		return ""
	}
	return fmt.Sprintf(
		"refusing --reset-cost-series: cost history starts %s but the oldest transcript on disk is %s, "+
			"so %.0f days of history (about $%.2f) would be deleted and could not be rebuilt. "+
			"Claude Code ages transcripts out, and a reset re-imports only what it can still parse. "+
			"To change the label set without losing history, use `claude-obs migrate-history-labels`, "+
			"which moves the existing samples through VictoriaMetrics' own export and import "+
			"rather than back through the parser. Pass --i-know-this-deletes-history to override",
		time.Unix(r.HistoryStart, 0).Format("2006-01-02"),
		time.Unix(r.OldestTranscript, 0).Format("2006-01-02"),
		r.Unreparseable.Hours()/24, r.AtRisk)
}

// CheckResetSafety compares where the cost history starts against the oldest
// transcript still on disk.
//
// It fails open on a query error: this is a guard against a known hazard, not
// an authority on whether VictoriaMetrics is reachable, and refusing a reset
// because a health check flaked would be its own kind of wrong.
func CheckResetSafety(vmURL string, roots []ProfileRoot) (ResetSafety, error) {
	var r ResetSafety

	start, err := queryTotal(vmURL, "min(timestamp(last_over_time(claude_hist_daily_cost_usd[10y])))")
	if err != nil || start <= 0 {
		return r, err
	}
	r.HistoryStart = int64(start)

	oldest, err := oldestTranscript(roots)
	if err != nil {
		return r, err
	}
	if oldest == 0 {
		// No transcripts at all: a reset would delete everything and rebuild
		// nothing, which is the most dangerous case rather than the safest.
		r.OldestTranscript = time.Now().Unix()
	} else {
		r.OldestTranscript = oldest
	}

	if r.HistoryStart >= r.OldestTranscript {
		return r, nil
	}
	r.Unreparseable = time.Duration(r.OldestTranscript-r.HistoryStart) * time.Second

	// The dollars in the span that cannot be rebuilt, so the refusal can say
	// what is actually at stake rather than only how many days.
	atRisk, err := queryTotal(vmURL, fmt.Sprintf(
		"sum(sum_over_time(claude_hist_daily_cost_usd[%ds] @ %d))",
		int64(r.Unreparseable.Seconds()), r.OldestTranscript))
	if err == nil {
		r.AtRisk = atRisk
	}
	return r, nil
}

// oldestTranscript is the earliest modification time across every profile's
// transcripts, or zero when there are none.
func oldestTranscript(roots []ProfileRoot) (int64, error) {
	var oldest int64
	for _, root := range roots {
		err := filepath.WalkDir(root.ProjectsRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // an unreadable subtree is not evidence about dates
			}
			if d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			ts := info.ModTime().Unix()
			if oldest == 0 || ts < oldest {
				oldest = ts
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return oldest, nil
}
