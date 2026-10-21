package rules

import (
	"fmt"
	"strings"
	"testing"

	"github.com/heapsgoodstudio/agent-observability/internal/pricing"
)

// contains is this file's stand-in for JS's assert.match against a literal
// substring: every regex dashboard/rules.test.mjs asserts with here is a
// literal pattern (no wildcards or alternation), so a substring check is a
// faithful equivalent.
func contains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected %q to contain %q", s, substr)
	}
}

func notContains(t *testing.T, s, substr string) {
	t.Helper()
	if strings.Contains(s, substr) {
		t.Errorf("expected %q NOT to contain %q", s, substr)
	}
}

// sessionWithCache is this file's stand-in for rules.test.mjs's sessionsWith
// helper, narrowed to the two token fields cacheDiscipline and the runRules
// smoke test actually read. models carries the same "claude-opus-5": 10
// fixture the JS helper's default does, for parity even where a given test
// does not assert on it.
func sessionWithCache(cacheRead, cacheCreation float64) []SessionFacts {
	return []SessionFacts{{CacheRead: cacheRead, CacheCreation: cacheCreation, Models: map[string]float64{"claude-opus-5": 10}}}
}

/* realMoney ---------------------------------------------------------------- */

func TestRealMoney(t *testing.T) {
	for name, facts := range map[string]VMFacts{
		"no activity":                     {},
		"zero exhausted-window estimate":  {Cost7d: 100, OverageKnown: true},
		"small exhausted-window estimate": {Cost7d: 100, Overage7d: 2, OverageKnown: true},
		"large exhausted-window estimate": {Cost7d: 100, Overage7d: 91, OverageKnown: true},
		"missing quota series":            {Cost7d: 100, OverageKnown: false},
	} {
		t.Run(name, func(t *testing.T) {
			f := RealMoney(Data{VM: facts})
			if f.Severity != "insufficient" || f.Impact != 0 {
				t.Fatalf("estimates classified billing: %+v", f)
			}
			contains(t, f.Title, "actual overage cannot be determined")
			contains(t, f.Fix, "provider-reported billing display")
			notContains(t, f.Title+f.Evidence+f.Fix, "$")
		})
	}
}

/* cacheDiscipline ------------------------------------------------------------ */

func TestCacheDiscipline(t *testing.T) {
	t.Run("flags context being rebuilt instead of read back", func(t *testing.T) {
		clean := CacheDiscipline(Data{Index: IndexFacts{Sessions: sessionWithCache(10e6, 2e5)}})
		if clean.Severity != "clean" {
			t.Errorf("severity = %q, want clean", clean.Severity)
		}
		bad := CacheDiscipline(Data{Index: IndexFacts{Sessions: sessionWithCache(10e6, 4e6)}})
		if bad.Severity != "high" {
			t.Errorf("severity = %q, want high", bad.Severity)
		}
	})

	t.Run("needs a million tokens before it speaks", func(t *testing.T) {
		f := CacheDiscipline(Data{Index: IndexFacts{Sessions: sessionWithCache(100, 90)}})
		if f.Severity != "insufficient" {
			t.Errorf("severity = %q, want insufficient", f.Severity)
		}
	})
}

/* modelMix ------------------------------------------------------------------- */

func TestModelMix(t *testing.T) {
	t.Run("counts sessions rather than monetary weights", func(t *testing.T) {
		f := ModelMix(Data{Index: IndexFacts{Sessions: []SessionFacts{
			{Models: map[string]float64{"claude-opus-5": 0}},
			{Models: map[string]float64{"claude-opus-5": 1}},
			{Models: map[string]float64{"claude-haiku-4-5": 10000}},
			{}, // sessions without model metadata do not enter the denominator
		}}})
		if f.Severity != "info" {
			t.Errorf("severity = %q, want info", f.Severity)
		}
		contains(t, f.Title, "opus-5")
		contains(t, f.Title, "67% of 3 sessions with model data")
		contains(t, f.Evidence, "2 sessions (67%)")
		notContains(t, f.Title+f.Evidence, "$")
	})
	t.Run("multiple models overlap within sessions", func(t *testing.T) {
		f := ModelMix(Data{Index: IndexFacts{Sessions: []SessionFacts{
			{Models: map[string]float64{"model-b": 999, "model-a": 0}},
		}}})
		contains(t, f.Title, "model-a appears in 100% of 1 sessions")
		contains(t, f.Evidence, "model-b in 1 sessions (100%)")
		contains(t, f.Evidence, "percentages may overlap")
	})
	t.Run("needs model metadata", func(t *testing.T) {
		if f := ModelMix(Data{Index: IndexFacts{Sessions: []SessionFacts{{Models: map[string]float64{"": 123}}}}}); f.Severity != "insufficient" {
			t.Fatalf("empty model identity treated as activity: %+v", f)
		}
	})
}

