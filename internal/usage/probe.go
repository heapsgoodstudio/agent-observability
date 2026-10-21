// Package usage asks Claude Code for a profile's live plan-window usage.
//
// This is the same fetch the /usage panel makes and the same one
// bin/limits-poller.sh makes on its five minute cadence: a get_usage control
// request over the stream-json transport, answered by a headless process in
// about three seconds, with no model tokens and no transcript written.
//
// It exists as a package rather than as part of the poller because two callers
// now need it on two different cadences. The poller probes every profile every
// five minutes and cares about the durable effect (Claude Code rewrites
// cachedUsageUtilization, which every metric is then read from). GetUsageSnapshot
// probes on demand when a caller asks for a reading fresher than the store holds,
// and cares about the response itself, because it has to answer now rather than
// after the next poller run.
//
// WHAT MUST AGREE WITH bin/limits-poller.sh. The settings blob and the
// default-profile rule are the two things a divergence would break silently,
// and both are spelled out at their definitions below. This package does NOT
// reimplement the poller's throttle, its metric emission or its confirmation
// stamp: those belong to the poller, and a second writer of the same state is
// how two sources of truth start.
package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// probeRequest is the control request that makes Claude Code fetch
// GET /api/oauth/usage. Byte-identical to PROBE_REQUEST in
// bin/limits-poller.sh.
const probeRequest = `{"type":"control_request","request_id":"probe","request":{"subtype":"get_usage"}}`

// probeSettings suppresses this process's own telemetry, and it is required
// rather than decoration.
//
// Without it every probe counts itself in claude_code.session.count. At the
// poller's cadence alone that was measured at roughly 288 phantom sessions a
// day; an orchestrator probing before every launch wave adds more. The plain
// CLAUDE_CODE_ENABLE_TELEMETRY environment variable does not work here,
// because the profile's own settings.json wins over the environment, which is
// why this travels as --settings.
const probeSettings = `{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"0"}}`

// DefaultTimeout bounds one probe. The poller uses 30s for the same call.
const DefaultTimeout = 30 * time.Second

// Result is one profile's live usage, as Claude Code just reported it.
//
// Available is the field that distinguishes "this profile has no usage to
// report" from "the probe failed". Claude Code answers rate_limits_available
// false, with a null subscription, for a profile whose sign-in has lapsed:
// that is a successful probe with a real answer, and it must not read as an
// error or as zero usage.
type Result struct {
	Available bool
	Windows   map[string]Window
	// Subscription is "max", "team" or empty. Carried because it is the
	// clearest single signal that a profile is signed in and what to.
	Subscription string
}

// Window is one plan window as the API reports it. ResetTS is epoch seconds,
// parsed from the RFC 3339 resets_at, and HasReset is false when it was absent
// or unparseable: a zero here would be 1970 and would read as a window that
// reset long ago.
type Window struct {
	Pct      float64
	ResetTS  int64
	HasReset bool
}

// IsDefaultProfile reports whether dir is the default profile's directory.
//
// The default profile is the one case where CLAUDE_CONFIG_DIR must be ABSENT
// rather than set. Claude Code keeps its config at ~/.claude.json, in $HOME,
// and only moves it to <dir>/.claude.json when CLAUDE_CONFIG_DIR names <dir>.
// So exporting CLAUDE_CONFIG_DIR=$HOME/.claude does not describe the default
// profile, it creates a second one beside it, and a probe sent that way
// refreshes a cache nothing reads.
//
// Spelled in three places: here, is_default_profile in bin/limits-poller.sh,
// and analysis.IsDefaultProfile, which delegates to this one so the Go side
// has a single implementation.
func IsDefaultProfile(dir string) bool {
	home, err := os.UserHomeDir()
	if err != nil || dir == "" {
		return false
	}
	return filepath.Clean(dir) == filepath.Clean(filepath.Join(home, ".claude"))
}

// spawnEnv sets CLAUDE_CONFIG_DIR for a named profile and strips it for the
// default one. Stripping matters even when this process has none of its own:
// launchd gives the server whatever environment it inherited, and an
// orchestrator's shell may well export one.
func spawnEnv(base []string, profileDir string) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			continue
		}
		out = append(out, kv)
	}
	if !IsDefaultProfile(profileDir) {
		out = append(out, "CLAUDE_CONFIG_DIR="+profileDir)
	}
	return out
}

// Prober runs probes, at most one per profile at a time.
//
// The single flight is not an optimisation. A launch wave asks for every
// profile at once and the poller is probing on its own cadence, so without it
// one profile can have several probes in the air, each spawning a process and
// each racing to rewrite the same config file. Callers that arrive while a
// probe is running wait for it and share its answer, which is both cheaper and
// more consistent than starting another.
type Prober struct {
	// ClaudeBin is the claude binary. It must be the real one: a PATH
	// wrapper that refuses out-of-scope working directories is absolute,
	// executable and answers --version, so nothing can validate it away.
	// cmd/claude-obs/install.go's detectClaude records the canonical path
	// for this reason.
	ClaudeBin string
	Timeout   time.Duration

	mu     sync.Mutex
	inWork map[string]*flight
}

type flight struct {
	done chan struct{}
	res  Result
	err  error
}

