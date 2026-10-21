package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestUpgradeGuardRejectsOlderCheckout(t *testing.T) {
	script, err := filepath.Abs("../../bin/check-upgrade.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return string(out)
	}
	git("init", "-q")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "older")
	old := git("rev-parse", "HEAD")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "installed")
	install := filepath.Join(dir, "install")
	os.MkdirAll(filepath.Join(install, "bin"), 0700)
	os.WriteFile(filepath.Join(install, "bin", "claude-obs"), []byte("#!/bin/sh\nprintf '%s' '"+git("rev-parse", "HEAD")+"'\n"), 0700)
	check := func() error { c := exec.Command("bash", script, install); c.Dir = dir; return c.Run() }
	if err := check(); err != nil {
		t.Fatal("matching revision refused:", err)
	}
	git("checkout", "--detach", old[:len(old)-1])
	if err := check(); err == nil {
		t.Fatal("older checkout may overwrite the installed UI")
	}
}
