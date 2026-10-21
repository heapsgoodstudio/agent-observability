package indexer

// Backfill: walk every transcript under PROFILE_DIR/projects (nested
// subagent files merged, the same parser as RunIndex), aggregate every
// billable request into four claude_hist_* series, and import them into
// VictoriaMetrics. Idempotent: identical
// series+timestamp lines overwrite under VM dedup, so rerunning after an
// index pass or a prior backfill cannot double count. Lives in this package
// (not internal/transcript) for the same import-cycle reason RunIndex does:
// internal/transcript stays a pure parsing leaf.

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/pricing"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
	"github.com/heapsgoodstudio/agent-observability/internal/workspace"
)

// BatchLines caps each import POST at this many lines, so VM's Prometheus
// import endpoint is never asked to swallow one arbitrarily large body.
const BatchLines = 20000

// BackfillOpts configures one RunBackfill pass. It carries no config.env
// lookup of its own: the CLI resolves ProjectsRoot and VMURL from config.env
// the same way cmd/claude-obs's index subcommand resolves
// ProjectsRoot/OutRoot/DB, then passes the resolved values in here.
type BackfillOpts struct {
	ProjectsRoot string // <profile>/projects, one dir per project, *.jsonl transcripts inside
	VMURL        string // VictoriaMetrics base URL, e.g. http://127.0.0.1:8428
	DryRun       bool   // compute and return totals only; skip every VM call
	// Workspace resolves each session's display name for the workspace label.
	// Nil means workspace.NewGitResolver, matching IndexOpts.
	Workspace workspace.Resolver
	// ResetCostSeries deletes claude_hist_cost_usd and
	// claude_hist_daily_cost_usd from VictoriaMetrics before importing.
	//
	// This exists for label-set changes and nothing else. The normal
	// idempotency guarantee is VM dedup: identical series+timestamp lines
	// overwrite. Adding or removing a label changes the series identity, so
	// the old samples are not overwritten, and a query summing the metric
	// then reads the old series plus the new one, double counting every
	// historical dollar. Deleting first is the only way to land a label
	// change cleanly. Everything deleted is recomputed from transcripts by
	// the same run, so a failure between the delete and the import costs a
	// re-run, not the history.
	ResetCostSeries bool
	// Profile labels every cost sample this run writes. Empty writes the
	// unlabelled series, which is what a caller predating profiles does and
	// what keeps its output identical.
	//
	// Changing this on an existing install is a label-set change, which a
	// plain re-run cannot land: see RelabelHistory, and the guard on
	// ResetCostSeries.
	Profile string
}

// BackfillStats is main()'s printed summary, ported. VMTotal and Warning are
// only meaningful when Reconciled is true; a dry run leaves both zero/false.
type BackfillStats struct {
	Requests                                                       int
	Total                                                          float64
	CostSamples, TokenSamples, DailyCostSamples, DailyTokenSamples int
	Reconciled                                                     bool
	VMTotal                                                        float64
	Warning                                                        bool
	Reset                                                          bool // the two cost series were deleted before importing
}

