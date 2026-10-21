package server

import (
	"os"
	"path/filepath"
	"testing"
)

func writeReportFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadReportPrefersStructured(t *testing.T) {
	dir := t.TempDir()
	writeReportFile(t, dir, "report.json", `{"schema":1,"headline":"H","summary":"S","verdict":"mixed",
	  "cost_drivers":[{"label":"Helper agents","amount_usd":12.5,"share_pct":80}],
	  "actions":[{"do":"Cap reviews","effort":"low","saving":"large"}]}`)
	writeReportFile(t, dir, "report.md", "# old")

	rep, md := loadReport(dir)
	if md != "" {
		t.Errorf("markdown = %q, want empty when a structured report exists", md)
	}
	if rep == nil {
		t.Fatal("report = nil, want the structured report")
	}
	if rep.Headline != "H" || rep.Verdict != "mixed" {
		t.Errorf("report = %+v, want H/mixed", rep)
	}
	if len(rep.CostDrivers) != 1 || rep.CostDrivers[0].AmountUsd != 12.5 {
		t.Errorf("cost drivers = %+v, want one at 12.5", rep.CostDrivers)
	}
	if len(rep.Actions) != 1 || rep.Actions[0].DoThis != "Cap reviews" {
		t.Errorf("actions = %+v, want the do field mapped to do_this", rep.Actions)
	}
}

func TestLoadReportFallsBackToMarkdown(t *testing.T) {
	dir := t.TempDir()
	writeReportFile(t, dir, "report.md", "# only markdown")
	rep, md := loadReport(dir)
	if rep != nil {
		t.Errorf("report = %+v, want nil", rep)
	}
	if md != "# only markdown" {
		t.Errorf("markdown = %q, want the file's contents", md)
	}
}

func TestLoadReportTreatsCorruptJSONAsAbsent(t *testing.T) {
	dir := t.TempDir()
	writeReportFile(t, dir, "report.json", "{not json")
	writeReportFile(t, dir, "report.md", "# fallback")
	rep, md := loadReport(dir)
	if rep != nil {
		t.Errorf("report = %+v, want nil for a corrupt file", rep)
	}
	if md != "# fallback" {
		t.Errorf("markdown = %q, want the fallback to reach the page", md)
	}
}

func TestLoadReportEmptyDir(t *testing.T) {
	rep, md := loadReport(t.TempDir())
	if rep != nil || md != "" {
		t.Errorf("got %+v / %q, want both empty", rep, md)
	}
}
