package installer

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
// Every open session of a profile renders into one tap file, and each render
// carries the figures that session last heard from the API, however long ago.
// An idle session must never put its old figures back over a newer reading,
// while the session that owns the file keeps it fresh between replies.
func TestStatuslineTapIgnoresIdleSessions(t *testing.T) {
	if _, err := os.Stat("/usr/bin/jq"); err != nil {
		t.Skip("statusline requires macOS /usr/bin/jq")
	}
	inst := t.TempDir()
	os.MkdirAll(filepath.Join(inst, "state"), 0o755)
	os.MkdirAll(filepath.Join(inst, "bin"), 0o755)
	data, err := os.ReadFile(filepath.Join("..", "..", "bin", "statusline-tap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tap := filepath.Join(inst, "bin", "statusline-tap.sh")
	if err := os.WriteFile(tap, data, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p, err := profiles.New(inst).Save(profiles.Profile{Provider: profiles.Claude, Name: "Work", Directory: dir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	latest := filepath.Join(inst, "state", "statusline-latest."+p.ID+".json")

	// render runs one statusline render of a session that holds pct and has
	// spent apiMs waiting on the API so far. An empty session sends no id.
	render := func(session string, pct, apiMs int) {
		t.Helper()
		id := ""
		if session != "" {
			id = fmt.Sprintf(`"session_id":%q,`, session)
		}
		cmd := exec.Command("/bin/bash", tap)
		cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+dir)
		cmd.Stdin = strings.NewReader(fmt.Sprintf(`{%s"rate_limits":{"seven_day":{"used_percentage":%d,"resets_at":1795514400}},"cost":{"total_api_duration_ms":%d}}`, id, pct, apiMs))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("tap: %s %v", out, err)
		}
	}
	type published struct {
		TsMs       int64   `json:"ts_ms"`
		Session    *string `json:"session_id"`
		RateLimits struct {
			SevenDay struct {
				Used int `json:"used_percentage"`
			} `json:"seven_day"`
		} `json:"rate_limits"`
	}
	read := func() (published, bool) {
		t.Helper()
		var got published
		raw, err := os.ReadFile(latest)
		if os.IsNotExist(err) {
			return got, false
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("tap file %s: %v", raw, err)
		}
		return got, true
	}
	expect := func(step, session string, pct int) published {
		t.Helper()
		got, ok := read()
		owner := ""
		if ok && got.Session != nil {
			owner = *got.Session
		}
		if !ok || owner != session || got.RateLimits.SevenDay.Used != pct {
			t.Fatalf("%s: want %d%% from %q, tap file holds %d%% from %q (present %v)", step, pct, session, got.RateLimits.SevenDay.Used, owner, ok)
		}
		return got
	}

	// A first sighting cannot say how old its figures are, so it only records.
	render("active", 20, 1000)
	if got, ok := read(); ok {
		t.Fatalf("a first sighting published %d%%", got.RateLimits.SevenDay.Used)
	}
	// Hearing from the API again is what publishes.
	render("active", 20, 2000)
	expect("active session replied", "active", 20)

	// The incident: a session idle since before an early reset still holds
	// 98%. Neither its first render nor any later one may publish that.
	render("idle", 98, 500)
	render("idle", 98, 500)
	before := expect("idle session rendered", "active", 20)

	// The owner's renders keep its file fresh between replies.
	time.Sleep(20 * time.Millisecond)
	render("active", 20, 2000)
	if after := expect("owner rendered again", "active", 20); after.TsMs <= before.TsMs {
		t.Fatalf("owner render did not refresh the file: ts_ms %d then %d", before.TsMs, after.TsMs)
	}

	// A session that hears from the API takes the file over, and the previous
	// owner, rendering figures it heard earlier, does not take it back.
	render("idle", 21, 600)
	expect("idle session replied", "idle", 21)
	render("active", 20, 2000)
	expect("previous owner rendered", "idle", 21)

	// Without a session id renders cannot be told apart, so they publish as before.
	render("", 22, 0)
	expect("render with no session id", "", 22)
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