// RunBackfill performs one backfill pass, mirroring main(): collect every
// request into the four series maps, then (unless DryRun) push them into
// VictoriaMetrics and reconcile the imported total against the computed one.
func RunBackfill(opts BackfillOpts) (BackfillStats, error) {
	resolver := opts.Workspace
	if resolver == nil {
		resolver = workspace.NewGitResolver()
	}
	cost, tokens, daily, dailyTokens, total, n, err := collect(opts.ProjectsRoot, opts.Profile, resolver)
	if err != nil {
		return BackfillStats{}, fmt.Errorf("indexer: collect: %w", err)
	}
	stats := BackfillStats{
		Requests: n, Total: total,
		CostSamples: len(cost), TokenSamples: len(tokens),
		DailyCostSamples: len(daily), DailyTokenSamples: len(dailyTokens),
	}
	if opts.DryRun {
		return stats, nil
	}

	if err := healthCheck(opts.VMURL); err != nil {
		return stats, fmt.Errorf("indexer: VictoriaMetrics not reachable at %s: %w", opts.VMURL, err)
	}
	if opts.ResetCostSeries {
		for _, m := range costSeries {
			if err := deleteSeries(opts.VMURL, m); err != nil {
				return stats, fmt.Errorf("indexer: delete %s: %w", m, err)
			}
		}
		stats.Reset = true
	}
	lines := toLines(cost, tokens, daily, dailyTokens)
	for _, batch := range batchLines(lines, BatchLines) {
		if err := postBatch(opts.VMURL, batch); err != nil {
			return stats, fmt.Errorf("indexer: import batch: %w", err)
		}
	}
	if err := resetRollupCache(opts.VMURL); err != nil {
		return stats, fmt.Errorf("indexer: reset rollup cache: %w", err)
	}
	raw, warn, err := reconcile(opts.VMURL, total)
	if err != nil {
		return stats, fmt.Errorf("indexer: reconcile query: %w", err)
	}
	stats.Reconciled = true
	stats.VMTotal = raw
	stats.Warning = warn
	return stats, nil
}

// Reconciliation polling knobs. Vars rather than consts so tests can shrink
// them; nothing outside this file and its tests writes them.
var (
	reconcileInterval    = 500 * time.Millisecond
	reconcileMinSettle   = 2 * time.Second
	reconcileTimeout     = 90 * time.Second
	reconcileStableReads = 2
)

const (
	reconcileTolerance = 0.01
	reconcileQuery     = "sum(sum_over_time(claude_hist_cost_usd[10y]))"
)

// costSeries names the two series carrying the project/workspace labels, the
// ones ResetCostSeries deletes. The token series are deliberately not here:
// they carry no such label, so a label change never orphans them and
// deleting them would be pure risk for no gain.
var costSeries = []string{"claude_hist_cost_usd", "claude_hist_daily_cost_usd"}

// withinTolerance reports whether the imported total is close enough to the
// computed one to call the import reconciled. A zero computed total has
// nothing to compare against, so it never warns, matching the previous
// `total > 0 &&` guard.
func withinTolerance(raw, total float64) bool {
	if total <= 0 {
		return true
	}
	return math.Abs(raw-total)/total <= reconcileTolerance
}

// reconcile polls the imported total until it either matches the computed
// total or stops moving, and reports whether the result should warn.
//
// This replaces a fixed one second sleep, which was never long enough: freshly imported samples only become queryable after
// VM's own ingestion and index lag (this install also runs with
// -search.latencyOffset=1s), which for a six figure sample count trails the
// final POST by tens of seconds. The old code therefore read a partial total
// and warned about a perfectly good import. Measured on a 135k-sample import:
// the one second read returned a total 18% short of the computed one, and
// on a second run, immediately after the cache reset, it returned $0.00. Both
// settled to the exact computed figure within about twenty seconds.
//
// Polling on the value rather than sleeping longer keeps a healthy import
// fast (it returns as soon as the totals agree) while still reaching a
// verdict on a genuine mismatch: an unchanged reading across
// reconcileStableReads consecutive polls, no earlier than reconcileMinSettle,
// means ingestion has finished and the difference is real.
func reconcile(vmURL string, total float64) (raw float64, warn bool, err error) {
	start := time.Now()
	deadline := start.Add(reconcileTimeout)
	var last float64
	var stable int
	first := true
	for {
		raw, err = queryTotal(vmURL, reconcileQuery)
		if err != nil {
			return 0, false, err
		}
		if withinTolerance(raw, total) {
			return raw, false, nil
		}
		if !first && raw == last {
			stable++
		} else {
			stable = 0
		}
		last, first = raw, false

		// A reading of exactly 0 against a non-zero computed total is never
		// "settled", however many polls in a row it holds: it means the
		// samples are not queryable yet, which is normal for seconds after an
		// import and for longer after --reset-cost-series, since the delete's
		// tombstones are applied before the new samples surface. Without this
		// the stability heuristic accepts a stable 0 and warns about a
		// perfectly good import (measured: a reset run reported $0.00 against
		// a computed total that was queryable and exact moments later).
		// A genuinely empty import still reports, via the deadline below.
		settled := stable >= reconcileStableReads && time.Since(start) >= reconcileMinSettle
		if raw == 0 && total != 0 {
			settled = false
		}
		if settled || !time.Now().Before(deadline) {
			return raw, true, nil
		}
		time.Sleep(reconcileInterval)
	}
}

