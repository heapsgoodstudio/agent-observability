// Package rules holds the rules behind the Findings page. Every rule is a pure function from an already-fetched data
// bundle (Data) to one Finding, so the arithmetic behind a piece of advice is
// testable without a browser or a store.
//
// Severity: high and medium are worth acting on, info is context, clean means
// the rule ran and found nothing, insufficient means there is not enough
// history to judge. A rule that cannot tell the difference between clean and
// insufficient must return insufficient: claiming health on thin data is
// worse than saying nothing.
package rules

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/heapsgoodstudio/agent-observability/internal/pricing"
)

// severityOrder is the sort key Run uses ahead of impact and id.
var severityOrder = map[string]int{"high": 0, "medium": 1, "info": 2, "clean": 3, "insufficient": 4}

// pct formats v (a fraction) as a whole-number percentage.
func pct(v float64) string {
	return fmt.Sprintf("%d%%", int64(math.Round(v*100)))
}

// intFmt formats v with comma-grouped thousands and no decimals.
func intFmt(v float64) string {
	n := int64(math.Round(v))
	neg := n < 0
	if neg {
		n = -n
	}
	digits := fmt.Sprintf("%d", n)
	var grouped strings.Builder
	length := len(digits)
	for i, r := range digits {
		if i > 0 && (length-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(r)
	}
	if neg {
		return "-" + grouped.String()
	}
	return grouped.String()
}

// finding builds a full Finding.
func finding(id, severity, title, evidence, fix, window string, impact float64) Finding {
	return Finding{ID: id, Severity: severity, Title: title, Evidence: evidence, Fix: fix, Window: window, Impact: impact}
}

// quiet builds a clean or insufficient Finding: id, severity and title, plus
// an empty window and zero impact so every finding shares one shape.
func quiet(id, severity, title string) Finding {
	return Finding{ID: id, Severity: severity, Title: title, Window: "", Impact: 0}
}

/* Rules --------------------------------------------------------------------
   Ten pure functions, one per finding. Each carries its own thresholds and
   sentence fragments; the reasoning behind a threshold is in the comment
   above the rule that uses it. */

// RealMoney retains its stable finding ID, but the input contains no
// provider-reported bill. A sampled exhausted window and an API token
// valuation cannot establish which requests were outside the allowance.
func RealMoney(_ Data) Finding {
	f := quiet("real-money", "insufficient", "actual overage cannot be determined from token activity or plan-window percentages")
	f.Evidence = "These activity records do not identify actual billed charges. Unknown billing must not be treated as zero."
	f.Fix = "Check the provider-reported billing display for actual overage, and monitor allowance and reset times before starting more work."
	return f
}

// CacheDiscipline reports the ratio of cache reads to cache writes: writing
// far more than is read back means the prompt prefix keeps changing.
func CacheDiscipline(d Data) Finding {
	var read, write float64
	for _, s := range d.Index.Sessions {
		read += s.CacheRead
		write += s.CacheCreation
	}
	if read+write < 1e6 {
		return quiet("cache-discipline", "insufficient", "not enough cached context to judge")
	}
	ratio := write / math.Max(read, 1)
	if ratio <= 0.15 {
		return quiet("cache-discipline", "clean", fmt.Sprintf("cache write to read ratio is %.3f", ratio))
	}
	sev := "medium"
	if ratio > 0.3 {
		sev = "high"
	}
	return finding(
		"cache-discipline", sev,
		fmt.Sprintf("cache write to read ratio is %.2f", ratio),
		fmt.Sprintf("%s cache tokens written against %s read.", intFmt(write), intFmt(read)),
		"Review repeated context setup and unnecessary session restarts. Cache availability also depends on the provider's retention policy.",
		"all history", 0,
	)
}

// modelShort strips a leading "claude-" prefix. Deliberately not
// pricing.ShortModel: that also strips a trailing date and a [1m]-style
// context suffix, which would change the title for a model name carrying
// either.
func modelShort(m string) string {
	return strings.TrimPrefix(m, "claude-")
}

// modelMixRow counts sessions mentioning a model. The input's monetary map
// values do not measure requests or tokens and are deliberately ignored.
type modelMixRow struct {
	model    string
	sessions float64
}

// ModelMix reports model presence among sessions with model metadata. Sessions
// using multiple models count toward each model, so percentages may overlap.
func ModelMix(d Data) Finding {
	byModel := map[string]float64{}
	var total float64
	for _, s := range d.Index.Sessions {
		hasModel := false
		for m := range s.Models {
			if strings.TrimSpace(m) != "" {
				byModel[m]++
				hasModel = true
			}
		}
		if hasModel {
			total++
		}
	}
	if !(total > 0) {
		return quiet("model-mix", "insufficient", "no sessions with model metadata on record")
	}
	var rows []modelMixRow
	for model, count := range byModel {
		rows = append(rows, modelMixRow{model: model, sessions: count})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].sessions == rows[j].sessions {
			return rows[i].model < rows[j].model
		}
		return rows[i].sessions > rows[j].sessions
	})
	n := len(rows)
	if n > 4 {
		n = 4
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = fmt.Sprintf("%s in %s sessions (%s)", modelShort(rows[i].model), intFmt(rows[i].sessions), pct(rows[i].sessions/total))
	}
	return finding(
		"model-mix", "info",
		fmt.Sprintf("%s appears in %s of %s sessions with model data", modelShort(rows[0].model), pct(rows[0].sessions/total), intFmt(total)),
		strings.Join(parts, ", ")+". Sessions can include multiple models; percentages may overlap.",
		"Use this activity breakdown to review model choices against the work performed. Session presence does not measure token volume or billing.",
		"all history", 0,
	)
}

