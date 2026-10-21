package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

// validatorRejecting answers like ValidateClaudeBin would: everything passes
// except the paths named. The real validator cannot be used here, since it
// refuses any path under TMPDIR as a temporary shim and every path in a test
// is under TMPDIR.
func validatorRejecting(bad ...string) func(string) error {
	set := map[string]bool{}
	for _, b := range bad {
		set[b] = true
	}
	return func(p string) error {
		if set[p] {
			return fmt.Errorf("not usable: %s", p)
		}
		return nil
	}
}

func stubLookPath(f func(string) (string, error)) func() {
	prev := lookPath
	lookPath = f
	return func() { lookPath = prev }
}

// TestDetectClaudePrefersCanonicalOverPath is the regression guard for a live
// outage on 2026-09-21: a wrapper script earlier on PATH than ~/.local/bin was
// recorded as CLAUDE_BIN by an upgrade, and it then refused every usage probe
// the poller made, freezing the billed figure with no error anywhere.
//
// Such a wrapper is indistinguishable from the real binary to
// ValidateClaudeBin, since it is absolute, executable, and passes --version
// straight through. Preferring the canonical location is the only rule that
// holds without knowing what else is on PATH.
func TestDetectClaudePrefersCanonicalOverPath(t *testing.T) {
	home := "/home/tester"
	canonical := filepath.Join(home, ".local", "bin", "claude")
	shim := "/opt/wrappers/claude"

	defer stubLookPath(func(string) (string, error) { return shim, nil })()

	got, err := detectClaudeWith(home, validatorRejecting())
	if err != nil {
		t.Fatalf("detectClaudeWith: %v", err)
	}
	if got != canonical {
		t.Errorf("took the PATH entry over the canonical binary:\n got  %s\n want %s", got, canonical)
	}
}

// TestDetectClaudeFallsBackToPath keeps the preference from becoming a
// hardcoding: a claude installed somewhere else entirely must still be found.
func TestDetectClaudeFallsBackToPath(t *testing.T) {
	home := "/home/tester"
	canonical := filepath.Join(home, ".local", "bin", "claude")
	elsewhere := "/opt/homebrew/bin/claude"

	defer stubLookPath(func(string) (string, error) { return elsewhere, nil })()

	got, err := detectClaudeWith(home, validatorRejecting(canonical))
	if err != nil {
		t.Fatalf("detectClaudeWith: %v", err)
	}
	if got != elsewhere {
		t.Errorf("did not fall back to PATH:\n got  %s\n want %s", got, elsewhere)
	}
}

// TestDetectClaudeReportsWhenNothingValidates: with no canonical binary and
// nothing on PATH, the caller still gets the canonical path back plus the
// error, which is what the install flow prints.
func TestDetectClaudeReportsWhenNothingValidates(t *testing.T) {
	home := "/home/tester"
	canonical := filepath.Join(home, ".local", "bin", "claude")

	defer stubLookPath(func(string) (string, error) { return "", fmt.Errorf("not found") })()

	got, err := detectClaudeWith(home, validatorRejecting(canonical))
	if err == nil {
		t.Fatal("expected an error when nothing validates")
	}
	if got != canonical {
		t.Errorf("want the canonical path reported back, got %s", got)
	}
}
