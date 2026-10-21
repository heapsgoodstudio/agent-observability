package installer

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRenderPlistAllAgents(t *testing.T) {
	vars := map[string]string{
		"OBS_DIR":      "/Users/x/.agent-observability",
		"LABEL_PREFIX": "com.agent-observability",
		"CLAUDE_OBS":   "/Users/x/.agent-observability/bin/claude-obs",
	}
	for _, agent := range Agents {
		out, err := RenderPlist(agent, vars)
		if err != nil {
			t.Fatalf("%s: %v", agent, err)
		}
		s := string(out)
		if strings.Contains(s, "{{") {
			t.Fatalf("%s: unresolved placeholder in %s", agent, s)
		}
		if !strings.Contains(s, "com.agent-observability."+agent) {
			t.Fatalf("%s: label missing", agent)
		}
		if runtime.GOOS == "darwin" {
			cmd := exec.Command("plutil", "-lint", "-")
			cmd.Stdin = strings.NewReader(s)
			if lint, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: plutil: %s", agent, lint)
			}
		}
	}
}

func TestRenderPlistUnknownAgent(t *testing.T) {
	if _, err := RenderPlist("nonsense", nil); err == nil {
		t.Fatal("want error")
	}
}

// launchdFake simulates launchd's asynchronous bootout teardown: `list`
// reports the label loaded for gonAfter calls, and `bootstrap` fails with a
// transient error for failFirst attempts (the EIO 5 a real launchd returns
// while the old instance drains).
type launchdFake struct {
	listCalls, bootstraps int
	goneAfter, failFirst  int
	bootstrapBeforeGone   bool
}

func (f *launchdFake) Run(name string, args ...string) (string, error) {
	if name != "launchctl" {
		return "", nil
	}
	switch args[0] {
	case "bootout":
		return "", nil
	case "list":
		f.listCalls++
		if f.listCalls > f.goneAfter {
			return "", exec.ErrNotFound
		}
		return "loaded", nil
	case "bootstrap":
		if f.listCalls <= f.goneAfter {
			f.bootstrapBeforeGone = true
		}
		f.bootstraps++
		if f.bootstraps <= f.failFirst {
			return "Bootstrap failed: 5: Input/output error", exec.ErrNotFound
		}
		return "", nil
	}
	return "", nil
}

func TestBootstrapFreshWaitsForTeardownAndRetries(t *testing.T) {
	oldSleep := launchdSleep
	launchdSleep = func(time.Duration) {}
	defer func() { launchdSleep = oldSleep }()

	f := &launchdFake{goneAfter: 3, failFirst: 1}
	if err := BootstrapFresh(f, 501, "com.agent-observability.victoriametrics", "/tmp/x.plist"); err != nil {
		t.Fatalf("BootstrapFresh: %v", err)
	}
	if f.bootstrapBeforeGone {
		t.Fatal("bootstrap attempted while launchd still reported the label loaded")
	}
	if f.bootstraps != 2 {
		t.Fatalf("bootstraps = %d, want 2 (one transient failure, one success)", f.bootstraps)
	}
}

func TestBootstrapFreshGivesUpWithTheRealError(t *testing.T) {
	oldSleep := launchdSleep
	launchdSleep = func(time.Duration) {}
	defer func() { launchdSleep = oldSleep }()

	f := &launchdFake{goneAfter: 0, failFirst: 99}
	err := BootstrapFresh(f, 501, "com.agent-observability.server", "/tmp/x.plist")
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Fatalf("want the real bootstrap error surfaced, got %v", err)
	}
}

// TestPollerPlistPinsAWorkingDirectory guards a live failure mode.
//
// launchd gives a job with no WorkingDirectory the root directory. That is
// invisible until something the poller spawns cares where it was started
// from, and on 2026-09-21 something did: a PATH wrapper around the claude
// binary refused every usage probe because "/" was out of scope for the
// profile, and the billed figure silently stopped refreshing.
//
// The install dir is the right answer rather than merely a safe one: it is
// this stack's own directory, it needs no macOS permission to enter, and it is
// already what cmdServe gives spawned analysis runs for a related reason.
func TestPollerPlistPinsAWorkingDirectory(t *testing.T) {
	body, err := RenderPlist("limits-poller", map[string]string{
		"OBS_DIR":      "/install/root",
		"LABEL_PREFIX": LabelPrefix,
		"CLAUDE_OBS":   "/install/root/bin/claude-obs",
	})
	if err != nil {
		t.Fatalf("RenderPlist: %v", err)
	}
	want := "<key>WorkingDirectory</key><string>/install/root</string>"
	if !strings.Contains(string(body), want) {
		t.Errorf("limits-poller plist has no WorkingDirectory; launchd would use /\n%s", body)
	}
}
