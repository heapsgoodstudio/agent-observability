package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heapsgoodstudio/agent-observability/internal/config"
)

const exampleEnv = `OBS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROFILE_DIR="$HOME/.claude"
CLAUDE_BIN="$HOME/.local/bin/claude"
LABEL_PREFIX="com.claude-observability"
VM_VERSION="v1.149.0"
DAILY_BUDGET_USD=750       # rolling-24h notional budget
NOTIFIER_APP="$HOME/Applications/Agent Observability.app"
NEW_KEY=42
`

func TestMergeConfigEnvOnAFreshInstall(t *testing.T) {
	out, kept, extra := MergeConfigEnv([]byte(exampleEnv), nil)
	if len(kept) != 0 || len(extra) != 0 {
		t.Fatalf("fresh install kept %v extra %v, want none", kept, extra)
	}
	if string(out) != exampleEnv {
		t.Errorf("fresh install did not reproduce the example:\n%s", out)
	}
}

func TestMergeConfigEnvPreservesTuningAndAddsNewKeys(t *testing.T) {
	// A previous install: the old example (no NEW_KEY, older VM_VERSION),
	// one tuned threshold, one operator-only key, plus the machine block.
	existing := `OBS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROFILE_DIR="$HOME/.claude"
CLAUDE_BIN="$HOME/.local/bin/claude"
LABEL_PREFIX="com.claude-observability"
VM_VERSION="v1.100.0"
DAILY_BUDGET_USD=2000      # raised for the migration
NOTIFIER_APP="$HOME/Applications/Agent Observability.app"
POLLER_TAP_FILE="/tmp/tap.json"

# Machine-local values written by claude-obs install on 2026-08-01
PROFILE_DIR="/Users/x/Claude/.claude-acme"
CLAUDE_BIN="/opt/bin/claude"
LABEL_PREFIX="com.claude-observability"
`
	out, kept, extra := MergeConfigEnv([]byte(exampleEnv), []byte(existing))
	got := string(out)

	// The operator's tuned value survives the upgrade.
	if !strings.Contains(got, "DAILY_BUDGET_USD=2000      # raised for the migration") {
		t.Errorf("tuned DAILY_BUDGET_USD was reset:\n%s", got)
	}
	// A key the example gained is added at its default.
	if !strings.Contains(got, "NEW_KEY=42") {
		t.Errorf("new example key missing:\n%s", got)
	}
	// A key only this machine has is carried, not dropped.
	if !strings.Contains(got, `POLLER_TAP_FILE="/tmp/tap.json"`) {
		t.Errorf("operator-only key dropped:\n%s", got)
	}
	if len(extra) != 1 || extra[0] != "POLLER_TAP_FILE" {
		t.Errorf("extra = %v, want [POLLER_TAP_FILE]", extra)
	}
	// Machine-owned keys are never carried over: the block that follows owns
	// them, so the example's generic defaults must survive here.
	if strings.Contains(got, "/Users/x/Claude/.claude-acme") || strings.Contains(got, "/opt/bin/claude") {
		t.Errorf("machine-owned value leaked into the example half:\n%s", got)
	}
	if !strings.Contains(got, `PROFILE_DIR="$HOME/.claude"`) {
		t.Errorf("example PROFILE_DIR line was rewritten:\n%s", got)
	}
	// The $(...) expression must stay an expression.
	if !strings.Contains(got, `OBS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"`) {
		t.Errorf("OBS_DIR expression mangled:\n%s", got)
	}
	// A divergence from the shipped default is reported, not silent.
	joined := strings.Join(kept, "; ")
	if !strings.Contains(joined, "VM_VERSION") || !strings.Contains(joined, "v1.149.0") {
		t.Errorf("VM_VERSION divergence not reported: %v", kept)
	}
	if !strings.Contains(joined, "DAILY_BUDGET_USD") {
		t.Errorf("DAILY_BUDGET_USD divergence not reported: %v", kept)
	}
	// An untouched $HOME line must not be rewritten to an expanded path just
	// because the upgrade ran.
	if !strings.Contains(got, `NOTIFIER_APP="$HOME/Applications/Agent Observability.app"`) {
		t.Errorf("untouched $HOME line churned:\n%s", got)
	}
	for _, k := range kept {
		if strings.HasPrefix(k, "NOTIFIER_APP") {
			t.Errorf("unchanged key reported as diverged: %v", kept)
		}
	}
}

func TestMergeConfigEnvMigratesTheRenamedNotifierDefault(t *testing.T) {
	existing := strings.Replace(exampleEnv,
		`NOTIFIER_APP="$HOME/Applications/Agent Observability.app"`,
		`NOTIFIER_APP="$HOME/Applications/Claude Observability.app"`, 1)
	out, kept, _ := MergeConfigEnv([]byte(exampleEnv), []byte(existing))
	got := string(out)
	if !strings.Contains(got, `NOTIFIER_APP="$HOME/Applications/Agent Observability.app"`) {
		t.Fatalf("renamed notifier default was not migrated:\n%s", got)
	}
	if strings.Contains(got, `NOTIFIER_APP="$HOME/Applications/Claude Observability.app"`) {
		t.Fatalf("old notifier default survived migration:\n%s", got)
	}
	for _, k := range kept {
		if strings.HasPrefix(k, "NOTIFIER_APP") {
			t.Fatalf("renamed default reported as an operator override: %v", kept)
		}
	}
}

func TestMergeConfigEnvPreservesACustomNotifierPath(t *testing.T) {
	existing := strings.Replace(exampleEnv,
		`NOTIFIER_APP="$HOME/Applications/Agent Observability.app"`,
		`NOTIFIER_APP="/Applications/My Alerts.app"`, 1)
	out, kept, _ := MergeConfigEnv([]byte(exampleEnv), []byte(existing))
	if !strings.Contains(string(out), `NOTIFIER_APP="/Applications/My Alerts.app"`) {
		t.Fatalf("custom notifier path was reset:\n%s", out)
	}
	if !strings.Contains(strings.Join(kept, "; "), "NOTIFIER_APP") {
		t.Fatalf("custom notifier path was not reported: %v", kept)
	}
}

// TestConfiguredProfilesReadsTheListOrFallsBack mirrors what
// bin/limits-poller.sh does with "${PROFILE_DIRS:-$PROFILE_DIR}". The two
// must agree: the poller decides which profiles are polled and this decides
// which ones doctor checks and uninstall unwires, so a disagreement leaves a
// profile exporting telemetry that nothing reports on.
func TestConfiguredProfilesReadsTheListOrFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want []string
	}{
		{"list wins", "PROFILE_DIR=\"/a\"\nPROFILE_DIRS=\"/a:/b\"\n", []string{"/a", "/b"}},
		{"falls back to the single profile", "PROFILE_DIR=\"/a\"\n", []string{"/a"}},
		{"dedupes and drops blanks", "PROFILE_DIR=\"/a\"\nPROFILE_DIRS=\"/a:/a/:/b:\"\n", []string{"/a", "/b"}},
		{"nothing configured", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.env")
			if err := os.WriteFile(path, []byte(tc.env), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			got := ConfiguredProfiles(cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
