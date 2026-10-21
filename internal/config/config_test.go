package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadParsesConfigEnv(t *testing.T) {
	home, _ := os.UserHomeDir()
	dir := t.TempDir()
	content := `# comment line

OBS_DIR="$(cd "$(dirname x)" && pwd)"
PROFILE_DIR="$HOME/.claude"
CLAUDE_BIN="${HOME}/.local/bin/claude"
LABEL_PREFIX="com.claude-observability"
VM_VERSION="v1.149.0"
USAGE_PROBE_MIN_AGE_S=300  # only probe when older than this
EMPTY=""
noequalsline
`
	path := filepath.Join(dir, "config.env")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"OBS_DIR":               "", // $( lines are skipped
		"PROFILE_DIR":           home + "/.claude",
		"CLAUDE_BIN":            home + "/.local/bin/claude",
		"LABEL_PREFIX":          "com.claude-observability",
		"VM_VERSION":            "v1.149.0",
		"USAGE_PROBE_MIN_AGE_S": "300", // trailing comment stripped
		"EMPTY":                 "",    // explicit empty values are retained
	}
	for k, want := range cases {
		if got := c.Get(k, ""); got != want {
			t.Errorf("Get(%q) = %q, want %q", k, got, want)
		}
	}
	if c.Dir != dir {
		t.Errorf("Dir = %q, want %q", c.Dir, dir)
	}
	if got := c.Path("state", "claude-obs.db"); got != filepath.Join(dir, "state", "claude-obs.db") {
		t.Errorf("Path() = %q", got)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "config.env")); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestCheckInstallDirRefusesTheSourceCheckout(t *testing.T) {
	// An install root, as claude-obs install leaves it: bin/, notifier/,
	// config.env and the state tree, and no go.mod anywhere.
	install := t.TempDir()
	for _, d := range []string{"bin", "notifier", "state", "dashboard/data"} {
		if err := os.MkdirAll(filepath.Join(install, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(install, "config.env"), []byte("PROFILE_DIR=/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckInstallDir(install); err != nil {
		t.Errorf("CheckInstallDir(install root) = %v, want nil", err)
	}

	// A scratch dir a dev points `serve --install-dir` at is likewise fine.
	if err := CheckInstallDir(t.TempDir()); err != nil {
		t.Errorf("CheckInstallDir(empty scratch dir) = %v, want nil", err)
	}

	// The source checkout is the one case that must be refused: writing state
	// there is what left 89 MB of a dead install inside the repository.
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckInstallDir(checkout); err == nil {
		t.Error("CheckInstallDir(source checkout) = nil, want an error")
	}

	// The real repository root, reached from this package, is the case that
	// matters in practice; pin it so a future layout change cannot silently
	// stop the guard from recognising it.
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckInstallDir(repo); err == nil {
		t.Errorf("CheckInstallDir(%s) = nil, want an error", repo)
	}
}

// config.env is config.env.example with a machine-local block appended, so
// every key the example ships appears twice: once with the example's default
// and once with this machine's real value. bash takes the last assignment and
// so must Load.
//
// This is load-bearing for upgrades. `claude-obs install` reuses the recorded
// PROFILE_DIR so a re-install does not have to ask again; reading the
// example's PROFILE_DIR="$HOME/.claude" instead of the machine block's real
// value would silently re-point the whole stack at a different profile.
func TestLoadTakesTheLastAssignment(t *testing.T) {
	home, _ := os.UserHomeDir()
	dir := t.TempDir()
	// The exact shape stepConfigEnv produces: the example, then the block.
	content := `PROFILE_DIR="$HOME/.claude"
CLAUDE_BIN="${HOME}/.local/bin/claude"
LABEL_PREFIX="com.claude-observability"
DAILY_BUDGET_USD=750

# Machine-local values written by claude-obs install on 2026-08-17
PROFILE_DIR="/Users/x/Claude/.claude-acme"
CLAUDE_BIN="/opt/bin/claude"
LABEL_PREFIX="com.claude-observability"
`
	path := filepath.Join(dir, "config.env")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, want string }{
		{"PROFILE_DIR", "/Users/x/Claude/.claude-acme"},
		{"CLAUDE_BIN", "/opt/bin/claude"},
		{"LABEL_PREFIX", "com.claude-observability"},
		{"DAILY_BUDGET_USD", "750"}, // only in the example half; still read
	} {
		if got := c.Get(tc.key, ""); got != tc.want {
			t.Errorf("Get(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
	// Guard the specific failure: the example's default must not survive.
	if got := c.Get("PROFILE_DIR", ""); got == filepath.Join(home, ".claude") {
		t.Error("PROFILE_DIR resolved to the example's default, not the machine block")
	}
}

func TestLoadEmptyOverridesEarlierDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(path, []byte("PROFILE_DIR=\"/some/default\"\nPROFILE_DIR=\"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Get("PROFILE_DIR", "fallback"); got != "" {
		t.Fatalf("empty became %q", got)
	}
	if value, present := cfg.Lookup("PROFILE_DIR"); !present || value != "" {
		t.Fatalf("lookup %q %v", value, present)
	}
}
