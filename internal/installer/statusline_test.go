package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

func TestStatuslineScriptsUseRegistryIdentityAfterDirectoryChange(t *testing.T) {
	if _, err := os.Stat("/usr/bin/jq"); err != nil {
		t.Skip("statusline requires macOS /usr/bin/jq")
	}
	inst := t.TempDir()
	os.MkdirAll(filepath.Join(inst, "state"), 0o755)
	os.MkdirAll(filepath.Join(inst, "bin"), 0o755)
	for _, name := range []string{"statusline.sh", "statusline-tap.sh"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "bin", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inst, "bin", name), data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := profiles.New(inst)
	dir := filepath.Join(t.TempDir(), ".claude-work")
	os.MkdirAll(dir, 0o755)
	p, err := s.Save(profiles.Profile{Provider: profiles.Claude, Name: "Work", Directory: dir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "a-new-name")
	os.MkdirAll(replacement, 0o755)
	p.Directory = replacement
	p, err = s.Save(p)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/bash", filepath.Join(inst, "bin", "statusline-tap.sh"))
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+replacement)
	cmd.Stdin = strings.NewReader(`{"rate_limits":{"five_hour":{"used_percentage":20}}}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tap: %s %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(inst, "state", "statusline-latest."+p.ID+".json")); err != nil {
		t.Fatal("tap did not use stable ID:", err)
	}
	os.WriteFile(filepath.Join(inst, "state", "statusline-segment."+p.ID+".txt"), []byte("stable-segment"), 0o600)
	cmd = exec.Command("/bin/bash", filepath.Join(inst, "bin", "statusline.sh"))
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+replacement)
	cmd.Stdin = strings.NewReader(`{"cost":{"total_cost_usd":9876.54},"rate_limits":{"five_hour":{"used_percentage":20}}}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("statusline: %s %v", out, err)
	}
	if strings.Contains(string(out), "$") || strings.Contains(string(out), "9876") {
		t.Fatalf("statusline valued included/unclassified session usage: %s", out)
	}
	if !strings.Contains(string(out), "stable-segment") {
		t.Fatalf("statusline did not use stable ID: %s", out)
	}
}
func TestWizardNoAgentProfilesNeedsNoSelection(t *testing.T) {
	in := Inputs{InstallDir: t.TempDir()}
	if err := RunWizard(Report{}, &in, true, "ask"); err != nil {
		t.Fatal(err)
	}
	if in.ProfileDir != "" || !in.Backfill {
		t.Fatalf("empty install: %+v", in)
	}
	if err := RunWizard(Report{}, &in, true, "no"); err != nil {
		t.Fatal(err)
	}
	if in.Backfill {
		t.Fatal("--no-backfill ignored")
	}
}

func TestPollerHonorsEmptyProjectionAndStableIDs(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("poller requires jq")
	}
	inst := t.TempDir()
	os.MkdirAll(filepath.Join(inst, "bin"), 0o755)
	os.MkdirAll(filepath.Join(inst, "state"), 0o755)
	os.MkdirAll(filepath.Join(inst, "logs"), 0o755)
	data, err := os.ReadFile(filepath.Join("..", "..", "bin", "limits-poller.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(inst, "bin", "limits-poller.sh")
	os.WriteFile(script, data, 0o755)
	s := profiles.New(inst)
	if _, err := s.Initialize(t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateConfig(func(existing []byte) ([]byte, error) {
		return append(existing, []byte("\nOBS_DIR=\""+inst+"\"\nCLAUDE_BIN=\"\"\nUSAGE_PROBE=0\n")...), nil
	}); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		t.Helper()
		cmd := exec.Command("/bin/bash", script)
		cmd.Env = append(os.Environ(), "POLLER_DRY_RUN=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("poller: %s %v", out, err)
		}
		return string(out)
	}
	if out := run(); out != "" {
		t.Fatalf("empty registry emitted %s", out)
	}
	dir := filepath.Join(t.TempDir(), ".claude-work")
	os.MkdirAll(dir, 0o755)
	p, err := s.Save(profiles.Profile{Provider: profiles.Claude, Name: "Work", Directory: dir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "renamed")
	os.MkdirAll(replacement, 0o755)
	p.Directory = replacement
	if _, err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(replacement, ".claude.json"), []byte(`{}`), 0o600)
	out := run()
	if !strings.Contains(out, `profile="`+p.ID+`"`) {
		t.Fatalf("poller lost stable ID: %s", out)
	}
	if strings.Contains(out, `profile="renamed"`) {
		t.Fatalf("poller used directory name: %s", out)
	}
}