// mainSources are query_source values that are main-thread cost, not
// subagent fan-out: interactive main thread (repl_main*), headless -p runs
// (sdk), and the title-generation side request.
var mainSources = map[string]bool{"main": true, "sdk": true, "generate_session_title": true}

// IsMainThreadSource classifies one query_source. internal/server/sessions.go's
// loadLiveSession calls it to split a live session's cost into main thread vs
// sidechain, so the classifier lives here rather than being duplicated there.
func IsMainThreadSource(source string) bool {
	return mainSources[source] || strings.HasPrefix(source, "repl_main")
}

// IsSubagentSource reports whether a query_source is subagent fan-out.
func IsSubagentSource(source string) bool {
	return strings.HasPrefix(source, "agent:")
}

// MainThreadConcentration measures request activity by query source. Cost
// values cannot identify actual charges and are not used by this rule.
func MainThreadConcentration(d Data) Finding {
	var mainN, agentN, otherN float64
	var otherSources []string
	seenOther := map[string]bool{}
	for _, r := range d.VL.BySource {
		if !(r.N > 0) {
			continue
		}
		switch {
		case IsMainThreadSource(r.QuerySource):
			mainN += r.N
		case IsSubagentSource(r.QuerySource):
			agentN += r.N
		default:
			// Other sources remain in the request denominator.
			otherN += r.N
			name := r.QuerySource
			if name == "" {
				name = "unknown"
			}
			if !seenOther[name] {
				seenOther[name] = true
				otherSources = append(otherSources, name)
			}
		}
	}
	if mainN < 50 || agentN < 50 {
		return quiet("main-thread-concentration", "insufficient",
			"not enough requests on both the main thread and subagents yet")
	}
	totalN := mainN + agentN + otherN
	share := mainN / totalN
	if share <= 0.7 {
		return quiet("main-thread-concentration", "clean",
			fmt.Sprintf("the main thread accounts for %s of requests", pct(share)))
	}
	// Worth naming only when it moves the arithmetic: a stray source with a
	// rounding error of a share is noise in the evidence.
	other := ""
	if otherN/totalN >= 0.01 {
		sorted := append([]string(nil), otherSources...)
		sort.Strings(sorted)
		other = fmt.Sprintf(" %s requests came from other sources: %s.", intFmt(otherN), strings.Join(sorted, ", "))
	}
	return finding(
		"main-thread-concentration", "medium",
		fmt.Sprintf("the main thread accounts for %s of requests", pct(share)),
		fmt.Sprintf("%s main-thread requests and %s subagent requests out of %s total.%s",
			intFmt(mainN), intFmt(agentN), intFmt(totalN), other),
		"Review whether independent, well-specified tasks could be delegated. Request counts alone do not measure token use, quality, or billing.",
		"30d", 0,
	)
}

