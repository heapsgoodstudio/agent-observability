package pricing

import (
	"regexp"
	"strings"
)

// ShortModel returns a canonicalized model name by stripping the "claude-" prefix,
// the trailing date (YYYYMMDD), and any context suffix like [1m].
// An empty result becomes "unknown".
func ShortModel(name string) string {
	s := strings.TrimPrefix(name, "claude-")
	s = regexp.MustCompile(`-\d{8}$`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`\[\d+[a-z]\]$`).ReplaceAllString(s, "")
	if s == "" {
		return "unknown"
	}
	return s
}

// ShortProject returns a canonicalized project path by stripping the
// "-Users-<username>-" prefix from macOS paths. If the path doesn't have
// this prefix, it returns the original input.
func ShortProject(name string) string {
	s := regexp.MustCompile(`^-Users-[^-]+-`).ReplaceAllString(name, "")
	if s == "" {
		return name
	}
	return s
}

// NormServer returns a canonicalized MCP server name by converting to lowercase
// and collapsing all runs of non-alphanumeric characters into underscores.
// This folds different spellings like "plugin:linear:linear" and
// "plugin_linear_linear" onto the same key.
func NormServer(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "_")
	return s
}
