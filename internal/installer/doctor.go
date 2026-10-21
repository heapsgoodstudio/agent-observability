package installer

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/config"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

// Check is one doctor finding. Every check is OK/FAIL except "notifier",
// which is warn-level: a missing or non-executable notifier binary still
// leaves the osascript fallback for alerts, so the caller renders it as a
// warning rather than a hard failure.
type Check struct {
	Name, Detail string
	OK           bool
}

// ConfiguredProfiles reads the profile list out of a loaded config.env,
// falling back to PROFILE_DIR alone. It is the one place that fallback is
// spelled on the Go side, matching bin/limits-poller.sh's
// "${PROFILE_DIRS:-$PROFILE_DIR}".
func ConfiguredProfiles(cfg *config.Config) []string {
	primary := cfg.Get("PROFILE_DIR", "")
	list, present := cfg.Lookup("PROFILE_DIRS")
	if !present {
		if primary == "" {
			return nil
		}
		return []string{primary}
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Split(list, ":") {
		p = strings.TrimRight(strings.TrimSpace(p), "/")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func Doctor(x Exec, cfg *config.Config) []Check {
	var checks []Check
	add := func(name string, ok bool, detail string) {
		checks = append(checks, Check{Name: name, OK: ok, Detail: detail})
	}

	for _, a := range Agents {
		label := LabelPrefix + "." + a
		add("agent "+a, Loaded(x, label), label)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	for name, url := range map[string]string{
		"server":          "http://127.0.0.1:8427/",
		"victoriametrics": "http://127.0.0.1:8428/health",
		"victorialogs":    "http://127.0.0.1:9428/health",
	} {
		resp, err := client.Get(url)
		ok := err == nil && resp.StatusCode < 400
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		add("endpoint "+name, ok, url)
	}

	roster, err := profiles.New(cfg.Dir).List()
	if err != nil {
		add("profile registry", false, err.Error())
	} else if len(roster) == 0 {
		add("profiles", true, "No profiles configured. Add one in dashboard Settings.")
	}
	for _, p := range roster {
		if !p.Enabled {
			add("profile "+p.Name, true, "disabled; history retained")
			continue
		}
		detail := p.Directory
		if p.WiringError != "" {
			detail += ": " + p.WiringError
		}
		add("profile wiring "+p.Name, p.WiringStatus == "connected", detail)
	}

	notifier := filepath.Join(cfg.Get("NOTIFIER_APP", ""), "Contents", "MacOS", "AgentObsNotifier")
	fi, err := os.Stat(notifier)
	add("notifier", err == nil && fi.Mode()&0o111 != 0, notifier+" (osascript fallback covers a miss)")

	dbPath := cfg.Path("state", "claude-obs.db")
	d, err := db.Open(dbPath)
	if err == nil {
		n, merr := db.Migrate(d)
		add("sqlite", merr == nil && n == 0, dbPath)
		d.Close()
	} else {
		add("sqlite", false, dbPath)
	}
	return checks
}