// --- collect ---------------------------------------------------------------

// costKey, tokenKey, dailyKey and dailyTokenKey are collect's four dedup
// keys. Pre-summing by (labels..., timestamp) keeps idempotency under VM
// dedup: colliding rows share one sample carrying the full value.
type costKey struct {
	Model, Project, Workspace, Sidechain, Profile string
	TSMs                                          int64
}
type tokenKey struct {
	Model, Type string
	TSMs        int64
}
type dailyKey struct {
	Day, Model, Project, Workspace, Sidechain, Profile string
}
type dailyTokenKey struct {
	Day, Model, Type string
}

// collect walks every primary transcript under projectsRoot (nested subagent
// files merged, same *.jsonl glob shape as RunIndex) and aggregates every
// timestamped, billable request into the four series maps. Requests with no
// timestamp are skipped: there is no bucket to put them in.
func collect(projectsRoot, profile string, resolver workspace.Resolver) (cost map[costKey]float64, tokens map[tokenKey]int64, daily map[dailyKey]float64, dailyTokens map[dailyTokenKey]int64, total float64, n int, err error) {
	cost = map[costKey]float64{}
	tokens = map[tokenKey]int64{}
	daily = map[dailyKey]float64{}
	dailyTokens = map[dailyTokenKey]int64{}

	paths, err := globSorted(projectsRoot, "*", "*.jsonl")
	if err != nil {
		return nil, nil, nil, nil, 0, 0, fmt.Errorf("glob transcripts: %w", err)
	}
	for _, path := range paths {
		sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		nested, err := globSorted(filepath.Join(filepath.Dir(path), sid, "subagents"), "*.jsonl")
		if err != nil {
			return nil, nil, nil, nil, 0, 0, fmt.Errorf("glob subagents for %s: %w", sid, err)
		}
		scanned, err := transcript.Scan(path, nested)
		if err != nil {
			return nil, nil, nil, nil, 0, 0, fmt.Errorf("scan %s: %w", path, err)
		}
		project := filepath.Base(filepath.Dir(path))
		// Same derivation RunIndex writes into the sessions table's workspace
		// column, so a session's dollars in the history series and its row on
		// the sessions page carry the identical name.
		ws := workspaceName(scanned.Cwds, project, resolver)
		for _, r := range scanned.Requests {
			if r.Timestamp == "" {
				continue
			}
			c, _ := pricing.RequestCost(r.Model, r.InputTokens, r.OutputTokens, r.CacheRead, r.Cache5m, r.Cache1h, r.WebSearchRequests, r.Speed, r.Tier)
			tsMs, err := iso2epochMs(r.Timestamp)
			if err != nil {
				return nil, nil, nil, nil, 0, 0, fmt.Errorf("%s: %w", path, err)
			}
			side := "0"
			if r.Sidechain {
				side = "1"
			}
			cost[costKey{r.Model, project, ws, side, profile, tsMs}] += c
			day := dayOf(tsMs)
			for _, tv := range [4]struct {
				typ string
				v   int64
			}{
				{"input", r.InputTokens}, {"output", r.OutputTokens},
				{"cacheRead", r.CacheRead}, {"cacheCreation", r.Cache5m + r.Cache1h},
			} {
				if tv.v != 0 {
					tokens[tokenKey{r.Model, tv.typ, tsMs}] += tv.v
					dailyTokens[dailyTokenKey{day, r.Model, tv.typ}] += tv.v
				}
			}
			daily[dailyKey{day, r.Model, project, ws, side, profile}] += c
			total += c
			n++
		}
	}
	return cost, tokens, daily, dailyTokens, total, n, nil
}

