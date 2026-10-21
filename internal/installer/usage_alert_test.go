package installer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type alertSeries struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}

// sampleValue formats a value the way VictoriaMetrics' query API does: plain
// decimal, never exponent notation. fmt.Sprint would write a reset ending in 0
// as 1.79020455e+09, which no real store sends.
func sampleValue(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func alertSample(account, profile, source, window string, value float64) alertSeries {
	return alertSeries{map[string]string{"account": account, "profile": profile, "source": source, "window": window}, []any{time.Now().Unix(), sampleValue(value)}}
}

type alertFixture struct {
	root, script string
	responses    map[string][]alertSeries
}

const alertObserved = "timestamp(claude_plan_data_age_seconds) - claude_plan_data_age_seconds"
const alertBurn = `clamp_min(delta(claude_extra_usage_usd{account!=""}[1h]),0)`

func newAlertFixture(t *testing.T) *alertFixture {
	t.Helper()
	for _, command := range []string{"bash", "jq", "curl", "awk"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skip(command + " is required for shell alert integration tests")
		}
	}
	f := &alertFixture{root: t.TempDir(), responses: map[string][]alertSeries{}}
	f.script = filepath.Join(f.root, "bin", "usage-alert.sh")
	os.MkdirAll(filepath.Dir(f.script), 0o755)
	os.MkdirAll(filepath.Join(f.root, "state"), 0o755)
	data, err := os.ReadFile(filepath.Join("..", "..", "bin", "usage-alert.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.script, data, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"result": f.responses[r.Form.Get("query")]}})
	}))
	t.Cleanup(srv.Close)
	config := fmt.Sprintf("OBS_DIR=%q\nVM_URL=%q\nDASH_PORT=8427\nDRY_RUN=1\nWARN_PCTS=\"75 90 100\"\nOVERAGE_WARN_PER_HR=100\nOVERAGE_CRIT_PER_HR=250\nUSAGE_STALE_ALERT_S=21600\n", f.root, srv.URL)
	os.WriteFile(filepath.Join(f.root, "config.env"), []byte(config), 0o600)
	os.WriteFile(filepath.Join(f.root, "profiles.json"), []byte(`{"profiles":[{"id":"work","provider":"claude","name":"Work"},{"id":"personal","provider":"claude","name":"Personal"},{"id":"shared","provider":"claude","name":"Shared"}]}`), 0o600)
	return f
}

