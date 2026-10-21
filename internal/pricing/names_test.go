package pricing

import "testing"

func TestShortModel(t *testing.T) {
	for in, want := range map[string]string{
		"claude-haiku-4-5-20251001": "haiku-4-5",
		"claude-opus-5[1m]":         "opus-5",
		"claude-opus-5":             "opus-5",
		"":                          "unknown",
	} {
		if got := ShortModel(in); got != want {
			t.Errorf("ShortModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortProject(t *testing.T) {
	if got := ShortProject("-Users-alice-Projects-app"); got != "Projects-app" {
		t.Errorf("got %q", got)
	}
	if got := ShortProject("plain"); got != "plain" {
		t.Errorf("got %q", got)
	}
}

func TestNormServer(t *testing.T) {
	if NormServer("plugin:linear:linear") != NormServer("plugin_linear_linear") {
		t.Error("linear spellings do not fold")
	}
	if NormServer("claude.ai Gmail") != NormServer("claude_ai_Gmail") {
		t.Error("gmail spellings do not fold")
	}
}