/* mainThreadConcentration + isMainThreadSource ------------------------------- */

func TestMainThreadConcentration(t *testing.T) {
	t.Run("uses request concentration without monetary weights", func(t *testing.T) {
		concentrated := MainThreadConcentration(Data{VL: VLFacts{BySource: []SourceRow{
			{QuerySource: "repl_main_thread", N: 900, Cost: 0},
			{QuerySource: "agent:custom", N: 100, Cost: 10000},
		}}})
		if concentrated.Severity != "medium" {
			t.Errorf("severity = %q, want medium", concentrated.Severity)
		}
		contains(t, concentrated.Title, "90% of requests")
		contains(t, concentrated.Evidence, "900 main-thread requests and 100 subagent requests out of 1,000 total")
		notContains(t, concentrated.Title+concentrated.Evidence, "$")

		balanced := MainThreadConcentration(Data{VL: VLFacts{BySource: []SourceRow{
			{QuerySource: "repl_main_thread", N: 400, Cost: 100},
			{QuerySource: "agent:custom", N: 400, Cost: 80},
		}}})
		if balanced.Severity != "clean" {
			t.Errorf("severity = %q, want clean", balanced.Severity)
		}
		contains(t, balanced.Title, "50% of requests")
	})

	t.Run("keeps a source in neither bucket in the denominator", func(t *testing.T) {
		// web_fetch_apply is neither the main thread nor a subagent, and it
		// exists in the live store. Dropping it would report 90% instead of 75%.
		f := MainThreadConcentration(Data{VL: VLFacts{BySource: []SourceRow{
			{QuerySource: "repl_main_thread", N: 900, Cost: 327.68},
			{QuerySource: "agent:custom", N: 100, Cost: 41.22},
			{QuerySource: "web_fetch_apply", N: 200, Cost: 40},
		}}})
		if f.Severity != "medium" {
			t.Errorf("severity = %q, want medium", f.Severity)
		}
		contains(t, f.Title, "75% of requests")
		contains(t, f.Evidence, "web_fetch_apply")
	})

	t.Run("needs traffic on both sides", func(t *testing.T) {
		f := MainThreadConcentration(Data{VL: VLFacts{BySource: []SourceRow{
			{QuerySource: "repl_main_thread", N: 10, Cost: 90},
		}}})
		if f.Severity != "insufficient" {
			t.Errorf("severity = %q, want insufficient", f.Severity)
		}
	})
}

func TestIsMainThreadSource(t *testing.T) {
	for _, s := range []string{"repl_main_thread", "repl_main", "main", "sdk", "generate_session_title"} {
		if !IsMainThreadSource(s) {
			t.Errorf("IsMainThreadSource(%q) = false, want true", s)
		}
	}
	// "" stands in for both JS null and undefined: String(source ?? "")
	// collapses both to the empty string before the classifier ever sees
	// them, so this one case covers what JS tests as three.
	for _, s := range []string{"agent:implementor", "web_fetch_apply", ""} {
		if IsMainThreadSource(s) {
			t.Errorf("IsMainThreadSource(%q) = true, want false", s)
		}
	}
}

/* subagentRoi ----------------------------------------------------------------- */