func (f *alertFixture) run(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("bash", f.script).CombinedOutput()
	if err != nil {
		t.Fatalf("usage-alert failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "jq:") {
		t.Fatalf("invalid alert query: %s", out)
	}
	return string(out)
}

func TestUsageAlertsNameScopeAndDeduplicateEachAccount(t *testing.T) {
	f := newAlertFixture(t)
	now := float64(time.Now().Unix())
	reset := now + 86400
	f.responses[alertObserved] = []alertSeries{
		alertSample("work-account", "work", "cache", "", now),
		alertSample("personal-account", "personal", "cache", "", now),
	}
	f.responses["claude_plan_window_pct"] = []alertSeries{
		alertSample("work-account", "work", "cache", "weekly_scoped", 78),
		alertSample("personal-account", "personal", "cache", "weekly_scoped", 82),
		alertSample("", "", "cache", "weekly_scoped", 100),
		alertSample("unknown", "work", "cache", "weekly_scoped", 100),
		alertSample("unknown", "personal", "cache", "weekly_scoped", 100),
	}
	f.responses["claude_plan_window_reset_timestamp_seconds"] = []alertSeries{
		alertSample("work-account", "work", "cache", "weekly_scoped", reset),
		alertSample("personal-account", "personal", "cache", "weekly_scoped", reset),
	}
	out := f.run(t)
	for _, expected := range []string{"Claude · Work: 78% used", "Claude · Personal: 82% used", "Model weekly allowance crossed 75%", "accountKey=claude%3Awork-account#now", "accountKey=claude%3Apersonal-account#now"} {
		if !strings.Contains(out, expected) {
			t.Fatalf("missing account context %q: %s", expected, out)
		}
	}
	if strings.Count(out, "ALERT [") != 2 || strings.Contains(out, "weekly_scoped") {
		t.Fatalf("duplicate/unattributed alert or raw window label: %s", out)
	}
	// Same reset wobble, separately suppressed for both accounts.
	f.responses["claude_plan_window_reset_timestamp_seconds"][0].Value[1] = sampleValue(reset + 1)
	if out = f.run(t); strings.Contains(out, "ALERT [") {
		t.Fatalf("repeated account/reset alert: %s", out)
	}
	// Missing reset metadata must not re-arm warnings or overwrite markers.
	resets := f.responses["claude_plan_window_reset_timestamp_seconds"]
	delete(f.responses, "claude_plan_window_reset_timestamp_seconds")
	if out = f.run(t); strings.Contains(out, "ALERT [") {
		t.Fatalf("missing reset re-armed warning: %s", out)
	}
	f.responses["claude_plan_window_reset_timestamp_seconds"] = resets
	if out = f.run(t); strings.Contains(out, "ALERT [") {
		t.Fatalf("reset recovery repeated warning: %s", out)
	}
	// One account resetting cannot re-arm the other.
	f.responses["claude_plan_window_reset_timestamp_seconds"][0].Value[1] = sampleValue(reset + 604800)
	out = f.run(t)
	if strings.Count(out, "ALERT [") != 1 || !strings.Contains(out, "Claude · Work") {
		t.Fatalf("reset re-armed the wrong account: %s", out)
	}
}

func TestUsageAlertsKeepFreshnessWithinAccountProfileAndSource(t *testing.T) {
	f := newAlertFixture(t)
	now := float64(time.Now().Unix())
	f.responses[alertObserved] = []alertSeries{
		alertSample("same", "work", "cache", "", now),
		alertSample("same", "shared", "cache", "", now-2000),
		alertSample("same", "work", "statusline", "", now-3000),
		alertSample("other", "personal", "statusline", "", now),
	}
	f.responses["claude_plan_window_pct"] = []alertSeries{
		alertSample("same", "work", "cache", "five_hour", 5),
		alertSample("same", "shared", "cache", "five_hour", 95),
		alertSample("same", "work", "statusline", "five_hour", 99),
		alertSample("other", "personal", "statusline", "five_hour", 10),
	}
	if out := f.run(t); strings.Contains(out, "ALERT [") {
		t.Fatalf("stale or another account's source won: %s", out)
	}
	// Profiles sharing one account produce one warning, with unknown reset explicit.
	f.responses["claude_plan_window_pct"][0].Value[1] = "78"
	out := f.run(t)
	if strings.Count(out, "ALERT [") != 1 || !strings.Contains(out, "Shared / Work: 78% used") || !strings.Contains(out, "Reset time unavailable") {
		t.Fatalf("shared allowance duplicated or reset guessed: %s", out)
	}
}

func TestUsageAlertsScopeOverageAndStaleStateWithoutRearmingMissingData(t *testing.T) {
	f := newAlertFixture(t)
	now := float64(time.Now().Unix())
	f.responses[alertObserved] = []alertSeries{alertSample("a", "work", "cache", "", now-22000), alertSample("b", "personal", "cache", "", now)}
	f.responses["claude_plan_window_pct"] = []alertSeries{alertSample("a", "work", "cache", "five_hour", 100), alertSample("b", "personal", "cache", "five_hour", 0)}
	f.responses[alertBurn] = []alertSeries{alertSample("a", "work", "", "", 300), alertSample("b", "personal", "", "", 0)}
	out := f.run(t)
	if strings.Count(out, "ALERT [Claude · Work:") != 6 || strings.Contains(out, "ALERT [Claude · Personal") || !strings.Contains(out, "Provider-reported overage rose by $300") {
		t.Fatalf("overage/stale alerts crossed account scope: %s", out)
	}
	delete(f.responses, "claude_plan_window_pct")
	delete(f.responses, alertBurn)
	if out := f.run(t); strings.Contains(out, "ALERT [") {
		t.Fatalf("missing data fired an alert: %s", out)
	}
	f.responses["claude_plan_window_pct"] = []alertSeries{alertSample("a", "work", "cache", "five_hour", 100)}
	f.responses[alertBurn] = []alertSeries{alertSample("a", "work", "", "", 300)}
	if out := f.run(t); strings.Contains(out, "ALERT [") {
		t.Fatalf("missing data re-armed saved state: %s", out)
	}
}

func TestUsageAlertsUseFreshWindowForExhaustionAndEncodeAccountIdentity(t *testing.T) {
	f := newAlertFixture(t)
	now := float64(time.Now().Unix())
	account := "team/a +b"
	f.responses[alertObserved] = []alertSeries{
		alertSample(account, "work", "cache", "", now-30000),
		alertSample(account, "work", "statusline", "", now),
		alertSample(account, "shared", "cache", "", now-20000),
	}
	f.responses["claude_plan_window_pct"] = []alertSeries{
		alertSample(account, "work", "statusline", "five_hour", 100),
		alertSample(account, "shared", "cache", "five_hour", 0),
	}
	out := f.run(t)
	expected := "accountKey=" + url.QueryEscape("claude:"+url.QueryEscape(account))
	if !strings.Contains(out, "allowance exhausted") || !strings.Contains(out, expected+"#overage") {
		t.Fatalf("lost freshest exhaustion or account link: %s", out)
	}
}
