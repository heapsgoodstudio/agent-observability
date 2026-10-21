package server

import (
	"fmt"
	"os"
	"path/filepath"

	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/report"
)

// loadReport reads one run's report out of dir, preferring the structured
// report.json and falling back to report.md. Exactly one half of the return is
// ever populated, so a caller never has to decide which wins.
//
// A report.json that will not parse is treated as absent rather than as an
// error: a corrupt file must not take an RPC down, and an older markdown report
// sitting beside it should still reach the page. The parse runs on read rather
// than trusting the file, since a half-written or hand-edited one is possible.
func loadReport(dir string) (*claudeobsv1.Report, string) {
	if data, err := os.ReadFile(filepath.Join(dir, "report.json")); err == nil {
		rep, perr := report.Parse(data)
		if perr == nil {
			return reportToProto(rep), ""
		}
		fmt.Fprintf(os.Stderr, "server: report.json in %s did not parse (%v), falling back to report.md\n", dir, perr)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "report.md")); err == nil {
		return nil, string(data)
	}
	return nil, ""
}

// reportToProto converts a validated report into its wire shape. Field for
// field, with one rename: the contract's "do" is a keyword in generated Go, so
// it travels as do_this.
func reportToProto(r *report.Report) *claudeobsv1.Report {
	out := &claudeobsv1.Report{
		Schema:         int32(r.Schema),
		Verdict:        r.Verdict,
		Avoidable:      r.Avoidable,
		Headline:       r.Headline,
		Summary:        r.Summary,
		Coverage:       r.Coverage,
		Story:          r.Story,
		AllowanceAware: r.AllowanceAware(),
	}
	for _, d := range r.CostDrivers {
		out.CostDrivers = append(out.CostDrivers, &claudeobsv1.ReportDriver{
			Label: d.Label, AmountUsd: d.AmountUSD, SharePct: d.SharePct, Plain: d.Plain,
		})
	}
	for _, p := range r.Problems {
		out.Problems = append(out.Problems, &claudeobsv1.ReportProblem{
			Title: p.Title, WhatHappened: p.WhatHappened, WhyItCosts: p.WhyItCosts,
			Fix: p.Fix, Cost: p.Cost, Evidence: p.Evidence, Confidence: p.Confidence, Category: p.Category,
		})
	}
	for _, g := range r.Good {
		out.Good = append(out.Good, &claudeobsv1.ReportGood{
			Title: g.Title, What: g.What, WhyGood: g.WhyGood,
		})
	}
	for _, a := range r.Actions {
		out.Actions = append(out.Actions, &claudeobsv1.ReportAction{
			DoThis: a.Do, Why: a.Why, Effort: a.Effort, Saving: a.Saving, Verification: a.Verification,
		})
	}
	for _, c := range r.ConfigChanges {
		out.ConfigChanges = append(out.ConfigChanges, &claudeobsv1.ReportConfigChange{
			File: c.File, Setting: c.Setting, Change: c.Change, Why: c.Why,
		})
	}
	for _, f := range r.Fine {
		out.Fine = append(out.Fine, &claudeobsv1.ReportFine{
			Title: f.Title, WhyFine: f.WhyFine,
		})
	}
	return out
}