func TestSubagentRoi(t *testing.T) {
	t.Run("reports the spread in tokens per run", func(t *testing.T) {
		f := SubagentRoi(Data{VL: VLFacts{Subagents: []SubagentRow{
			{AgentType: "implementor", Runs: 24, Tok: 2723916, Tools: 300, Ms: 7245373},
			{AgentType: "mechanical-executor", Runs: 17, Tok: 702768, Tools: 200, Ms: 1145164},
		}}})
		if f.Severity != "info" {
			t.Errorf("severity = %q, want info", f.Severity)
		}
		contains(t, f.Title, "implementor")

		wide := SubagentRoi(Data{VL: VLFacts{Subagents: []SubagentRow{
			{AgentType: "heavy", Runs: 10, Tok: 1000000, Tools: 10, Ms: 100},
			{AgentType: "light", Runs: 10, Tok: 100000, Tools: 10, Ms: 100},
		}}})
		if wide.Severity != "medium" {
			t.Errorf("severity = %q, want medium", wide.Severity)
		}
	})

	t.Run("needs at least two agent types with five runs each", func(t *testing.T) {
		f := SubagentRoi(Data{VL: VLFacts{Subagents: []SubagentRow{
			{AgentType: "solo", Runs: 10, Tok: 1000, Tools: 1, Ms: 1},
			{AgentType: "thin", Runs: 3, Tok: 300, Tools: 1, Ms: 1},
		}}})
		if f.Severity != "insufficient" {
			t.Errorf("severity = %q, want insufficient", f.Severity)
		}
	})

	t.Run("says insufficient rather than a 0.0x spread when token counts are missing", func(t *testing.T) {
		f := SubagentRoi(Data{VL: VLFacts{Subagents: []SubagentRow{
			{AgentType: "implementor", Runs: 24, Tok: 0, Tools: 300, Ms: 7245373},
			{AgentType: "general-purpose", Runs: 17, Tok: 0, Tools: 200, Ms: 1145164},
		}}})
		if f.Severity != "insufficient" {
			t.Errorf("severity = %q, want insufficient", f.Severity)
		}
		contains(t, f.Title, "no token counts")
	})

	t.Run("keeps the runs count unformatted in evidence even at four digits", func(t *testing.T) {
		// rules.js's evidence sentence interpolates `${r.runs}` raw, unlike
		// its other numeric fields in the same sentence (which go through
		// int()/toLocaleString). A type with >= 1000 runs must render
		// "over 1234 runs", never comma-grouped as "over 1,234 runs".
		f := SubagentRoi(Data{VL: VLFacts{Subagents: []SubagentRow{
			{AgentType: "big", Runs: 1234, Tok: 12340000, Tools: 1, Ms: 1},
			{AgentType: "small", Runs: 10, Tok: 1000, Tools: 1, Ms: 1},
		}}})
		contains(t, f.Evidence, "over 1234 runs")
		notContains(t, f.Evidence, "1,234")
	})
}

/* turnCost --------------------------------------------------------------------- */

func repeatTurns(n int, cost, reqs float64) []TurnRow {
	rows := make([]TurnRow, n)
	for i := range rows {
		rows[i] = TurnRow{Cost: cost, Reqs: reqs}
	}
	return rows
}

func TestTurnCost(t *testing.T) {
	t.Run("escalates on the spread between typical and worst", func(t *testing.T) {
		flat := TurnCost(Data{VL: VLFacts{Turns: repeatTurns(20, 2, 5)}})
		if flat.Severity != "clean" {
			t.Errorf("severity = %q, want clean", flat.Severity)
		}

		turns := append(repeatTurns(19, 1, 5), TurnRow{Cost: 40, Reqs: 106})
		spiky := TurnCost(Data{VL: VLFacts{Turns: turns}})
		if spiky.Severity != "high" {
			t.Errorf("severity = %q, want high", spiky.Severity)
		}
		contains(t, spiky.Title, "106 requests")
		contains(t, spiky.Evidence, "Median 5 requests per turn")
		notContains(t, spiky.Title+spiky.Evidence, "$")
	})

	t.Run("is medium when p95 is stretched but the max stays under twenty times the median", func(t *testing.T) {
		turns := append(repeatTurns(19, 1, 5), TurnRow{Cost: 0, Reqs: 75})
		f := TurnCost(Data{VL: VLFacts{Turns: turns}})
		if f.Severity != "medium" {
			t.Errorf("severity = %q, want medium", f.Severity)
		}
	})
	t.Run("ignores monetary spikes and requires actual request counts", func(t *testing.T) {
		turns := append(repeatTurns(19, 1, 5), TurnRow{Cost: 99999, Reqs: 5})
		if f := TurnCost(Data{VL: VLFacts{Turns: turns}}); f.Severity != "clean" {
			t.Fatalf("monetary weight drove turn finding: %+v", f)
		}
		if f := TurnCost(Data{VL: VLFacts{Turns: repeatTurns(20, 10, 0)}}); f.Severity != "insufficient" {
			t.Fatalf("cost-only turns treated as request activity: %+v", f)
		}
	})

	t.Run("needs ten turns", func(t *testing.T) {
		f := TurnCost(Data{VL: VLFacts{Turns: []TurnRow{{Cost: 5, Reqs: 2}}}})
		if f.Severity != "insufficient" {
			t.Errorf("severity = %q, want insufficient", f.Severity)
		}
	})
}