// subagentRow is one subagent type's per-run token cost, computed once and
// carried alongside the raw row so the evidence sentence can still name runs.
type subagentRow struct {
	SubagentRow
	perRun float64
}

// SubagentRoi reports token spend per subagent run, per agent type, and
// flags a spread wide enough to suggest one type is being misused.
func SubagentRoi(d Data) Finding {
	var rows []SubagentRow
	for _, r := range d.VL.Subagents {
		if r.Runs >= 5 {
			rows = append(rows, r)
		}
	}
	if len(rows) < 2 {
		return quiet("subagent-roi", "insufficient", "fewer than two agent types with five runs each")
	}
	// Tokens per run is the whole measurement. If the field stopped being
	// emitted every type scores zero and the spread reads 0.0x, which is not
	// a finding.
	var tokSum float64
	for _, r := range rows {
		tokSum += r.Tok
	}
	if !(tokSum > 0) {
		return quiet("subagent-roi", "insufficient", "no token counts recorded against agent runs")
	}
	per := make([]subagentRow, len(rows))
	for i, r := range rows {
		per[i] = subagentRow{SubagentRow: r, perRun: r.Tok / r.Runs}
	}
	sort.SliceStable(per, func(i, j int) bool { return per[i].perRun > per[j].perRun })
	top := per[0]
	bottom := per[len(per)-1]
	spread := top.perRun / math.Max(bottom.perRun, 1)
	sev := "info"
	if spread > 3 {
		sev = "medium"
	}
	parts := make([]string, len(per))
	for i, r := range per {
		// r.Runs is interpolated raw: unlike the other numeric fields in
		// this sentence it is deliberately not comma-grouped, since a run
		// count is small enough to read without separators.
		parts[i] = fmt.Sprintf("%s %s tokens per run over %d runs", r.AgentType, intFmt(r.perRun), int64(r.Runs))
	}
	return finding(
		"subagent-roi", sev,
		fmt.Sprintf("%s uses %.1fx the tokens per run of %s", top.AgentType, spread, bottom.AgentType),
		strings.Join(parts, ", ")+".",
		"Compare the tasks assigned to each agent type before interpreting the difference. Task complexity and context size also affect tokens per run.",
		"30d", 0,
	)
}

// TurnCost keeps its stable ID but measures requests per turn. Monetary
// weights do not establish actual charges and must not drive this finding.
func TurnCost(d Data) Finding {
	var requests []float64
	for _, t := range d.VL.Turns {
		if t.Reqs > 0 {
			requests = append(requests, t.Reqs)
		}
	}
	sort.Float64s(requests)
	if len(requests) < 10 {
		return quiet("turn-cost", "insufficient", "fewer than ten turns with request counts on record")
	}
	at := func(frac float64) float64 {
		idx := int(math.Floor(frac * float64(len(requests))))
		if idx > len(requests)-1 {
			idx = len(requests) - 1
		}
		return requests[idx]
	}
	p50 := at(0.5)
	p95 := at(0.95)
	maxRequests := requests[len(requests)-1]
	if !(maxRequests > 20*p50 || p95 > 10*p50) {
		return quiet("turn-cost", "clean", fmt.Sprintf("turns use %s requests at the median", intFmt(p50)))
	}
	sev := "medium"
	if maxRequests > 20*p50 {
		sev = "high"
	}
	return finding(
		"turn-cost", sev,
		fmt.Sprintf("the busiest turn used %s requests", intFmt(maxRequests)),
		fmt.Sprintf("Median %s requests per turn, p95 %s across %s turns with request counts.", intFmt(p50), intFmt(p95), intFmt(float64(len(requests)))),
		"Review the busiest turns for retries or repeated work. Split work at a natural checkpoint when it helps keep the task focused.",
		"7d", 0,
	)
}

// toolTally is a tool name paired with an accumulated count, in
// first-appearance order so ties sort the same way JS's stable Array.sort
// over a Map's insertion-ordered entries would.
type toolTally struct {
	tool string
	n    float64
}

func tallyByTool(names []string, counts map[string]float64) []toolTally {
	out := make([]toolTally, len(names))
	for i, name := range names {
		out[i] = toolTally{tool: name, n: counts[name]}
	}
	return out
}

