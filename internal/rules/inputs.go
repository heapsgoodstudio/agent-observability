package rules

// The input shapes Run reads: every field here is something a rule looks at,
// nothing more. Building these values (the VictoriaMetrics and VictoriaLogs
// queries, and the numeric coercions over their responses) is
// internal/server's job; keeping the shapes separate is what lets every rule
// be tested without a store.

// VMFacts mirrors findings.js's loadVm() return value: the last-7-days
// figures read from VictoriaMetrics.
type VMFacts struct {
	Cost7d        float64
	ActiveHours7d float64
	Commits7d     float64
	Lines7d       float64
	// Deprecated analytical estimates retained for input compatibility. Neither
	// an API valuation nor an exhausted-window sample establishes billing.
	// Rules must not use these fields to assert actual charges or covered use.
	Overage7d    float64
	OverageKnown bool
	Reachable    bool
}

// SourceRow mirrors one row of loadVl's bySource: cost and request count
// grouped by query_source over the last 30 days.
type SourceRow struct {
	QuerySource string
	N           float64
	Cost        float64
}

// SubagentRow mirrors one row of loadVl's subagents: token/tool/duration
// totals grouped by agent_type over the last 30 days.
type SubagentRow struct {
	AgentType string
	Runs      float64
	Tok       float64
	Tools     float64
	Ms        float64
}

// TurnRow mirrors one row of loadVl's turns: one prompt turn's cost and
// request count over the last 7 days.
type TurnRow struct {
	Cost float64
	Reqs float64
}

// ToolResultRow mirrors one row of loadVl's toolResults: a tool_name/success
// pair's call count over the last 30 days.
type ToolResultRow struct {
	ToolName string
	Success  bool
	N        float64
}

// WorstFail mirrors loadVl's worstFailSession, which is null in JS when no
// failed tool call exists in the window. Found is Go's stand-in for that
// null check; SessionID and N are meaningless while it is false.
type WorstFail struct {
	SessionID string
	N         float64
	Found     bool
}

// DecisionRow mirrors one row of loadVl's decisions: a permission decision's
// count grouped by source, decision and tool_name over the last 30 days.
type DecisionRow struct {
	Source   string
	Decision string
	ToolName string
	N        float64
}

// HookRow mirrors one row of loadVl's hooks: a hook's run count, total
// duration and error count over the last 30 days.
type HookRow struct {
	HookName string
	Runs     float64
	Ms       float64
	Errs     float64
}

// ServerRow mirrors one row of loadVl's mcpConnected/mcpFailed: an MCP
// server's connection-event count over the last 30 days.
type ServerRow struct {
	ServerName string
	N          float64
}

// VLFacts mirrors findings.js's loadVl() return value: everything the
// log-based rules read from VictoriaLogs.
type VLFacts struct {
	// LogsStart is the Unix timestamp (seconds) of the earliest event the
	// log store holds; HasLogsStart is false when the store is empty. No
	// rule in this package reads it today (findings.js uses it only for the
	// page's own coverage caption), but it rides along with the rest of
	// loadVl's shape so a future rule can use it without widening Data.
	LogsStart        float64
	HasLogsStart     bool
	BySource         []SourceRow
	Subagents        []SubagentRow
	Turns            []TurnRow
	ToolResults      []ToolResultRow
	WorstFailSession WorstFail
	Decisions        []DecisionRow
	Hooks            []HookRow
	MCPConnected     []ServerRow
	MCPFailed        []ServerRow
}

// SessionFacts mirrors the fields the rules read from one row of
// data/sessions/index.json's sessions array.
type SessionFacts struct {
	CacheRead     float64
	CacheCreation float64
	// Models retains legacy analytical weights by model name. ModelMix uses
	// only key presence per session: values are not request/token counts or
	// evidence of actual billing.
	Models map[string]float64
	// MCPServers is the raw list of MCP server names this session called,
	// in whatever spelling the transcript used. Rules that key by server
	// identity normalize with pricing.NormServer.
	MCPServers []string
}

// IndexFacts mirrors findings.js's loadIndex() return value.
type IndexFacts struct {
	Sessions []SessionFacts
}

// Data bundles the three sources every rule reads from, matching
// findings.js's loadAll()'s { vl, vm, index } argument to runRules().
type Data struct {
	VM    VMFacts
	VL    VLFacts
	Index IndexFacts
}

// Finding is one rule's verdict. ID is a stable kebab-case identifier (or,
// for a rule that panicked, "rule-error:<name>"); Severity is one of "high",
// "medium", "info", "clean", "insufficient". Window and Impact are the empty
// string and zero for every clean/insufficient finding, matching rules.js's
// quiet().
type Finding struct {
	ID       string
	Severity string
	Title    string
	Evidence string
	Fix      string
	Window   string
	Impact   float64
}
