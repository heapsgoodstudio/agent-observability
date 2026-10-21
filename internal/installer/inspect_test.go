package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeExec struct{ out map[string]string }

func (f fakeExec) Run(name string, args ...string) (string, error) {
	key := name
	for _, a := range args {
		key += " " + a
	}
	if out, ok := f.out[key]; ok {
		return out, nil
	}
	return "", os.ErrNotExist
}

func TestDiscoverProfiles(t *testing.T) {
	home := t.TempDir()
	// .claude-observability is the legacy install dir: it matches the
	// ~/.claude-* glob but is the telemetry stack itself, never a profile.
	for _, d := range []string{".claude", ".claude-acme", ".claude-work", ".claude-observability", ".agent-observability", "other"} {
		os.MkdirAll(filepath.Join(home, d), 0o755)
	}
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte("{}"), 0o644)

	got := DiscoverProfiles(home, filepath.Join(home, ".claude-acme"), filepath.Join(home, ".agent-observability"))
	want := []string{
		filepath.Join(home, ".claude"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestValidateClaudeBin(t *testing.T) {
	// t.TempDir() lives under /var/folders on macOS, which the real shim
	// prefixes reject; override them so the good-binary case is testable
	// and the shim branch is exercised explicitly.
	old := shimPrefixes
	shimPrefixes = []string{"/var/shim/"}
	defer func() { shimPrefixes = old }()

	dir := t.TempDir()
	good := filepath.Join(dir, "claude")
	os.WriteFile(good, []byte("#!/bin/sh\necho 1.0\n"), 0o755)
	ex := fakeExec{out: map[string]string{good + " --version": "1.0"}}

	if err := ValidateClaudeBin(good, ex); err != nil {
		t.Fatalf("good bin rejected: %v", err)
	}
	shimErr := ValidateClaudeBin("/var/shim/x/claude", ex)
	if shimErr == nil || !strings.Contains(shimErr.Error(), "temporary shim") {
		t.Fatalf("shim path not rejected as shim: %v", shimErr)
	}
	for _, bad := range []string{
		"relative/claude",
		filepath.Join(dir, "missing"),
		"/tmp/has;semicolon",
	} {
		if err := ValidateClaudeBin(bad, ex); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestValidateSafePath(t *testing.T) {
	if err := ValidateSafePath("profile dir", "/Users/x/.claude"); err != nil {
		t.Fatalf("safe path rejected: %v", err)
	}
	for _, bad := range []string{"relative/path", "/Users/x/$(evil)", "/Users/x/a;b", "/Users/x/`tick`"} {
		if err := ValidateSafePath("profile dir", bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDefaultShimPrefixes(t *testing.T) {
	t.Setenv("TMPDIR", "/private/tmp/probe/")
	p := defaultShimPrefixes()
	if len(p) != 2 || p[0] != "/var/folders/" || p[1] != "/private/tmp/probe/" {
		t.Fatalf("defaultShimPrefixes = %v", p)
	}
}