// FailureLoops reports repeated failing tool calls, where the same command
// is retried without the error being addressed.
func FailureLoops(d Data) Finding {
	var total, failed float64
	counts := map[string]float64{}
	var order []string
	for _, r := range d.VL.ToolResults {
		total += r.N
		if r.Success {
			continue
		}
		failed += r.N
		if _, ok := counts[r.ToolName]; !ok {
			order = append(order, r.ToolName)
		}
		counts[r.ToolName] += r.N
	}
	if total < 200 {
		return quiet("failure-loops", "insufficient", "fewer than two hundred tool calls on record")
	}
	rate := failed / total
	if rate <= 0.02 {
		return quiet("failure-loops", "clean", fmt.Sprintf("%s of tool calls failed", pct(rate)))
	}
	worst := tallyByTool(order, counts)
	sort.SliceStable(worst, func(i, j int) bool { return worst[i].n > worst[j].n })
	where := ""
	if d.VL.WorstFailSession.Found {
		sid := d.VL.WorstFailSession.SessionID
		if len(sid) > 8 {
			sid = sid[:8]
		}
		where = fmt.Sprintf(" Worst session: %s with %s.", sid, intFmt(d.VL.WorstFailSession.N))
	}
	sev := "medium"
	if rate > 0.05 {
		sev = "high"
	}
	top := worst
	if len(top) > 3 {
		top = top[:3]
	}
	parts := make([]string, len(top))
	for i, w := range top {
		parts[i] = fmt.Sprintf("%s %s", w.tool, intFmt(w.n))
	}
	return finding(
		"failure-loops", sev,
		fmt.Sprintf("%s of tool calls failed", pct(rate)),
		fmt.Sprintf("%s of %s calls. %s.%s", intFmt(failed), intFmt(total), strings.Join(parts, ", "), where),
		"A failed call still buys a full request. Repeated failures on one tool usually mean a wrong assumption that belongs in CLAUDE.md rather than in a retry.",
		"30d", 0,
	)
}

// PermissionFriction reports how much time and cost goes into permission
// prompts that a settings.json rule could have answered.
func PermissionFriction(d Data) Finding {
	var total, manual, rejected float64
	counts := map[string]float64{}
	var order []string
	for _, r := range d.VL.Decisions {
		total += r.N
		// Config rejections are a deny rule doing its job, silently.
		// Counting them as friction would put a number in the title that
		// the evidence below (all non-config) and the advice (write the
		// rule you already wrote) contradict.
		if r.Source == "config" {
			continue
		}
		manual += r.N
		if r.Decision == "reject" {
			rejected += r.N
		}
		if _, ok := counts[r.ToolName]; !ok {
			order = append(order, r.ToolName)
		}
		counts[r.ToolName] += r.N
	}
	if total < 100 {
		return quiet("permission-friction", "insufficient", "fewer than a hundred tool decisions on record")
	}
	manualShare := manual / total
	rejectShare := rejected / total
	if rejectShare <= 0.05 && manualShare <= 0.2 && manual <= 50 {
		return quiet("permission-friction", "clean",
			fmt.Sprintf("%s of tool decisions came from config", pct(1-manualShare)))
	}
	all := tallyByTool(order, counts)
	sort.SliceStable(all, func(i, j int) bool { return all[i].n > all[j].n })
	top := all
	if len(top) > 4 {
		top = top[:4]
	}
	high := rejectShare > 0.05
	sev := "medium"
	if high {
		sev = "high"
	}
	title := fmt.Sprintf("%s of tool decisions needed a human", pct(manualShare))
	if high {
		title = fmt.Sprintf("%s of tool decisions were rejections", pct(rejectShare))
	}
	evidence := fmt.Sprintf("%s manual decisions of %s.", intFmt(manual), intFmt(total))
	if len(top) > 0 {
		parts := make([]string, len(top))
		for i, w := range top {
			parts[i] = fmt.Sprintf("%s %s", w.tool, intFmt(w.n))
		}
		evidence += fmt.Sprintf(" Most often: %s.", strings.Join(parts, ", "))
	}
	fix := "Approving the same tools by hand is pure latency. Move the settled ones into the permissions allowlist in settings.json."
	if high {
		fix = "Repeatedly rejecting the same tool means the model keeps proposing something you do not want. That belongs in CLAUDE.md, where it costs one line instead of a prompt every time."
	}
	return finding("permission-friction", sev, title, evidence, fix, "30d", 0)
}