// iso2epochMs parses a transcript timestamp into Unix milliseconds,
// truncated to the whole second first. Sub-second precision is discarded
// deliberately: the series these stamps feed are bucketed no finer than a
// second, and dropping it here keeps a re-import landing on the identical
// timestamp. It duplicates internal/transcript's unexported,
// already-seconds-only iso2epoch rather than exporting it.
func iso2epochMs(ts string) (int64, error) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0, fmt.Errorf("bad timestamp %q: %w", ts, err)
	}
	return t.Unix() * 1000, nil
}

// dayOf renders tsMs's local calendar date as "YYYY-MM-DD". Local, not UTC:
// the daily chart's columns are the user's own calendar days.
func dayOf(tsMs int64) string {
	return time.Unix(tsMs/1000, 0).Format("2006-01-02")
}

// noonMs is the single function that stamps both rollups at local noon on
// day. It is called from both the daily-cost and
// daily-token halves of toLines below, so the two families of rollup lines
// cannot drift onto different timestamps for the same day (see
// TestBackfillBothRollupsStampSameDayIdentically). day always came from
// dayOf above, so the parse here cannot fail in practice; a failure falls
// back to epoch 0 rather than propagating an error through every caller of
// toLines for a case that cannot occur.
func noonMs(day string) int64 {
	t, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		return 0
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 12, 0, 0, 0, time.Local).UnixMilli()
}

// --- line building -----------------------------------------------------------