// Probe fetches one profile's live usage. profileDir is the profile's config
// directory, the same string PROFILE_DIRS carries.
func (p *Prober) Probe(ctx context.Context, profileDir string) (Result, error) {
	if p.ClaudeBin == "" {
		return Result{}, fmt.Errorf("usage: no claude binary configured")
	}

	p.mu.Lock()
	if p.inWork == nil {
		p.inWork = map[string]*flight{}
	}
	if f, running := p.inWork[profileDir]; running {
		p.mu.Unlock()
		select {
		case <-f.done:
			return f.res, f.err
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	p.inWork[profileDir] = f
	p.mu.Unlock()

	f.res, f.err = p.run(ctx, profileDir)

	p.mu.Lock()
	delete(p.inWork, profileDir)
	p.mu.Unlock()
	close(f.done)
	return f.res, f.err
}

func (p *Prober) run(ctx context.Context, profileDir string) (Result, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.ClaudeBin,
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose", "-p",
		"--settings", probeSettings)
	cmd.Env = spawnEnv(os.Environ(), profileDir)
	cmd.Stdin = strings.NewReader(probeRequest + "\n")

	out, err := cmd.Output()
	if err != nil {
		return Result{}, fmt.Errorf("usage: probe %s: %w", profileDir, err)
	}
	res, ok := parseProbe(out)
	if !ok {
		return Result{}, fmt.Errorf("usage: probe %s: no control_response with rate limits", profileDir)
	}
	return res, nil
}

// controlResponse is the shape this reads out of the stream. Only the fields
// that are used are named: the response carries session totals and a spend
// figure too, and both are deliberately ignored. The spend field is not always
// populated (a rapid repeat comes back seeded from headers), and keeping
// cachedUsageUtilization as the single source for billed dollars is what lets
// the "billed figure as of" caption stay honest when a probe fails.
type controlResponse struct {
	Type     string `json:"type"`
	Response struct {
		Response struct {
			SubscriptionType    *string `json:"subscription_type"`
			RateLimitsAvailable bool    `json:"rate_limits_available"`
			// RateLimits is deliberately RawMessage rather than a typed map.
			// It is heterogeneous: measured on a live response, 25 keys of
			// which 5 were objects, 17 null, 2 arrays and one a bool. Decoding
			// it as map[string]someStruct fails on the first array and takes
			// the whole response down with it, which is exactly what happened:
			// the parser saw the control response, rejected it, and reported
			// "no control_response with rate limits" for every profile.
			RateLimits json.RawMessage `json:"rate_limits"`
		} `json:"response"`
	} `json:"response"`
}

// rateLimits is the one field inside that blob this reads.
//
// limits[] is used rather than the sibling five_hour and seven_day keys
// because it is byte-for-byte the same shape as cachedUsageUtilization's own
// .utilization.limits[], which bin/limits-poller.sh parses with the same kind
// mapping. Agreeing with the poller is the point: a probe's window has to land
// on the store's window of the same name, or a snapshot grows a second set of
// windows beside the ones it meant to refresh. It also carries weekly_scoped,
// which the named keys do not.
type rateLimits struct {
	Limits []struct {
		Kind     string  `json:"kind"`
		Percent  float64 `json:"percent"`
		ResetsAt string  `json:"resets_at"`
	} `json:"limits"`
}

// windowOfKind maps the API's kind onto the window name every metric,
// query and page in this repo uses. The same mapping as
// bin/limits-poller.sh's `win=$kind; weekly_all -> seven_day; session ->
// five_hour`. An unknown kind is skipped rather than passed through: a window
// nothing else knows the name of cannot be compared against anything.
var windowOfKind = map[string]string{
	"session":       "five_hour",
	"weekly_all":    "seven_day",
	"weekly_scoped": "weekly_scoped",
}

// parseProbe reads the first control_response out of the stream.
//
// ok is false only when no control_response arrived at all, which is a failed
// probe. A control_response saying rate_limits_available false IS a successful
// probe with a real answer, and comes back ok with Available false.
func parseProbe(out []byte) (Result, bool) {
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	// Control responses are small, but the stream can carry large lines
	// before one; a default 64KiB buffer would stop the scan early.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var cr controlResponse
		if err := json.Unmarshal(sc.Bytes(), &cr); err != nil {
			continue
		}
		if cr.Type != "control_response" {
			continue
		}
		r := cr.Response.Response
		res := Result{Available: r.RateLimitsAvailable, Windows: map[string]Window{}}
		if r.SubscriptionType != nil {
			res.Subscription = *r.SubscriptionType
		}
		// A rate_limits blob that will not decode leaves the windows empty
		// rather than failing the whole probe: rate_limits_available and the
		// subscription are still real answers, and a caller is better told
		// "this profile is signed in but no windows parsed" than nothing.
		var rl rateLimits
		if len(r.RateLimits) > 0 {
			_ = json.Unmarshal(r.RateLimits, &rl)
		}
		for _, l := range rl.Limits {
			name, known := windowOfKind[l.Kind]
			if !known {
				continue
			}
			win := Window{Pct: l.Percent}
			if ts, ok := parseResetsAt(l.ResetsAt); ok {
				win.ResetTS, win.HasReset = ts, true
			}
			res.Windows[name] = win
		}
		return res, true
	}
	return Result{}, false
}

// parseResetsAt turns the API's RFC 3339 resets_at into epoch seconds.
//
// It ROUNDS the sub-second part rather than truncating, matching
// bin/limits-poller.sh. The same reset instant comes back either side of the
// whole second between refetches (02:59:59.574463 and 03:00:00.410182 were
// both observed for one boundary), and truncating made the epoch alternate by
// one, which usage-alert.sh's dedup read as a fresh reset period and re-fired
// every crossed threshold. Rounding reduces that; it does not cure it, and
// nothing should be built on it holding.
func parseResetsAt(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, false
	}
	return t.Round(time.Second).Unix(), true
}