// HookTax reports the cost hooks add to every session that runs them.
func HookTax(d Data) Finding {
	var rows []HookRow
	for _, h := range d.VL.Hooks {
		if h.Runs >= 20 {
			rows = append(rows, h)
		}
	}
	if len(rows) == 0 {
		return quiet("hook-tax", "insufficient", "no hook has run twenty times yet")
	}
	// Only errors from hooks that cleared the sample floor count: a hook
	// with a single run and an error must not drag down the verdict for one
	// that ran hundreds of times cleanly.
	var errs float64
	for _, h := range rows {
		errs += h.Errs
	}
	var rowsMs float64
	for _, h := range rows {
		rowsMs += h.Ms
	}
	// With no durations recorded the slowest hook averages 0ms, which would
	// be health reported from an absent field. Errors are counted in a
	// different field though, and an error is judgeable on its own, so only
	// the timing claim is withheld here rather than the whole rule.
	if !(rowsMs > 0) {
		if errs == 0 {
			return quiet("hook-tax", "insufficient", "no hook durations recorded, so hook latency cannot be judged")
		}
		var noisy []HookRow
		for _, h := range rows {
			if h.Errs > 0 {
				noisy = append(noisy, h)
			}
		}
		sort.SliceStable(noisy, func(i, j int) bool { return noisy[i].Errs > noisy[j].Errs })
		errWord := "s"
		if errs == 1 {
			errWord = ""
		}
		hookWord := "s"
		if len(noisy) == 1 {
			hookWord = ""
		}
		parts := make([]string, len(noisy))
		for i, h := range noisy {
			parts[i] = fmt.Sprintf("%s %s of %s runs", h.HookName, intFmt(h.Errs), intFmt(h.Runs))
		}
		return finding(
			"hook-tax", "high",
			fmt.Sprintf("%s hook error%s on %d hook%s", intFmt(errs), errWord, len(noisy), hookWord),
			strings.Join(parts, ", ")+". No durations were recorded, so what these hooks cost in time is not judged here.",
			"A non-blocking hook error is silent: the hook still runs on the turn that triggered it and nothing in the session says the step failed. Fix it or take the hook out.",
			"30d", 0,
		)
	}
	worst := rows[0]
	worstMean := worst.Ms / worst.Runs
	for _, h := range rows[1:] {
		mean := h.Ms / h.Runs
		if mean > worstMean {
			worst = h
			worstMean = mean
		}
	}
	if worstMean <= 250 && errs == 0 {
		return quiet("hook-tax", "clean", fmt.Sprintf("slowest hook averages %dms", int64(math.Round(worstMean))))
	}
	var totalMs float64
	for _, h := range d.VL.Hooks {
		totalMs += h.Ms
	}
	sev := "medium"
	if worstMean > 1000 || errs > 0 {
		sev = "high"
	}
	errsSuffix := ""
	if errs != 0 {
		errsSuffix = fmt.Sprintf(", %s errors", intFmt(errs))
	}
	return finding(
		"hook-tax", sev,
		fmt.Sprintf("%s averages %dms per run", worst.HookName, int64(math.Round(worstMean))),
		fmt.Sprintf("%s runs, %.1fs spent in that hook alone, %.1fs across all hooks%s.", intFmt(worst.Runs), worst.Ms/1000, totalMs/1000, errsSuffix),
		"Hooks run inline on the turn that triggers them, so their latency lands on every request behind them. Anything that can run in the background should.",
		"30d", 0,
	)
}

