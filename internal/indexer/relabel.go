package indexer

// Relabelling history in place, without going back through the parser.
//
// Adding a label to a series changes its identity, and the repository's
// existing answer to that is `backfill --reset-cost-series`: delete the two
// cost series and rebuild them from transcripts. That is only safe while the
// transcripts that produced the samples still exist, and Claude Code ages
// transcripts out after roughly 30 days. Measured on the machine this was
// written against: the cost history spanned 2026-07-16 to 2026-08-17 and the
// surviving transcripts began 2026-08-19, so the two did not overlap by a
// single day. A reset would have deleted that whole month of cost history and
// re-imported nothing for that period.
//
// So the samples travel through VictoriaMetrics' own export and import
// instead. Values round-trip exactly, no transcript is consulted, and the
// totals are compared before and after with a failure if they differ.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RelabelOpts configures one RelabelHistory pass.
type RelabelOpts struct {
	VMURL string
	// Label and Value are added to every series of every metric in Metrics.
	// A series that already carries Label is left exactly as it is, so a
	// half-finished run can be repeated.
	Label, Value string
	// Metrics defaults to the two cost series. The token series carry no
	// project or workspace label, so nothing about them needs relabelling.
	Metrics []string
	// DryRun exports and rewrites but neither deletes nor imports, and still
	// reports what the totals would be. It is how this is rehearsed.
	DryRun bool
	// BackupDir receives a copy of the rewritten lines before anything is
	// deleted. Empty skips it, which only a test should do.
	BackupDir string
}

// RelabelStats is what the command prints.
type RelabelStats struct {
	Metric        string
	Series        int
	Samples       int
	Skipped       int // series that already carried the label
	Before, After float64
	DryRun        bool
	Backup        string // where the pre-delete dump was written
}

// exported is one line of VictoriaMetrics' /api/v1/export: a metric's labels
// plus its values and their timestamps.
type exported struct {
	Metric     map[string]string `json:"metric"`
	Values     []float64         `json:"values"`
	Timestamps []int64           `json:"timestamps"`
}

// RelabelHistory adds Label=Value to every series of each metric.
//
// The order matters and is not negotiable: export everything and sum it,
// rewrite in memory, delete, import, then compare. Deleting before the
// export is held in memory would put the only copy of unreparseable history
// behind a network call that might fail.
func RelabelHistory(opts RelabelOpts) ([]RelabelStats, error) {
	if opts.Label == "" || opts.Value == "" {
		return nil, fmt.Errorf("indexer: relabel needs both a label and a value")
	}
	metrics := opts.Metrics
	if len(metrics) == 0 {
		metrics = costSeries
	}
	if err := healthCheck(opts.VMURL); err != nil {
		return nil, fmt.Errorf("indexer: VictoriaMetrics not reachable at %s: %w", opts.VMURL, err)
	}

	out := make([]RelabelStats, 0, len(metrics))
	for _, m := range metrics {
		st, err := relabelOne(opts, m)
		if err != nil {
			return out, err
		}
		out = append(out, st)
	}
	return out, nil
}

func relabelOne(opts RelabelOpts, metric string) (RelabelStats, error) {
	st := RelabelStats{Metric: metric, DryRun: opts.DryRun}

	series, err := exportSeries(opts.VMURL, metric)
	if err != nil {
		return st, fmt.Errorf("indexer: export %s: %w", metric, err)
	}
	if len(series) == 0 {
		return st, nil
	}

	var lines []string
	for _, s := range series {
		st.Series++
		st.Samples += len(s.Values)
		for _, v := range s.Values {
			st.Before += v
		}
		if s.Metric[opts.Label] != "" {
			// Already carries it. Rewriting would be a no-op, but counting it
			// separately is what makes a repeat run legible rather than
			// looking like it did nothing.
			st.Skipped++
		}
		lines = append(lines, seriesToPromLines(s, opts.Label, opts.Value)...)
	}

	if opts.DryRun {
		st.After = st.Before
		return st, nil
	}

	// The dump goes to disk BEFORE anything is deleted, and the path travels
	// on the stats so every message can name it. The samples being moved may
	// be the only copy: once the transcripts behind them have aged out, a
	// delete with the export held only in this process's memory is one failed
	// write away from unrecoverable.
	if opts.BackupDir != "" {
		path, err := writeDump(opts.BackupDir, metric, lines)
		if err != nil {
			return st, fmt.Errorf("indexer: could not write the pre-delete backup, refusing to delete %s: %w", metric, err)
		}
		st.Backup = path
	}

	if err := deleteSeries(opts.VMURL, metric); err != nil {
		return st, fmt.Errorf("indexer: delete %s: %w", metric, err)
	}
	for _, batch := range batchLines(lines, BatchLines) {
		if err := postBatch(opts.VMURL, batch); err != nil {
			return st, fmt.Errorf("indexer: import %s: %w", metric, err)
		}
	}

	after, err := settledTotal(opts.VMURL, metric, st.Before)
	if err != nil {
		return st, err
	}
	st.After = after
	if !withinTolerance(after, st.Before) {
		return st, fmt.Errorf(
			"indexer: %s totals disagree after relabelling: %.2f before, %.2f after. "+
				"Restore with:  curl --data-binary @%s %s/api/v1/import",
			metric, st.Before, after, st.Backup, opts.VMURL)
	}
	return st, nil
}