/* failureLoops ------------------------------------------------------------------ */

func TestFailureLoops(t *testing.T) {
	t.Run("rates failures against the whole tool population", func(t *testing.T) {
		rows := []ToolResultRow{
			{ToolName: "Bash", Success: true, N: 900},
			{ToolName: "Bash", Success: false, N: 32},
			{ToolName: "Read", Success: true, N: 300},
		}
		f := FailureLoops(Data{VL: VLFacts{
			ToolResults:      rows,
			WorstFailSession: WorstFail{Found: true, SessionID: "abc123de-0000-4000-8000-000000000000", N: 21},
		}})
		if f.Severity != "medium" {
			t.Errorf("severity = %q, want medium", f.Severity)
		}
		contains(t, f.Evidence, "Bash")
		contains(t, f.Evidence, "abc123de")
	})

	t.Run("is clean below the threshold and insufficient below the sample floor", func(t *testing.T) {
		clean := FailureLoops(Data{VL: VLFacts{ToolResults: []ToolResultRow{
			{ToolName: "Bash", Success: true, N: 1000},
			{ToolName: "Bash", Success: false, N: 5},
		}}})
		if clean.Severity != "clean" {
			t.Errorf("severity = %q, want clean", clean.Severity)
		}

		thin := FailureLoops(Data{VL: VLFacts{ToolResults: []ToolResultRow{
			{ToolName: "Bash", Success: true, N: 10},
		}}})
		if thin.Severity != "insufficient" {
			t.Errorf("severity = %q, want insufficient", thin.Severity)
		}
	})

	t.Run("escalates to high above a five percent failure rate", func(t *testing.T) {
		f := FailureLoops(Data{VL: VLFacts{ToolResults: []ToolResultRow{
			{ToolName: "Bash", Success: true, N: 900},
			{ToolName: "Bash", Success: false, N: 100},
		}}})
		if f.Severity != "high" {
			t.Errorf("severity = %q, want high", f.Severity)
		}
	})
}

/* permissionFriction -------------------------------------------------------------- */

func TestPermissionFriction(t *testing.T) {
	t.Run("names what is approved by hand", func(t *testing.T) {
		clean := PermissionFriction(Data{VL: VLFacts{Decisions: []DecisionRow{
			{Source: "config", Decision: "accept", ToolName: "Bash", N: 2033},
			{Source: "user_temporary", Decision: "accept", ToolName: "AskUserQuestion", N: 30},
		}}})
		if clean.Severity != "clean" {
			t.Errorf("severity = %q, want clean", clean.Severity)
		}

		noisy := PermissionFriction(Data{VL: VLFacts{Decisions: []DecisionRow{
			{Source: "config", Decision: "accept", ToolName: "Bash", N: 100},
			{Source: "user_temporary", Decision: "accept", ToolName: "Bash", N: 60},
		}}})
		if noisy.Severity != "medium" {
			t.Errorf("severity = %q, want medium", noisy.Severity)
		}
		contains(t, noisy.Evidence, "Bash")

		rejected := PermissionFriction(Data{VL: VLFacts{Decisions: []DecisionRow{
			{Source: "config", Decision: "accept", ToolName: "Bash", N: 100},
			{Source: "user_temporary", Decision: "reject", ToolName: "Write", N: 20},
		}}})
		if rejected.Severity != "high" {
			t.Errorf("severity = %q, want high", rejected.Severity)
		}
	})

	t.Run("does not charge a config deny rule as friction", func(t *testing.T) {
		// A permissions.deny rule firing on 20 of 120 decisions is the
		// config doing its job with no human in the loop. Counting it would
		// title the card 17% rejections while the evidence below it listed
		// no manual decisions at all.
		f := PermissionFriction(Data{VL: VLFacts{Decisions: []DecisionRow{
			{Source: "config", Decision: "accept", ToolName: "Bash", N: 100},
			{Source: "config", Decision: "reject", ToolName: "WebFetch", N: 20},
		}}})
		if f.Severity != "clean" {
			t.Errorf("severity = %q, want clean", f.Severity)
		}
		contains(t, f.Title, "100% of tool decisions came from config")
	})

	t.Run("never prints a dangling most-often sentence", func(t *testing.T) {
		f := PermissionFriction(Data{VL: VLFacts{Decisions: []DecisionRow{
			{Source: "config", Decision: "accept", ToolName: "Bash", N: 100},
			{Source: "user_temporary", Decision: "reject", ToolName: "Write", N: 20},
		}}})
		if f.Severity != "high" {
			t.Errorf("severity = %q, want high", f.Severity)
		}
		notContains(t, f.Evidence, "Most often: .")
		contains(t, f.Evidence, "20 manual decisions of 120. Most often: Write 20.")
	})

	t.Run("needs a hundred decisions before it speaks", func(t *testing.T) {
		f := PermissionFriction(Data{VL: VLFacts{Decisions: []DecisionRow{
			{Source: "config", Decision: "accept", ToolName: "Bash", N: 50},
		}}})
		if f.Severity != "insufficient" {
			t.Errorf("severity = %q, want insufficient", f.Severity)
		}
	})
}