// orderedUniqueNorm normalizes each row's server name with pricing.NormServer
// and returns the distinct results in first-appearance order, matching JS's
// [...new Set(rows.map(normServer))].
func orderedUniqueNorm(rows []ServerRow) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		k := pricing.NormServer(r.ServerName)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// McpFreight reports MCP server token freight. Server identity goes through
// pricing.NormServer, since VictoriaLogs and the session transcripts spell the
// same server two different ways ("plugin:linear:linear" vs
// "plugin_linear_linear").
func McpFreight(d Data) Finding {
	connected := orderedUniqueNorm(d.VL.MCPConnected)
	if len(connected) == 0 {
		return quiet("mcp-freight", "insufficient", "no mcp server connections on record")
	}
	failed := orderedUniqueNorm(d.VL.MCPFailed)
	// Which servers were called comes from the session index alone. When
	// the index fails to load its sessions are empty, and every connected
	// server would look never called: an outage read as an accusation. A
	// failed connection is observed in the log store and owes the index
	// nothing, so it is only the never-called half that goes quiet.
	noIndex := len(d.Index.Sessions) == 0
	if noIndex && len(failed) == 0 {
		return quiet("mcp-freight", "insufficient",
			"the session index is unavailable, so servers cannot be matched to calls")
	}
	used := map[string]bool{}
	for _, s := range d.Index.Sessions {
		for _, m := range s.MCPServers {
			used[pricing.NormServer(m)] = true
		}
	}
	var unused []string
	if !noIndex {
		for _, c := range connected {
			if !used[c] {
				unused = append(unused, c)
			}
		}
	}
	if len(unused) == 0 && len(failed) == 0 {
		return quiet("mcp-freight", "clean", fmt.Sprintf("all %d connected mcp servers get used", len(connected)))
	}
	var bits []string
	if len(unused) > 0 {
		bits = append(bits, fmt.Sprintf("never called: %s", strings.Join(unused, ", ")))
	}
	if len(failed) > 0 {
		bits = append(bits, fmt.Sprintf("failed to connect: %s", strings.Join(failed, ", ")))
	}
	if noIndex {
		bits = append(bits, "the session index was unavailable, so servers that connect but are never called could not be identified")
	}
	sev := "medium"
	if len(failed) > 0 {
		sev = "high"
	}
	var title string
	if len(failed) > 0 {
		s := "s"
		if len(failed) == 1 {
			s = ""
		}
		title = fmt.Sprintf("%d mcp server%s failed to connect", len(failed), s)
	} else {
		title = fmt.Sprintf("%d of %d mcp servers are never called", len(unused), len(connected))
	}
	return finding(
		"mcp-freight", sev, title,
		strings.Join(bits, ". ")+".",
		"Every connected server's tool definitions ride in the system prompt of every request, whether or not you call them. Disconnect what you do not use.",
		"30d", 0,
	)
}

/* Registry ------------------------------------------------------------------
   The rules, in the order Run applies them. Each entry's name is the
   identifier a panicking rule's fallback finding carries as
   "rule-error:<name>"; rules_test.go asserts on those strings directly, so
   renaming one is a test-visible change. */

type ruleEntry struct {
	name string
	fn   func(Data) Finding
}

var registry = []ruleEntry{
	{"realMoney", RealMoney},
	{"mainThreadConcentration", MainThreadConcentration},
	{"subagentRoi", SubagentRoi},
	{"turnCost", TurnCost},
	{"failureLoops", FailureLoops},
	{"permissionFriction", PermissionFriction},
	{"hookTax", HookTax},
	{"mcpFreight", McpFreight},
	{"cacheDiscipline", CacheDiscipline},
	{"modelMix", ModelMix},
}

// runOne calls fn(d), recovering a panic into an insufficient finding whose
// id is prefixed "rule-error:" so it can never be mistaken for a real
// kebab-case rule id. One panicking rule can never take down the page.
func runOne(name string, fn func(Data) Finding, d Data) (f Finding) {
	defer func() {
		if err := recover(); err != nil {
			f = quiet(fmt.Sprintf("rule-error:%s", name), "insufficient", fmt.Sprintf("rule failed: %v", err))
		}
	}()
	return fn(d)
}

// Run applies all ten rules to d and returns their findings sorted by
// severity (high, medium, info, clean, insufficient), then impact
// descending, then id ascending.
func Run(d Data) []Finding {
	out := make([]Finding, 0, len(registry))
	for _, r := range registry {
		out = append(out, runOne(r.name, r.fn, d))
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := severityOrder[out[i].Severity], severityOrder[out[j].Severity]
		if si != sj {
			return si < sj
		}
		if out[i].Impact != out[j].Impact {
			return out[i].Impact > out[j].Impact
		}
		return out[i].ID < out[j].ID
	})
	return out
}