// writeDump saves the rewritten exposition lines beside the install, named
// for the metric and the minute, so two runs cannot overwrite each other.
func writeDump(dir, metric string, lines []string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s.%s.prom", metric, time.Now().Format("20060102-150405")))
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// exportSeries reads every sample of one metric out of VictoriaMetrics.
// start=0 rather than a window: the whole point is history older than
// anything a default range would reach.
func exportSeries(vmURL, metric string) ([]exported, error) {
	q := url.Values{
		"match[]": {fmt.Sprintf(`{__name__=%q}`, metric)},
		"start":   {"0"},
		"end":     {fmt.Sprintf("%d", time.Now().Unix())},
	}
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(vmURL + "/api/v1/export?" + q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("export returned %s", resp.Status)
	}

	var out []exported
	sc := bufio.NewScanner(resp.Body)
	// Export lines carry every sample of one series, so a busy series' line
	// is far longer than bufio's default 64 KiB ceiling.
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e exported
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("decode export line: %w", err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// seriesToPromLines renders one exported series back as Prometheus
// text-exposition lines with the extra label added, one line per sample so
// each keeps its own timestamp.
func seriesToPromLines(s exported, label, value string) []string {
	name := s.Metric["__name__"]
	labels := make([]string, 0, len(s.Metric))
	for k, v := range s.Metric {
		if k == "__name__" || k == label {
			continue
		}
		labels = append(labels, fmt.Sprintf("%s=%q", k, esc(v)))
	}
	labels = append(labels, fmt.Sprintf("%s=%q", label, esc(value)))
	sort.Strings(labels)

	lines := make([]string, 0, len(s.Values))
	for i, v := range s.Values {
		ts := int64(0)
		if i < len(s.Timestamps) {
			ts = s.Timestamps[i]
		}
		lines = append(lines, fmt.Sprintf("%s{%s} %s %d", name, strings.Join(labels, ","), formatValue(v), ts))
	}
	return lines
}

// formatValue renders a float without exponent notation, which the
// Prometheus text format does not accept, and without trailing zeros.
func formatValue(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%.0f", v)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.10f", v), "0"), ".")
}

// settledTotal polls the metric's total until it stops moving, for the same
// reason reconcile does: freshly imported samples are not queryable
// immediately, and a read taken too early reports a partial figure that looks
// exactly like data loss.
func settledTotal(vmURL, metric string, want float64) (float64, error) {
	query := fmt.Sprintf("sum(sum_over_time(%s[10y]))", metric)
	start := time.Now()
	deadline := start.Add(reconcileTimeout)
	for {
		got, err := queryTotal(vmURL, query)
		if err != nil {
			return 0, err
		}
		if withinTolerance(got, want) {
			return got, nil
		}
		// Deliberately NOT the stability heuristic backfill's reconcile uses.
		// A large import climbs through many intermediate totals on its way
		// up, and any of them can hold still across two 500ms polls, so
		// "stable" is not evidence that ingestion has finished. Accepting one
		// reported a $963 shortfall against an import that was in fact
		// complete, and the caller's failure path tells an operator their
		// history is gone. Here the only acceptances are "the totals match"
		// and "the deadline expired", which turns a false alarm into a slower
		// success.
		if !time.Now().Before(deadline) {
			return got, nil
		}
		time.Sleep(reconcileInterval)
	}
}
