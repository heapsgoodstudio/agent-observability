package server

import (
	"context"
	"fmt"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/rules"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

/* VictoriaLogs queries --------------------------------------------------
   The log-side inputs every rule reads. Every query but turns runs at limit
   500; turns runs at 20000, since a busy week produces far more user turns
   than the other event kinds and a truncated count would understate it. */

const (
	vlBySourceQuery  = `_time:30d event.name:api_request | stats by (query_source) count() n`
	vlSubagentsQuery = `_time:30d event.name:subagent_completed | stats by (agent_type) count() runs,` +
		` sum(total_tokens) tok, sum(total_tool_uses) tools, sum(duration_ms) ms`
	vlTurnsQuery       = `_time:7d event.name:api_request | stats by (prompt.id) count() reqs`
	vlToolResultsQuery = `_time:30d event.name:tool_result | stats by (tool_name, success) count() n`
	vlWorstFailQuery   = `_time:30d event.name:tool_result success:false | stats by (session.id) count() n` +
		` | sort by (n desc) | limit 1`
	vlDecisionsQuery = `_time:30d event.name:tool_decision | stats by (source, decision, tool_name) count() n`
	vlHooksQuery     = `_time:30d event.name:hook_execution_complete | stats by (hook_name) count() runs,` +
		` sum(total_duration_ms) ms, sum(num_non_blocking_error) errs`
	vlMcpConnectedQuery = `_time:30d event.name:mcp_server_connection status:connected | stats by (server_name) count() n`
	vlMcpFailedQuery    = `_time:30d event.name:mcp_server_connection status:failed | stats by (server_name) count() n`
	vlFirstQuery        = `_time:90d | sort by (_time) | limit 1 | fields _time`
)

const (
	vlDefaultLimit = 500
	vlTurnsLimit   = 20000
)

/* VictoriaMetrics inputs: active CLI time, commits and lines added over
   seven days. Compatibility costs and exhausted-window valuations are not
   billing evidence, so findings do not query or expose them. */

// activeQuery sums 7 days of active CLI time. type="cli" is pinned: active_time
// also has a type="user" population, and an unpinned sum would mix two
// different definitions of active time.
const activeQuery = `sum(increase({__name__="claude_code.active_time.total", type="cli"}[7d]))`

// commitsQuery deliberately keeps increase(), unlike the per-session and
// per-project reads of the same counter, which go through
// history.MergedTotals and were switched to max_over_time. The two ask
// different questions. "How many commits did this session make" wants the
// counter's final value, and increase() answers it wrong. "How many commits
// in the last 7 days", which is what this rule pairs against 7 days of cost,
// wants only the growth inside the window: a session whose commits all
// landed before it should contribute nothing here, and increase() is what
// gives that. Every term of this rule's ratio shares the window, and they
// must keep sharing it.
const commitsQuery = `sum(increase({__name__="claude_code.commit.count"}[7d]))`

// linesQuery sums 7 days of lines written. type="added" is pinned for the same
// reason activeQuery pins type="cli": summing added and removed together
// would count deletions as production.
const linesQuery = `sum(increase({__name__="claude_code.lines_of_code.count", type="added"}[7d]))`

// badgeSeverities are the severities the sidebar badge counts: high, medium
// and info. clean and insufficient never contribute, since neither is a
// problem and a badge that counted them would always be lit.
var badgeSeverities = map[string]bool{"high": true, "medium": true, "info": true}

// numVL coerces a VictoriaLogs stats field (always a string over the wire)
// to a float, falling back to 0 when the result is not finite.
// strconv.ParseFloat accepts "Inf" and "NaN" spellings, so those are screened
// out explicitly rather than flowing into an arithmetic rule.
func numVL(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// strOr reads key from m, matching JS's `r.key ?? def`: a key that is
// present (even as an empty string) is returned as-is, and only a key
// entirely absent from the row falls back to def. A plain m[key] would not
// tell those two cases apart.
func strOr(m map[string]string, key, def string) string {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// parseVLTime parses a VictoriaLogs `_time` value (an RFC3339-ish string,
// with or without fractional seconds) into floor(epoch seconds). ok is false
// when s is empty or does not parse, so an unreadable timestamp is reported
// as "no logs start known" rather than silently becoming a zero the rules
// would treat as 1970.
func parseVLTime(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return 0, false
		}
	}
	return math.Floor(float64(t.UnixMilli()) / 1000), true
}

// loadVLFacts runs the ten log-side queries as a first-failure-aborts
// sequence: any one of them failing means the VL source is down, and a
// partial fact bundle would let rules draw conclusions from missing data.
func loadVLFacts(ctx context.Context, mvl interface {
	Query(context.Context, string, int) ([]map[string]string, error)
}) (rules.VLFacts, error) {
	bySourceRows, err := mvl.Query(ctx, vlBySourceQuery, vlDefaultLimit)
	if err != nil {
		return rules.VLFacts{}, err
	}
	subagentRows, err := mvl.Query(ctx, vlSubagentsQuery, vlDefaultLimit)
	if err != nil {
		return rules.VLFacts{}, err
	}
	turnRows, err := mvl.Query(ctx, vlTurnsQuery, vlTurnsLimit)
	if err != nil {
		return rules.VLFacts{}, err
	}
	toolResultRows, err := mvl.Query(ctx, vlToolResultsQuery, vlDefaultLimit)
	if err != nil {
		return rules.VLFacts{}, err
	}
	worstFailRows, err := mvl.Query(ctx, vlWorstFailQuery, vlDefaultLimit)
	if err != nil {
		return rules.VLFacts{}, err
	}
	decisionRows, err := mvl.Query(ctx, vlDecisionsQuery, vlDefaultLimit)
	if err != nil {
		return rules.VLFacts{}, err
	}
	hookRows, err := mvl.Query(ctx, vlHooksQuery, vlDefaultLimit)
	if err != nil {
		return rules.VLFacts{}, err
	}
	mcpConnectedRows, err := mvl.Query(ctx, vlMcpConnectedQuery, vlDefaultLimit)
	if err != nil {
		return rules.VLFacts{}, err
	}
	mcpFailedRows, err := mvl.Query(ctx, vlMcpFailedQuery, vlDefaultLimit)
	if err != nil {
		return rules.VLFacts{}, err
	}
	firstRows, err := mvl.Query(ctx, vlFirstQuery, vlDefaultLimit)
	if err != nil {
		return rules.VLFacts{}, err
	}

	facts := rules.VLFacts{}

	facts.BySource = make([]rules.SourceRow, len(bySourceRows))
	for i, r := range bySourceRows {
		facts.BySource[i] = rules.SourceRow{
			QuerySource: strOr(r, "query_source", ""),
			N:           numVL(r["n"]),
			Cost:        numVL(r["cost"]),
		}
	}

	facts.Subagents = make([]rules.SubagentRow, len(subagentRows))
	for i, r := range subagentRows {
		facts.Subagents[i] = rules.SubagentRow{
			AgentType: strOr(r, "agent_type", "unknown"),
			Runs:      numVL(r["runs"]),
			Tok:       numVL(r["tok"]),
			Tools:     numVL(r["tools"]),
			Ms:        numVL(r["ms"]),
		}
	}

	facts.Turns = make([]rules.TurnRow, len(turnRows))
	for i, r := range turnRows {
		facts.Turns[i] = rules.TurnRow{Cost: numVL(r["cost"]), Reqs: numVL(r["reqs"])}
	}

	facts.ToolResults = make([]rules.ToolResultRow, len(toolResultRows))
	for i, r := range toolResultRows {
		facts.ToolResults[i] = rules.ToolResultRow{
			ToolName: strOr(r, "tool_name", "unknown"),
			Success:  r["success"] == "true",
			N:        numVL(r["n"]),
		}
	}

	if len(worstFailRows) > 0 {
		r := worstFailRows[0]
		facts.WorstFailSession = rules.WorstFail{
			SessionID: strOr(r, "session.id", ""),
			N:         numVL(r["n"]),
			Found:     true,
		}
	}

	facts.Decisions = make([]rules.DecisionRow, len(decisionRows))
	for i, r := range decisionRows {
		facts.Decisions[i] = rules.DecisionRow{
			Source:   strOr(r, "source", ""),
			Decision: strOr(r, "decision", ""),
			ToolName: strOr(r, "tool_name", "unknown"),
			N:        numVL(r["n"]),
		}
	}

	facts.Hooks = make([]rules.HookRow, len(hookRows))
	for i, r := range hookRows {
		facts.Hooks[i] = rules.HookRow{
			HookName: strOr(r, "hook_name", "unknown"),
			Runs:     numVL(r["runs"]),
			Ms:       numVL(r["ms"]),
			Errs:     numVL(r["errs"]),
		}
	}

	facts.MCPConnected = make([]rules.ServerRow, len(mcpConnectedRows))
	for i, r := range mcpConnectedRows {
		facts.MCPConnected[i] = rules.ServerRow{ServerName: strOr(r, "server_name", ""), N: numVL(r["n"])}
	}

	facts.MCPFailed = make([]rules.ServerRow, len(mcpFailedRows))
	for i, r := range mcpFailedRows {
		facts.MCPFailed[i] = rules.ServerRow{ServerName: strOr(r, "server_name", ""), N: numVL(r["n"])}
	}

	if len(firstRows) > 0 {
		if ts, ok := parseVLTime(firstRows[0]["_time"]); ok {
			facts.LogsStart = ts
			facts.HasLogsStart = true
		}
	}

	return facts, nil
}

// loadVMFacts reads the three activity metrics, all-or-nothing like logs.
func loadVMFacts(ctx context.Context, mvm interface {
	Q(context.Context, string) ([]vm.Series, error)
}) (rules.VMFacts, error) {
	activeRes, err := mvm.Q(ctx, activeQuery)
	if err != nil {
		return rules.VMFacts{}, err
	}
	commitsRes, err := mvm.Q(ctx, commitsQuery)
	if err != nil {
		return rules.VMFacts{}, err
	}
	linesRes, err := mvm.Q(ctx, linesQuery)
	if err != nil {
		return rules.VMFacts{}, err
	}

	active, _ := vm.Scalar(activeRes)
	commits, _ := vm.Scalar(commitsRes)
	lines, _ := vm.Scalar(linesRes)

	return rules.VMFacts{
		ActiveHours7d: active / 3600,
		Commits7d:     commits,
		Lines7d:       lines,
		Reachable:     true,
	}, nil
}

// findingsToProto converts rules.Run's output to the wire shape, in the same
// order Run returned it (every severity, including clean and insufficient:
// the response carries the whole list, and a consumer partitions it into the
// summary lines itself).
func findingsToProto(fs []rules.Finding) []*claudeobsv1.Finding {
	out := make([]*claudeobsv1.Finding, len(fs))
	for i, f := range fs {
		out[i] = &claudeobsv1.Finding{
			Id:       f.ID,
			Severity: f.Severity,
			Title:    f.Title,
			Evidence: f.Evidence,
			Fix:      f.Fix,
			Window:   f.Window,
			Impact:   f.Impact,
		}
	}
	return out
}

// badgeCount is the sidebar badge: only high/medium/info findings count.
func badgeCount(fs []rules.Finding) int32 {
	var n int32
	for _, f := range fs {
		if badgeSeverities[f.Severity] {
			n++
		}
	}
	return n
}

// buildFindings is GetFindings' real body. The three sources (VL, VM, the
// session index) load independently and each degrades to its zero shape on
// failure; rules.Run still runs on whatever loaded. Only when all three are
// down is there nothing left to compute at all.
func buildFindings(ctx context.Context, o Options, scopes ...dashboardScope) (*claudeobsv1.GetFindingsResponse, error) {
	scope := dashboardScope{All: true}
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	note := findingsScopeNote(scope)
	if scope.Provider != "" && scope.Provider != "claude" {
		if o.DB == nil {
			return nil, errNoDB
		}
		rows, err := db.ListAdviceSessions(o.DB, scope.Provider, findingsProfiles(scope))
		if err != nil {
			return nil, err
		}
		out := indexedFindings(db.FilterAdviceSessions(rows, scope.AccountKey))
		out.Provider, out.AccountKey, out.Profile, out.ScopeNote = scope.Provider, scope.AccountKey, scope.Profile, note
		return out, nil
	}
	mvm := findingsVM{memoVM: memoVM{client: o.VM, memo: o.Memo}, scope: scope}
	mvl := findingsVL{memoVL: memoVL{client: o.VL, memo: o.Memo}, scope: scope}

	vl, vlErr := loadVLFacts(ctx, mvl)
	vmFacts, vmErr := loadVMFacts(ctx, mvm)
	var idxRows []db.SessionRow
	var idxErr error
	if o.DB == nil {
		idxErr = errNoDB
	} else if _, ok, err := db.MetaGet(o.DB, sessionsGeneratedAtKey); err != nil {
		idxErr = err
	} else if !ok {
		idxErr = errNeverIndexed
	} else {
		idxRows, idxErr = db.ListAdviceSessions(o.DB, "claude", findingsProfiles(scope))
	}

	vlOk := vlErr == nil
	vmOk := vmErr == nil
	idxOk := idxErr == nil

	if !vlOk && !vmOk && !idxOk {
		return nil, fmt.Errorf("findings: no store reachable: nothing to analyse")
	}

	var errs []*claudeobsv1.SectionError
	if !vlOk {
		vl = rules.VLFacts{}
		errs = append(errs, &claudeobsv1.SectionError{Section: "vl", Error: vlErr.Error()})
	}
	if !vmOk {
		vmFacts = rules.VMFacts{}
		errs = append(errs, &claudeobsv1.SectionError{Section: "vm", Error: vmErr.Error()})
	}
	var indexFacts rules.IndexFacts
	if idxOk {
		indexFacts = sessionFactsFromSessions(idxRows)
	} else {
		errs = append(errs, &claudeobsv1.SectionError{Section: "index", Error: idxErr.Error()})
	}

	findings := rules.Run(rules.Data{VM: vmFacts, VL: vl, Index: indexFacts})

	resp := &claudeobsv1.GetFindingsResponse{
		Identity: identity(), Provider: scope.Provider, AccountKey: scope.AccountKey, Profile: scope.Profile, ScopeNote: note,
		ActiveHours_7D: vmFacts.ActiveHours7d,
		Commits_7D:     vmFacts.Commits7d,
		Lines_7D:       vmFacts.Lines7d,
		Findings:       findingsToProto(findings),
		VmOk:           vmOk,
		VlOk:           vlOk,
		IndexOk:        idxOk,
		BadgeCount:     badgeCount(findings),
		Errors:         errs,
	}
	if scope.All && o.DB != nil {
		for _, provider := range []string{"codex", "omp", "opencode"} {
			if rows, err := db.ListAdviceSessions(o.DB, provider, nil); err == nil && len(rows) > 0 {
				extra := indexedFindings(rows)
				for _, f := range extra.Findings {
					f.Id = provider + "-" + f.Id
					f.Title = profiles.ProviderName(profiles.Provider(provider)) + ": " + f.Title
				}
				resp.Findings = append(resp.Findings, extra.Findings...)
				resp.BadgeCount += extra.BadgeCount
			}
		}
	}
	if vl.HasLogsStart {
		resp.LogsStartTs = vl.LogsStart
	}
	return resp, nil
}

func findingsScopeNote(s dashboardScope) string {
	note := "Findings and advice use evidence within the selected scope. Missing observations remain unknown."
	if profiles.Detected(profiles.Provider(s.Provider)) && s.AccountKey != "" {
		note += " Sessions are selected by the inference provider they used; mixed-provider sessions retain their whole-session totals."
	}
	if s.ScopeNote != "" {
		note += " " + s.ScopeNote
	}
	return note
}

func findingsProfiles(s dashboardScope) []string {
	if s.AccountKey == "" && s.Profile == "" {
		return nil
	}
	if len(s.Profiles) == 0 {
		return []string{}
	}
	return s.Profiles
}

// Selectors become part of the memo cache key as well as the store request.
// Empty legacy provider labels are Claude; other providers never participate.
func findingsVMQuery(q string, s dashboardScope) string {
	labels := `provider=~"claude|"`
	if ps := findingsProfiles(s); ps != nil {
		parts := make([]string, len(ps))
		for i, p := range ps {
			parts[i] = regexp.QuoteMeta(p)
		}
		pattern := strings.Join(parts, "|")
		if len(parts) == 0 {
			pattern = "a^"
		}
		labels += ",profile=~" + strconv.Quote(pattern)
	}
	return strings.ReplaceAll(q, "}", ","+labels+"}")
}
func findingsVLQuery(q string, s dashboardScope) string {
	filter := `(provider:="claude" OR provider:="")`
	if ps := findingsProfiles(s); ps != nil {
		parts := make([]string, len(ps))
		for i, p := range ps {
			parts[i] = "profile:=" + strconv.Quote(p)
		}
		if len(parts) == 0 {
			filter += ` AND (profile:="" AND NOT profile:="")`
		} else {
			filter += " AND (" + strings.Join(parts, " OR ") + ")"
		}
	}
	return filter + " " + q
}

type findingsVM struct {
	memoVM
	scope dashboardScope
}

func (m findingsVM) Q(ctx context.Context, q string) ([]vm.Series, error) {
	if m.client == nil {
		return nil, fmt.Errorf("metrics store unavailable")
	}
	query := findingsVMQuery(q, m.scope)
	if m.memo == nil {
		return m.client.Q(ctx, query)
	}
	return m.memoVM.Q(ctx, query)
}

type findingsVL struct {
	memoVL
	scope dashboardScope
}

func (m findingsVL) Query(ctx context.Context, q string, limit int) ([]map[string]string, error) {
	if m.client == nil {
		return nil, fmt.Errorf("logs store unavailable")
	}
	query := findingsVLQuery(q, m.scope)
	if m.memo == nil {
		return m.client.Query(ctx, query, limit)
	}
	return m.memoVL.Query(ctx, query, limit)
}
