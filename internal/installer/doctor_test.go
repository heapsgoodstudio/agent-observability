package installer

import (
	"strings"
	"testing"
)

func TestUninstallStepsCoverEverything(t *testing.T) {
	steps := UninstallSteps("/inst", []string{"/p"}, "/Users/x/Applications/Agent Observability.app", 501, false)
	joined := strings.ToLower(strings.Join(Describe(steps), "\n"))
	for _, want := range []string{"agents", "notifier", "telemetry", "statusline"} {
		if !strings.Contains(joined, want) {
			t.Errorf("uninstall missing %q step:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "purge") {
		t.Error("purge step present without --purge")
	}
	purged := strings.ToLower(strings.Join(Describe(UninstallSteps("/inst", []string{"/p"}, "", 501, true)), "\n"))
	if !strings.Contains(purged, "purge") {
		t.Error("purge step missing with --purge")
	}
}