// esc escapes a label value: backslash first, then quote, then newline, so
// the backslashes introduced by the quote/newline substitutions are never
// themselves re-escaped.
func esc(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

// formatCost renders a raw (unrounded) per-request cost as the shortest
// fixed-point decimal that round-trips back to the same float64: 'f' rather
// than 'g' so a very small cost never comes out in scientific notation.
func formatCost(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// toLines renders the four series maps into Prometheus text-exposition
// lines. Model, project and workspace are escaped through esc(); sidechain ("0"/"1") and token type (one of four
// fixed literals) are not, since neither is ever user-controlled text.
// profileLabel renders the optional profile label, and renders nothing at
// all when there is no profile. Emitting profile="" instead would be a
// different series identity from the one an unlabelled install already has,
// which is exactly the kind of silent double count --reset-cost-series
// exists for.
func profileLabel(profile string) string {
	if profile == "" {
		return ""
	}
	return fmt.Sprintf(`,profile="%s"`, esc(profile))
}

func toLines(cost map[costKey]float64, tokens map[tokenKey]int64, daily map[dailyKey]float64, dailyTokens map[dailyTokenKey]int64) []string {
	lines := make([]string, 0, len(cost)+len(tokens)+len(daily)+len(dailyTokens))
	for k, v := range cost {
		lines = append(lines, fmt.Sprintf(`claude_hist_cost_usd{model="%s",project="%s",workspace="%s",sidechain="%s"%s} %s %d`,
			esc(k.Model), esc(k.Project), esc(k.Workspace), k.Sidechain, profileLabel(k.Profile), formatCost(v), k.TSMs))
	}
	for k, v := range tokens {
		lines = append(lines, fmt.Sprintf(`claude_hist_tokens{model="%s",type="%s"} %d %d`,
			esc(k.Model), k.Type, v, k.TSMs))
	}
	for k, v := range daily {
		lines = append(lines, fmt.Sprintf(`claude_hist_daily_cost_usd{model="%s",project="%s",workspace="%s",sidechain="%s"%s} %.4f %d`,
			esc(k.Model), esc(k.Project), esc(k.Workspace), k.Sidechain, profileLabel(k.Profile), v, noonMs(k.Day)))
	}
	for k, v := range dailyTokens {
		lines = append(lines, fmt.Sprintf(`claude_hist_daily_tokens{model="%s",type="%s"} %d %d`,
			esc(k.Model), k.Type, v, noonMs(k.Day)))
	}
	return lines
}

// batchLines splits lines into chunks of at most size. No lines means no
// batches, so RunBackfill's post loop makes zero POST calls.
func batchLines(lines []string, size int) [][]string {
	if len(lines) == 0 {
		return nil
	}
	if size <= 0 {
		size = len(lines)
	}
	out := make([][]string, 0, (len(lines)+size-1)/size)
	for i := 0; i < len(lines); i += size {
		end := i + size
		if end > len(lines) {
			end = len(lines)
		}
		out = append(out, lines[i:end])
	}
	return out
}

// --- HTTP bits, each a small func testable behind httptest ------------------

// healthCheck is a bare preflight GET against /health: any non-2xx status
// is treated as unreachable, so a misconfigured VMURL fails before the
// import starts rather than partway through it.
func healthCheck(vmURL string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(vmURL + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// postBatch POSTs one newline-joined batch of Prometheus lines to VM's
// import endpoint.
func postBatch(vmURL string, batch []string) error {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(vmURL+"/api/v1/import/prometheus", "text/plain", strings.NewReader(strings.Join(batch, "\n")))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// deleteSeries drops every series for one metric name from VictoriaMetrics.
// It is only reachable through BackfillOpts.ResetCostSeries, and the comment
// there explains when that is the right thing to do. The delete is safe in
// the specific sense that matters here and no other: the caller recomputes
// every deleted sample from the transcripts in the same run, so the data is
// re-derivable, not recoverable from VM.
// It POSTs rather than GETs: VictoriaMetrics answers this endpoint with 405
// on GET (measured against the pinned build), unlike every other endpoint
// this file calls.
func deleteSeries(vmURL, metric string) error {
	client := &http.Client{Timeout: 60 * time.Second}
	q := url.Values{"match[]": {fmt.Sprintf(`{__name__=%q}`, metric)}}
	resp, err := client.Post(vmURL+"/api/v1/admin/tsdb/delete_series",
		"application/x-www-form-urlencoded", strings.NewReader(q.Encode()))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// resetRollupCache clears VM's rollup result cache after an import, matching
// main()'s GET against /internal/resetRollupResultCache: a panel's next
// query must not be served from a result cached before the import.
func resetRollupCache(vmURL string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(vmURL + "/internal/resetRollupResultCache")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// queryTotal runs one instant PromQL query and returns its first scalar
// result. An empty result set returns 0, not an error: "nothing imported
// yet" is a legitimate answer during reconcile's poll.
func queryTotal(vmURL, query string) (float64, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	// nocache=1 is not a micro-optimisation in reverse, it is the difference
	// between this function working and lying.
	//
	// VictoriaMetrics caches instant-query results by query string. Every
	// caller here polls ONE query string repeatedly while an import is still
	// being ingested, so the first read lands mid-ingestion and every later
	// read returns that same partial figure from the cache, unchanged, for as
	// long as you care to poll. It looks exactly like a settled total that
	// happens to be short.
	//
	// Measured 2026-09-21 on a 147 series import: the cached query held a
	// total 4.6% short indefinitely while the same query with nocache=1
	// returned the dump's own sum to the cent. The repeated
	// "settles within about twenty seconds" behaviour this file's reconcile
	// comment records is partly this, not ingestion lag.
	q := url.Values{"query": {query}, "nocache": {"1"}}
	resp, err := client.Get(vmURL + "/api/v1/query?" + q.Encode())
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	if len(body.Data.Result) == 0 {
		return 0, nil
	}
	s, ok := body.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("unexpected value type %T", body.Data.Result[0].Value[1])
	}
	return strconv.ParseFloat(s, 64)
}
