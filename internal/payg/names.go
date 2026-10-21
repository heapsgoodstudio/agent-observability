package payg

import (
	"regexp"
	"strings"
)

// accountsPrefixRe strips a Fireworks-style "accounts/<anything>/models/"
// prefix before a model is reduced to its final path segment.
var accountsPrefixRe = regexp.MustCompile(`^accounts/[^/]+/models/`)

// pDigitRe recovers a decimal point providers sometimes replace with "p" to
// keep a model id URL/path safe, e.g. "glm-5p3-flash" -> "glm-5.3-flash".
var pDigitRe = regexp.MustCompile(`(\d)p(\d)`)

// CanonicalProvider normalizes a provider id to the spelling every harness's
// buckets and model costs are keyed on. It is lowercased and trimmed; the one
// provider whose harness spelling and billed name differ, "fireworks-ai" (Oh
// My Pi and opencode both use it), is mapped to "fireworks". Every other
// provider passes through unchanged.
func CanonicalProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "fireworks-ai" {
		return "fireworks"
	}
	return p
}

// CanonicalModel normalizes a model id to the spelling every harness's
// buckets and model costs are keyed on: lowercased and trimmed, with any
// "accounts/<anything>/models/" prefix stripped, reduced to the segment
// after the last "/" (dropping an org/namespace prefix such as "zai-org/"
// or "qwen/"), and with a "p" standing in for a decimal point between two
// digits restored to ".".
func CanonicalModel(m string) string {
	s := strings.ToLower(strings.TrimSpace(m))
	s = accountsPrefixRe.ReplaceAllString(s, "")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return pDigitRe.ReplaceAllString(s, "$1.$2")
}
