package report

import (
	"errors"
	"strings"
	"testing"
)

func TestHookFailureCannotBeParsedAsItsEchoedExampleReport(t *testing.T) {
	output := "UserPromptSubmit operation blocked by hook:\n[repo-guard]: use the account's workspace.\n\nOriginal prompt: PRIVATE\n" + `{"schema":2,"headline":"Example","summary":"This is the prompt, not a report."}`
	if _, err := Parse([]byte(output)); !errors.Is(err, ErrExecution) || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), "Example") {
		t.Fatalf("hook failure accepted or prompt leaked: %v", err)
	}
	if err := ExecutionError([]byte(output)); err == nil || !strings.Contains(err.Error(), "use the account's workspace") {
		t.Fatalf("missing actionable hook diagnostic: %v", err)
	}
	valid := `{"schema":2,"headline":"Hook configuration","summary":"The session examined a UserPromptSubmit hook."}`
	if _, err := Parse([]byte(valid)); err != nil {
		t.Fatalf("ordinary report mentioning hooks rejected: %v", err)
	}
}