/* hookTax --------------------------------------------------------------------------- */

func TestHookTax(t *testing.T) {
	t.Run("measures per-run cost and treats any error as serious", func(t *testing.T) {
		clean := HookTax(Data{VL: VLFacts{Hooks: []HookRow{
			{HookName: "PreToolUse:Bash", Runs: 979, Ms: 47792, Errs: 0},
		}}})
		if clean.Severity != "clean" {
			t.Errorf("severity = %q, want clean", clean.Severity)
		}

		slow := HookTax(Data{VL: VLFacts{Hooks: []HookRow{
			{HookName: "PreToolUse:Bash", Runs: 100, Ms: 40000, Errs: 0},
		}}})
		if slow.Severity != "medium" {
			t.Errorf("severity = %q, want medium", slow.Severity)
		}
		contains(t, slow.Title, "400ms")

		broken := HookTax(Data{VL: VLFacts{Hooks: []HookRow{
			{HookName: "Stop", Runs: 30, Ms: 300, Errs: 4},
		}}})
		if broken.Severity != "high" {
			t.Errorf("severity = %q, want high", broken.Severity)
		}
	})

	t.Run("needs twenty runs on some hook", func(t *testing.T) {
		f := HookTax(Data{VL: VLFacts{Hooks: []HookRow{
			{HookName: "Stop", Runs: 3, Ms: 30, Errs: 0},
		}}})
		if f.Severity != "insufficient" {
			t.Errorf("severity = %q, want insufficient", f.Severity)
		}
	})

	t.Run("does not blame a hook that never errored for another hook's errors", func(t *testing.T) {
		// PostToolUse:Read cleared the twenty-run floor and never errored.
		// SessionStart has a single run and one error but never clears the
		// floor, so its error must not leak into the verdict for
		// PostToolUse:Read.
		f := HookTax(Data{VL: VLFacts{Hooks: []HookRow{
			{HookName: "PostToolUse:Read", Runs: 500, Ms: 10000, Errs: 0},
			{HookName: "SessionStart", Runs: 1, Ms: 5, Errs: 1},
		}}})
		if f.Severity != "clean" {
			t.Errorf("severity = %q, want clean", f.Severity)
		}
	})

	t.Run("says insufficient rather than clean when no durations were recorded", func(t *testing.T) {
		f := HookTax(Data{VL: VLFacts{Hooks: []HookRow{
			{HookName: "PreToolUse:Bash", Runs: 979, Ms: 0, Errs: 0},
		}}})
		if f.Severity != "insufficient" {
			t.Errorf("severity = %q, want insufficient", f.Severity)
		}
		contains(t, f.Title, "no hook durations")
	})

	t.Run("still reports errors when no durations were recorded", func(t *testing.T) {
		// errs and ms are different fields. An absent duration withholds
		// the timing claim only; the errors were counted and are
		// judgeable on their own.
		f := HookTax(Data{VL: VLFacts{Hooks: []HookRow{
			{HookName: "PreToolUse:Bash", Runs: 979, Ms: 0, Errs: 0},
			{HookName: "Stop", Runs: 40, Ms: 0, Errs: 6},
		}}})
		if f.Severity != "high" {
			t.Errorf("severity = %q, want high", f.Severity)
		}
		contains(t, f.Title, "6 hook errors on 1 hook")
		contains(t, f.Evidence, "Stop 6 of 40 runs")
		contains(t, f.Evidence, "No durations were recorded")
		notContains(t, f.Evidence, "0ms")
	})
}

