package report

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var ErrExecution = errors.New("Claude execution was blocked")
var blockedHook = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]* operation blocked by hook:`)

// ExecutionError recognises CLI diagnostics that can arrive with exit code
// zero. In particular, hook rejection output echoes the original prompt,
// including its example report JSON. That is not a generated report.
func ExecutionError(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if !blockedHook.MatchString(s) {
		return nil
	}
	if i := strings.Index(s, "\nOriginal prompt:"); i >= 0 {
		s = s[:i]
	}
	runes := []rune(strings.TrimSpace(s))
	if len(runes) > 400 {
		runes = append(runes[:400], []rune("...")...)
	}
	return fmt.Errorf("%w: %s", ErrExecution, string(runes))
}
