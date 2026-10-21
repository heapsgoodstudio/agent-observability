package installer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProfileName(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/x/.claude":      "default",
		"/Users/x/.claude-acme": "acme",
		"/Users/x/custom":       "custom",
	} {
		if got := ProfileName(in); got != want {
			t.Errorf("ProfileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTelemetryEnvExactKeys(t *testing.T) {
	env := TelemetryEnv("acme")
	if len(env) != 10 {
		t.Fatalf("len = %d, want exactly 10", len(env))
	}
	if env["OTEL_RESOURCE_ATTRIBUTES"] != "profile=acme" {
		t.Fatal(env["OTEL_RESOURCE_ATTRIBUTES"])
	}
	if env["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"] != "http://127.0.0.1:8428/opentelemetry/v1/metrics" {
		t.Fatal(env["OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"])
	}
	if env["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"] != "http://127.0.0.1:9428/insert/opentelemetry/v1/logs" {
		t.Fatal(env["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"])
	}
}

func env(t *testing.T, out []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	e, _ := m["env"].(map[string]any)
	return e
}

func TestMergeTelemetry(t *testing.T) {
	desired := TelemetryEnv("default")

	out, changed, conflicts, err := MergeTelemetry([]byte(`{"model":"opus"}`), desired)
	if err != nil || !changed || len(conflicts) != 0 {
		t.Fatalf("fresh merge: changed=%v conflicts=%v err=%v", changed, conflicts, err)
	}
	if env(t, out)["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Fatal("key not merged")
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["model"] != "opus" {
		t.Fatal("unrelated key lost")
	}

	_, changed, _, _ = MergeTelemetry(out, desired)
	if changed {
		t.Fatal("identical merge reported change")
	}

	conflicted := []byte(`{"env":{"OTEL_METRICS_EXPORTER":"prometheus"}}`)
	_, _, conflicts, _ = MergeTelemetry(conflicted, desired)
	if len(conflicts) != 1 || conflicts[0] != "OTEL_METRICS_EXPORTER" {
		t.Fatalf("conflicts = %v", conflicts)
	}
}

func TestRemoveTelemetry(t *testing.T) {
	merged, _, _, _ := MergeTelemetry([]byte(`{"env":{"KEEP":"me"}}`), TelemetryEnv("default"))
	out, err := RemoveTelemetry(merged)
	if err != nil {
		t.Fatal(err)
	}
	e := env(t, out)
	if len(e) != 1 || e["KEEP"] != "me" {
		t.Fatalf("env after removal = %v", e)
	}
	merged, _, _, _ = MergeTelemetry([]byte(`{}`), TelemetryEnv("default"))
	out, _ = RemoveTelemetry(merged)
	if strings.Contains(string(out), `"env"`) {
		t.Fatal("empty env not deleted")
	}
}

func TestStatusline(t *testing.T) {
	out, existing, err := SetStatusline([]byte(`{}`), "/inst/bin/statusline.sh")
	if err != nil || existing != "" {
		t.Fatalf("existing=%q err=%v", existing, err)
	}
	_, existing, _ = SetStatusline(out, "/other/path.sh")
	if existing != "/inst/bin/statusline.sh" {
		t.Fatalf("existing = %q", existing)
	}

	removed, ok, _ := RemoveStatusline(out, []string{"/inst/bin/statusline.sh"})
	if !ok || strings.Contains(string(removed), "statusLine") {
		t.Fatal("ours not removed")
	}
	_, ok, _ = RemoveStatusline(out, []string{"/not/ours.sh"})
	if ok {
		t.Fatal("removed a statusline that is not ours")
	}
}