/* normServer ------------------------------------------------------------------------- */

func TestNormServer(t *testing.T) {
	// mcp-freight ports rules.js's normServer as pricing.NormServer rather
	// than a second copy in this package: see McpFreight and the task
	// report. This test asserts the exact fixture values
	// dashboard/rules.test.mjs's normServer test does, against that shared
	// implementation.
	cases := map[string]string{
		"plugin:linear:linear":   "plugin_linear_linear",
		"claude.ai Google Drive": "claude_ai_google_drive",
		"claude_ai_Google_Drive": "claude_ai_google_drive",
	}
	for in, want := range cases {
		if got := pricing.NormServer(in); got != want {
			t.Errorf("NormServer(%q) = %q, want %q", in, got, want)
		}
	}
}

/* mcpFreight ------------------------------------------------------------------------- */

func TestMcpFreight(t *testing.T) {
	t.Run("finds servers that connect and are never called", func(t *testing.T) {
		f := McpFreight(Data{
			VL: VLFacts{
				MCPConnected: []ServerRow{{ServerName: "plugin:linear:linear", N: 10}, {ServerName: "claude.ai Gmail", N: 10}},
				MCPFailed:    nil,
			},
			Index: IndexFacts{Sessions: []SessionFacts{{MCPServers: []string{"plugin_linear_linear"}}}},
		})
		if f.Severity != "medium" {
			t.Errorf("severity = %q, want medium", f.Severity)
		}
		contains(t, f.Evidence, "claude_ai_gmail")
	})

	t.Run("treats a failed connection as serious and silence as clean", func(t *testing.T) {
		failed := McpFreight(Data{
			VL:    VLFacts{MCPConnected: []ServerRow{{ServerName: "codex", N: 7}}, MCPFailed: []ServerRow{{ServerName: "codex", N: 3}}},
			Index: IndexFacts{Sessions: []SessionFacts{{MCPServers: []string{"codex"}}}},
		})
		if failed.Severity != "high" {
			t.Errorf("severity = %q, want high", failed.Severity)
		}

		fine := McpFreight(Data{
			VL:    VLFacts{MCPConnected: []ServerRow{{ServerName: "codex", N: 7}}, MCPFailed: nil},
			Index: IndexFacts{Sessions: []SessionFacts{{MCPServers: []string{"codex"}}}},
		})
		if fine.Severity != "clean" {
			t.Errorf("severity = %q, want clean", fine.Severity)
		}

		none := McpFreight(Data{VL: VLFacts{MCPConnected: nil, MCPFailed: nil}, Index: IndexFacts{Sessions: nil}})
		if none.Severity != "insufficient" {
			t.Errorf("severity = %q, want insufficient", none.Severity)
		}
	})

	t.Run("will not call servers unused when the session index is empty", func(t *testing.T) {
		// control.js falls back to { sessions: [] } when index.json fails
		// to load. Without this guard every connected server reads as
		// never called, and the card tells the user to disconnect nine
		// servers that work.
		f := McpFreight(Data{
			VL: VLFacts{
				MCPConnected: []ServerRow{{ServerName: "plugin:linear:linear", N: 10}, {ServerName: "claude.ai Gmail", N: 10}},
				MCPFailed:    nil,
			},
			Index: IndexFacts{Sessions: nil},
		})
		if f.Severity != "insufficient" {
			t.Errorf("severity = %q, want insufficient", f.Severity)
		}
		contains(t, f.Title, "session index is unavailable")
	})

	t.Run("still reports failed connections when the session index is empty", func(t *testing.T) {
		// A failed connection is observed in the log store and does not
		// depend on the index. Only the never-called half goes quiet, and
		// says why.
		f := McpFreight(Data{
			VL: VLFacts{
				MCPConnected: []ServerRow{{ServerName: "codex", N: 7}, {ServerName: "claude.ai Gmail", N: 4}},
				MCPFailed:    []ServerRow{{ServerName: "codex", N: 3}},
			},
			Index: IndexFacts{Sessions: nil},
		})
		if f.Severity != "high" {
			t.Errorf("severity = %q, want high", f.Severity)
		}
		contains(t, f.Title, "1 mcp server failed to connect")
		contains(t, f.Evidence, "failed to connect: codex")
		// No never-called list at all: that is the claim the missing index
		// cannot support. The caveat below is the only place the phrase may
		// appear.
		notContains(t, f.Evidence, "never called: ")
		contains(t, f.Evidence, "session index was unavailable, so servers that connect but are never called could not be identified")
	})
}

