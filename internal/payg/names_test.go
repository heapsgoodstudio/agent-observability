package payg

import "testing"

func TestCanonicalProvider(t *testing.T) {
	cases := map[string]string{
		"fireworks-ai": "fireworks",
		"Fireworks-AI": "fireworks",
		" baseten ":    "baseten",
		"lm-studio":    "lm-studio",
		"Anthropic":    "anthropic",
	}
	for in, want := range cases {
		if got := CanonicalProvider(in); got != want {
			t.Errorf("CanonicalProvider(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalModel(t *testing.T) {
	cases := map[string]string{
		"zai-org/GLM-5.3-Flash":                   "glm-5.3-flash",
		"accounts/fireworks/models/glm-5p3-flash": "glm-5.3-flash",
		"glm-5.3-flash":                           "glm-5.3-flash",
		"qwen/qwen3.8-27b":                        "qwen3.8-27b",
		"deepseek-ai/DeepSeek-V4-Pro":             "deepseek-v4-pro",
	}
	for in, want := range cases {
		if got := CanonicalModel(in); got != want {
			t.Errorf("CanonicalModel(%q) = %q, want %q", in, got, want)
		}
	}
}
