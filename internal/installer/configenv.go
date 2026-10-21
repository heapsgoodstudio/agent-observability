package installer

import (
	"fmt"
	"strings"
)

// machineOwned are the keys claude-obs install writes itself, in the
// machine-local block appended after the example. Carrying an old value over
// would fight the block that is about to be written, so they are never
// preserved. OBS_DIR joins them because the example computes it with a
// $(...) expression that must stay an expression.
var machineOwned = map[string]bool{
	"OBS_DIR": true, "PROFILE_DIR": true, "PROFILE_DIRS": true, "PROFILE_IDS": true, "PROFILES_REGISTRY": true,
	"CLAUDE_BIN": true, "LABEL_PREFIX": true,
}

// renamedDefaults are shipped values whose old spelling should move forward
// automatically. Without this exception, MergeConfigEnv would correctly treat
// the old notifier path as an operator override and keep installing the newly
// branded app under its previous name forever. Exact whole-line matching keeps
// a genuinely custom path under the operator's control.
var renamedDefaults = map[string]map[string]string{
	"NOTIFIER_APP": {
		`NOTIFIER_APP="$HOME/Applications/Claude Observability.app"`: `NOTIFIER_APP="$HOME/Applications/Agent Observability.app"`,
	},
}

// MergeConfigEnv renders a config.env body from the shipped example, carrying
// over whatever an existing install already had.
//
// config.env is generated, but it is also the only place an operator can tune
// alert thresholds and budgets, and rebuilding it from the example on every
// run silently reverted those on upgrade. So: a key the example ships keeps
// the value the install already has, a key the example has gained since is
// added at its default, and a key the operator added that the example never
// mentions is carried to the end rather than dropped.
//
// Preservation is decided by comparing whole lines, not parsed values. That
// keeps `$HOME/Applications/...` from being rewritten to an expanded absolute
// path on the first upgrade just because nothing about it actually changed.
//
// It cannot tell an operator's edit from a default we changed upstream, since
// nothing records which is which; both look like "differs from the example".
// The operator's value wins, and every such key comes back in kept so the
// caller can say so out loud instead of shipping a silent divergence.
func MergeConfigEnv(example, existing []byte) (out []byte, kept []string, extra []string) {
	prev, order := envLines(existing)

	var b strings.Builder
	seen := map[string]bool{}
	for _, line := range strings.Split(string(example), "\n") {
		key := envKey(line)
		if key == "" || machineOwned[key] {
			b.WriteString(line)
			b.WriteString("\n")
			continue
		}
		seen[key] = true
		if had, ok := prev[key]; ok && had != line {
			if replacements := renamedDefaults[key]; replacements != nil && replacements[had] == line {
				b.WriteString(line)
				b.WriteString("\n")
				continue
			}
			b.WriteString(had)
			b.WriteString("\n")
			kept = append(kept, fmt.Sprintf("%s=%s (shipped default is now %s)",
				key, envValue(had), envValue(line)))
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	// Keys only this machine has. Dropping them would quietly disable a
	// deliberate override (bin/limits-poller.sh reads several that the
	// example never lists, such as POLLER_TAP_FILE).
	var tail []string
	for _, key := range order {
		if seen[key] || machineOwned[key] {
			continue
		}
		tail = append(tail, prev[key])
		extra = append(extra, key)
	}
	body := strings.TrimRight(b.String(), "\n") + "\n"
	if len(tail) > 0 {
		body += "\n# Kept from the previous config.env: not in config.env.example.\n" +
			strings.Join(tail, "\n") + "\n"
	}
	return []byte(body), kept, extra
}

// envLines maps each assignment to its last occurrence, matching bash and
// config.Load: config.env is the example plus an appended machine block, so
// keys legitimately appear twice and the later one is the live value. order
// lists the keys once each, in the order they first appear.
func envLines(data []byte) (map[string]string, []string) {
	out := map[string]string{}
	var order []string
	for _, line := range strings.Split(string(data), "\n") {
		key := envKey(line)
		if key == "" {
			continue
		}
		if _, ok := out[key]; !ok {
			order = append(order, key)
		}
		out[key] = line
	}
	return out, order
}

// envKey returns the KEY of a `KEY=value` line, or "" for anything else:
// blanks, comments, and indented or lowercase text that bash would not treat
// as a plain assignment at the top level of this file.
func envKey(line string) string {
	if line != strings.TrimSpace(line) {
		return ""
	}
	if line == "" || strings.HasPrefix(line, "#") {
		return ""
	}
	i := strings.Index(line, "=")
	if i <= 0 {
		return ""
	}
	for _, r := range line[:i] {
		if r != '_' && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return ""
		}
	}
	return line[:i]
}

// envValue is the assignment's value for logging, with the trailing inline
// comment dropped the same way config.Load drops it.
func envValue(line string) string {
	_, val, _ := strings.Cut(line, "=")
	if i := strings.Index(val, " #"); i >= 0 {
		val = val[:i]
	}
	return strings.TrimSpace(val)
}