/* Run ------------------------------------------------------------------------------------ */

var severityRankOrder = []string{"high", "medium", "info", "clean", "insufficient"}

func bySeverityName(sev string) int {
	for i, s := range severityRankOrder {
		if s == sev {
			return i
		}
	}
	return len(severityRankOrder)
}

func TestRun(t *testing.T) {
	t.Run("sorts high before medium before info and drops nothing", func(t *testing.T) {
		data := Data{
			VM:    VMFacts{Cost7d: 100, Overage7d: 91, OverageKnown: true, ActiveHours7d: 0, Commits7d: 0, Lines7d: 0},
			Index: IndexFacts{Sessions: sessionWithCache(10e6, 4e6)},
			VL: VLFacts{
				BySource: []SourceRow{
					{QuerySource: "repl_main_thread", N: 364, Cost: 327.68},
					{QuerySource: "agent:custom", N: 1099, Cost: 41.22},
				},
				Subagents: []SubagentRow{
					{AgentType: "a", Runs: 10, Tok: 100, Tools: 1, Ms: 1},
					{AgentType: "b", Runs: 10, Tok: 100, Tools: 1, Ms: 1},
				},
				Turns:            repeatTurns(20, 2, 5),
				ToolResults:      []ToolResultRow{{ToolName: "Bash", Success: true, N: 1000}},
				WorstFailSession: WorstFail{},
				Decisions:        []DecisionRow{{Source: "config", Decision: "accept", ToolName: "Bash", N: 200}},
				Hooks:            []HookRow{{HookName: "Stop", Runs: 30, Ms: 300, Errs: 0}},
				MCPConnected:     []ServerRow{{ServerName: "codex", N: 3}},
				MCPFailed:        nil,
			},
		}
		out := Run(data)
		if len(out) != 10 {
			t.Fatalf("len(out) = %d, want 10", len(out))
		}
		if out[0].ID != "cache-discipline" {
			t.Errorf("out[0].ID = %q, want cache-discipline", out[0].ID)
		}
		for i := 1; i < len(out); i++ {
			if bySeverityName(out[i-1].Severity) > bySeverityName(out[i].Severity) {
				t.Errorf("findings not sorted by severity at index %d: %q before %q", i, out[i-1].Severity, out[i].Severity)
			}
		}
	})

	t.Run("recovers from a rule that throws with a fallback id that cannot be mistaken for a real rule id", func(t *testing.T) {
		// dashboard/rules.test.mjs exercises this by omitting `vl` entirely
		// from the data object it hands runRules, which makes every rule
		// that reads `d.vl.<field>` throw a TypeError against `undefined`.
		// Go's Data has no such state: VMFacts/VLFacts/IndexFacts are plain
		// value structs, so a "missing" VL is a zero-valued VLFacts (nil
		// slices, empty WorstFail), and every rule here is written to treat
		// that as "not enough data" rather than to panic on it, matching
		// rules.js's own defensive intent. There is therefore no valid Data
		// value that makes a real rule function panic, so this test
		// exercises the exact same recovery path (runOne's defer/recover,
		// the "rule-error:<name>" id shape, severity, window and impact)
		// against an injected rule that panics on purpose. See the task
		// report for the full reasoning.
		f := runOne("mainThreadConcentration", func(Data) Finding {
			panic(fmt.Errorf("boom"))
		}, Data{})
		if f.ID != "rule-error:mainThreadConcentration" {
			t.Errorf("id = %q, want rule-error:mainThreadConcentration", f.ID)
		}
		if f.Severity != "insufficient" {
			t.Errorf("severity = %q, want insufficient", f.Severity)
		}
		contains(t, f.Title, "rule failed")
		if f.Window != "" {
			t.Errorf("window = %q, want empty", f.Window)
		}
		if f.Impact != 0 {
			t.Errorf("impact = %v, want 0", f.Impact)
		}
	})
}
