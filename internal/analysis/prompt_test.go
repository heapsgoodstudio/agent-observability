package analysis

import (
	"regexp"
	"strings"
	"testing"
)

// placeholderRE matches an unsubstituted {PLACEHOLDER}. A frame that ships one
// reaches the model as literal braces, which is a silent prompt bug: the run
// still costs money and still produces a report, just a worse one.
var placeholderRE = regexp.MustCompile(`\{[A-Z_]+\}`)

func TestAnalysisPromptSubstitutesEverything(t *testing.T) {
	p, err := analysisPrompt("11111111-1111-4111-8111-111111111111", "/t/transcript.jsonl", `{"requests":3}`, "session x, 3 requests")
	if err != nil {
		t.Fatalf("analysisPrompt: %v", err)
	}
	if left := placeholderRE.FindAllString(p, -1); len(left) > 0 {
		t.Errorf("unsubstituted placeholders: %v", left)
	}
	for _, want := range []string{
		"11111111-1111-4111-8111-111111111111",
		"/t/transcript.jsonl",
		`{"requests":3}`,
		"session x, 3 requests",
		`"schema": 2`,
		"helper agents",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
}

// The analyse path allows Agent (AnalysisDisallowedTools denies Write, Edit,
// NotebookEdit and Bash, and --allowedTools permits rather than restricts), so
// the only bound on a run's fan-out is what the frame tells it. A measured run
// dispatched 4 readers over an 18MB transcript for $5.02; nothing but this text
// stops the next one dispatching 20, or a reader dispatching its own.
func TestAnalysisPromptBoundsItsFanOut(t *testing.T) {
	p, err := analysisPrompt("11111111-1111-4111-8111-111111111111", "/t/t.jsonl", "{}", "h")
	if err != nil {
		t.Fatalf("analysisPrompt: %v", err)
	}
	for _, want := range []string{
		"At most four readers",
		"may not dispatch readers of its own",
		"say in `coverage` how much of the transcript was actually",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the fan-out bound is missing %q", want)
		}
	}
	if strings.Contains(p, "Agent") && !strings.Contains(p, "at most four readers") &&
		!strings.Contains(p, "At most four readers") {
		t.Error("the prompt invites Agent use without stating a cap")
	}
}

func TestAdvicePromptSubstitutesEveryDepth(t *testing.T) {
	for depth := range Depths {
		t.Run(depth, func(t *testing.T) {
			p, err := advicePrompt(depth, []byte(`[{"id":"x"}]`), "/t/index.json", "/t/profile", "/t/sessions")
			if err != nil {
				t.Fatalf("advicePrompt: %v", err)
			}
			if left := placeholderRE.FindAllString(p, -1); len(left) > 0 {
				t.Errorf("unsubstituted placeholders: %v", left)
			}
			for _, want := range []string{"/t/index.json", "/t/profile", `"id": "x"`, `"schema": 2`} {
				if !strings.Contains(p, want) {
					t.Errorf("prompt is missing %q", want)
				}
			}
		})
	}
}

func TestAdvicePromptRejectsUnknownDepth(t *testing.T) {
	if _, err := advicePrompt("thorough", nil, "/i", "/p", "/s"); err == nil {
		t.Error("advicePrompt accepted an unknown depth, want an error")
	}
}

func TestPromptsCarryNoEmDashes(t *testing.T) {
	for _, name := range []string{
		"prompts/report-contract.md",
		"prompts/analysis-prompt.md",
		"prompts/advice-prompt.md",
		"prompts/advice-depth-shallow.md",
		"prompts/advice-depth-standard.md",
		"prompts/advice-depth-evidence.md",
	} {
		data, err := promptFS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(data), "—") {
			t.Errorf("%s contains an em dash", name)
		}
	}
}
