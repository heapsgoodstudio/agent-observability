package server

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/analysis"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vl"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// staleAfterS and indexStaleS are the freshness cutoffs: plan data older than
// the first, or a session index older than the second, is flagged stale
// rather than merely reported as read. Ten minutes gives the five-minute
// Claude probe cadence room for scheduling and network delays. Keep this
// aligned with CLAUDE_FRESH_FOR_S in frontend/src/lib/allowance.ts.
const (
	staleAfterS = 600
	indexStaleS = 900
)

// Keep profile/account/source labels and the original observation time. Using
// now minus an emitted age would make a stopped poller keep appearing fresh.
const pollerQuery = `timestamp(claude_plan_data_age_seconds) - claude_plan_data_age_seconds`
const confirmedQuery = `timestamp(claude_plan_data_confirmed_age_seconds) - claude_plan_data_confirmed_age_seconds`
const healthAccountQuery = `timestamp(claude_profile_account_info)`
const healthAvailabilityQuery = `claude_profile_usage_available`

const (
	logVolumeQuery = `_time:24h | stats by (event.name) count() n`
	logVolumeLimit = 1000
)

// sourceLabels names the two plan-data poller sources; any other source name
// is reported as "plan data, <source>" verbatim.
var sourceLabels = map[string]string{
	"cache":      "plan data, cache",
	"statusline": "plan data, statusline",
}

// pollerSeriesNames are the three series bin/limits-poller.sh writes on a
// fixed schedule: a missing sample is a genuine outage and fails healthOk().
// claude_code.cost.usage and claude_code.token.usage (the OTel exporter's
// own series) are deliberately excluded: those only exist while Claude Code
// is actually running, so their absence overnight is normal, not a problem.
var pollerSeriesNames = map[string]bool{
	"claude_in_overage":      true,
	"claude_extra_usage_usd": true,
	"claude_plan_window_pct": true,
}

type seriesSpec struct{ Name, Query string }

// expectedSeries is every series the Health page checks for a recent sample:
// a series missing here means nothing is writing it any more.
var expectedSeries = []seriesSpec{
	{"claude_in_overage", "count(count_over_time(claude_in_overage[1h]))"},
	{"claude_extra_usage_usd", "count(count_over_time(claude_extra_usage_usd[1h]))"},
	{"claude_plan_window_pct", "count(count_over_time(claude_plan_window_pct[1h]))"},
	{costMetric, fmt.Sprintf(`count(count_over_time({__name__=%q}[1h]))`, costMetric)},
	{tokenMetric, fmt.Sprintf(`count(count_over_time({__name__=%q}[1h]))`, tokenMetric)},
}

// memoVL adapts a *vl.Client to route every query through store.Memo, keyed
// by the query text itself.
type memoVL struct {
	client *vl.Client
	memo   *store.Memo
}

func (m memoVL) Query(ctx context.Context, query string, limit int) ([]map[string]string, error) {
	key := fmt.Sprintf("vl|%s|%d", query, limit)
	v, err := m.memo.Do(key, func() (any, error) { return m.client.Query(ctx, query, limit) })
	if err != nil {
		return nil, err
	}
	return v.([]map[string]string), nil
}

// ageText renders seconds since an observation at whichever precision that
// age deserves.
func ageText(s float64) string {
	switch {
	case s < 90:
		return fmt.Sprintf("%ds", int(math.Round(s)))
	case s < 5400:
		return fmt.Sprintf("%dm", int(math.Round(s/60)))
	case s < 2*86400:
		return fmt.Sprintf("%.1fh", s/3600)
	default:
		return fmt.Sprintf("%.1fd", s/86400)
	}
}

// planDataChecks only allows a confirmed cache to cover an idle statusline
// within the same enabled profile and current account. A shared account does
// not prove that another profile's poller is working.
func planDataChecks(ctx context.Context, o Options, q memoVM, observed []vm.Series, queryErr error, now int64) []*claudeobsv1.HealthCheck {
	type owner struct{ profile, account string }
	names := map[string]string{}
	owners := map[owner]bool{}
	var checks []*claudeobsv1.HealthCheck
	profileLabel := func(id string) string {
		name := names[id]
		if name == "" || name == id {
			return id
		}
		return name + " (" + id + ")"
	}
	if o.Profiles != nil {
		ps, err := o.Profiles.List()
		if err != nil {
			return []*claudeobsv1.HealthCheck{{Name: "plan data", State: "unknown", Detail: "Could not read enabled profiles: " + err.Error()}}
		}
		for _, p := range ps {
			if p.Enabled && p.Provider == profiles.Claude {
				names[p.ID] = p.Name
			}
		}
		if len(names) == 0 {
			return nil
		}
	}
	if queryErr != nil {
		return []*claudeobsv1.HealthCheck{{Name: "plan data", State: "unknown", Detail: "Could not check: " + queryErr.Error()}}
	}
	confirmed, confirmationErr := q.Q(ctx, confirmedQuery)
	if o.Profiles != nil {
		// A profile may have changed accounts while the old labelled series is
		// still in VM's lookbehind. Resolve the newest identity sample first.
		identity, identityErr := q.Q(ctx, healthAccountQuery)
		type binding struct {
			account   string
			at        float64
			ambiguous bool
		}
		latest := map[string]binding{}
		for _, s := range identity {
			at, ok := vm.SeriesValue(s)
			if !ok || !finite(at) || at <= 0 || at > float64(now+60) {
				continue
			}
			id, account := s.Metric["profile"], s.Metric["account"]
			old, exists := latest[id]
			if !exists || at > old.at {
				latest[id] = binding{account: account, at: at}
			} else if at == old.at && account != old.account {
				old.ambiguous = true
				latest[id] = old
			}
		}
		ids := make([]string, 0, len(names))
		for id := range names {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			b, ok := latest[id]
			if identityErr != nil || !ok || b.ambiguous || b.account == "" || b.account == "unknown" {
				detail := "Current account identity is unavailable; waiting for an attributable poller observation."
				if identityErr != nil {
					detail = "Could not check account identity: " + identityErr.Error()
				}
				checks = append(checks, &claudeobsv1.HealthCheck{Name: "plan data, identity · " + profileLabel(id), State: "unknown", Detail: detail})
				continue
			}
			owners[owner{id, b.account}] = true
		}
	} else {
		for _, series := range [][]vm.Series{observed, confirmed} {
			for _, s := range series {
				owners[owner{s.Metric["profile"], s.Metric["account"]}] = true
			}
		}
	}
	if confirmationErr != nil {
		checks = append(checks, &claudeobsv1.HealthCheck{Name: "plan data, confirmations", State: "unknown", Detail: "Could not check live cache confirmations: " + confirmationErr.Error()})
	}
	if len(owners) == 0 && len(checks) == 0 {
		return []*claudeobsv1.HealthCheck{{Name: "plan data", State: "warn", Detail: "claude_plan_data_age_seconds has no samples"}}
	}
	type reading struct {
		at    float64
		known bool
	}
	read := func(series []vm.Series) map[owner]map[string]reading {
		out := map[owner]map[string]reading{}
		for _, s := range series {
			key := owner{s.Metric["profile"], s.Metric["account"]}
			if !owners[key] {
				continue
			}
			if out[key] == nil {
				out[key] = map[string]reading{}
			}
			at, ok := vm.SeriesValue(s)
			ok = ok && finite(at) && at > 0 && at <= float64(now+60)
			source := s.Metric["source"]
			old, exists := out[key][source]
			if !exists || ok && (!old.known || at > old.at) {
				out[key][source] = reading{at, ok}
			}
		}
		return out
	}
	changed, confirmations := read(observed), read(confirmed)
	unavailable := map[owner]bool{}
	if o.Profiles != nil && len(owners) > 0 {
		availability, err := q.Q(ctx, healthAvailabilityQuery)
		if err != nil {
			checks = append(checks, &claudeobsv1.HealthCheck{Name: "plan data, availability", State: "unknown", Detail: "Could not check provider allowance availability: " + err.Error()})
		} else {
			for _, s := range availability {
				key := owner{s.Metric["profile"], s.Metric["account"]}
				if value, ok := vm.SeriesValue(s); owners[key] && ok && value == 0 {
					unavailable[key] = true
				}
			}
		}
	}
	keys := make([]owner, 0, len(owners))
	for key := range owners {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].profile != keys[j].profile {
			return keys[i].profile < keys[j].profile
		}
		return keys[i].account < keys[j].account
	})
	for _, key := range keys {
		if unavailable[key] {
			checks = append(checks, &claudeobsv1.HealthCheck{Name: "plan data, cache · " + profileLabel(key.profile), State: "unknown", Detail: "Provider does not report allowance for this profile; check Claude sign-in."})
			continue
		}
		values := changed[key]
		if values == nil {
			values = map[string]reading{}
		}
		// The scheduled cache is expected even when only a live statusline has
		// reported a value. Missing supplemental statusline samples are normal.
		if _, ok := values["cache"]; !ok {
			values["cache"] = reading{}
		}
		conf := confirmations[key]["cache"]
		cacheConfirmed := confirmationErr == nil && values["cache"].known && conf.known && float64(now)-conf.at <= staleAfterS
		sources := make([]string, 0, len(values))
		for source := range values {
			sources = append(sources, source)
		}
		sort.Strings(sources)
		for _, source := range sources {
			name := sourceLabels[source]
			if name == "" {
				name = "plan data, " + source
			}
			if key.profile != "" {
				name += " · " + profileLabel(key.profile)
				if o.Profiles == nil && key.account != "" {
					name += " · account " + key.account
				}
			} else if key.account != "" {
				name += " · account " + key.account
			}
			r := values[source]
			check := &claudeobsv1.HealthCheck{Name: name, State: "unknown", Detail: "No trustworthy source observation time reported"}
			if r.known {
				age := math.Max(0, float64(now)-r.at)
				verdict := age
				check.Detail = "Updated " + ageText(age) + " ago"
				if source == "cache" && conf.known && confirmationErr == nil {
					confirmedAge := math.Max(0, float64(now)-conf.at)
					verdict = math.Min(age, confirmedAge)
					check.Detail = "Confirmed " + ageText(confirmedAge) + " ago; last value change " + ageText(age) + " ago"
				}
				check.State = "ok"
				if verdict > staleAfterS {
					check.State = "warn"
					check.Detail += ", stale"
				}
				if source == "statusline" && age > staleAfterS && cacheConfirmed {
					check.State = "ok"
					check.Detail = "Idle for " + ageText(age) + "; this profile's cache was confirmed " + ageText(math.Max(0, float64(now)-conf.at)) + " ago"
				}
			}
			checks = append(checks, check)
		}
	}
	return checks
}

func indexerCheck(d *sql.DB, now int64) *claudeobsv1.HealthCheck {
	if d == nil {
		return &claudeobsv1.HealthCheck{Name: "session index", State: "bad", Detail: "no index yet: has claude-obs index run?"}
	}
	generatedAtStr, ok, err := db.MetaGet(d, sessionsGeneratedAtKey)
	if err != nil {
		return &claudeobsv1.HealthCheck{Name: "session index", State: "bad", Detail: err.Error()}
	}
	if !ok {
		return &claudeobsv1.HealthCheck{Name: "session index", State: "bad", Detail: "no index yet: has claude-obs index run?"}
	}
	generatedAt, err := strconv.ParseInt(generatedAtStr, 10, 64)
	if err != nil {
		return &claudeobsv1.HealthCheck{Name: "session index", State: "bad", Detail: err.Error()}
	}
	rows, err := db.ListSessions(d, "")
	if err != nil {
		return &claudeobsv1.HealthCheck{Name: "session index", State: "bad", Detail: err.Error()}
	}
	age := float64(now - generatedAt)
	stale := age > indexStaleS
	state, suffix := "ok", ""
	if stale {
		state, suffix = "warn", ", stale"
	}
	return &claudeobsv1.HealthCheck{
		Name:   "session index",
		State:  state,
		Detail: fmt.Sprintf("%s sessions, generated %s ago%s", groupInt(int64(len(rows))), ageText(age), suffix),
	}
}

// retentionRe matches the two stores' -retentionPeriod flag: a bare number is
// months (VictoriaMetrics), a "w" suffix is weeks (VictoriaLogs), per each
// store's own flag semantics.
var retentionRe = regexp.MustCompile(`-retentionPeriod=(\d+)(w?)`)

func parseRetention(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	m := retentionRe.FindStringSubmatch(string(data))
	if m == nil {
		return "", false
	}
	if m[2] == "w" {
		return m[1] + " weeks", true
	}
	return m[1] + " months", true
}

// defaultVMRetention and defaultVLRetention restate what the launchd
// templates configure, used whenever either plist cannot be read and parsed.
// They must be kept in step with -retentionPeriod in
// internal/installer/templates/victoria{metrics,logs}.plist.tmpl.
const (
	defaultVMRetention = "24 months"
	defaultVLRetention = "52 weeks"
)

// loadRetention reads the two rendered launchd plists for this profile's
// stores. Both must parse for observed to be true: a partial read (one
// plist present, one missing or unparseable) is not enough to trust either
// figure, so it falls back to both defaults together.
func loadRetention(launchAgentsDir string) (vmRetention, vlRetention string, observed bool) {
	vmVal, vmOK := parseRetention(filepath.Join(launchAgentsDir, "com.agent-observability.victoriametrics.plist"))
	vlVal, vlOK := parseRetention(filepath.Join(launchAgentsDir, "com.agent-observability.victorialogs.plist"))
	if vmOK && vlOK {
		return vmVal, vlVal, true
	}
	return defaultVMRetention, defaultVLRetention, false
}

// runnerCheck reports the in-process analysis Runner's state. The Runner runs
// inside this binary, so there is no network hop that can fail. A nil Runner
// (a server built without one) is reported "bad": no analysis capability is
// available at all. Otherwise Runner.Busy() (backed
// by the analysis_runs table; see internal/analysis's own doc comment)
// distinguishes idle from busy with a run, matching the old probe's ok/warn
// split and wording exactly.
func runnerCheck(r *analysis.Runner) *claudeobsv1.HealthCheck {
	if r == nil {
		return &claudeobsv1.HealthCheck{Name: "Analysis runner", State: "bad", Detail: "not configured"}
	}
	if r.Busy() {
		return &claudeobsv1.HealthCheck{Name: "Analysis runner", State: "warn", Detail: "running, busy with an analysis run"}
	}
	return &claudeobsv1.HealthCheck{Name: "Analysis runner", State: "ok", Detail: "running, idle"}
}

// logVolumeRows ports renderLogVolume's parsing: name -> count, zero counts
// dropped, sorted by count descending.
func logVolumeRows(rows []map[string]string) []*claudeobsv1.AttrRow {
	type row struct {
		Name  string
		Value float64
	}
	var parsed []row
	for _, r := range rows {
		n, _ := strconv.ParseFloat(r["n"], 64)
		if n <= 0 {
			continue
		}
		name := r["event.name"]
		if name == "" {
			name = "(unnamed)"
		}
		parsed = append(parsed, row{name, n})
	}
	sort.SliceStable(parsed, func(i, j int) bool { return parsed[i].Value > parsed[j].Value })
	out := make([]*claudeobsv1.AttrRow, len(parsed))
	for i, p := range parsed {
		out[i] = &claudeobsv1.AttrRow{Key: p.Name, Value: p.Value}
	}
	return out
}

// buildHealth is GetHealth's real body. Unlike GetOverview, GetHealthResponse
// carries no SectionError list: every observation folds into a HealthCheck's
// own state (ok/warn/bad/unknown), which is the whole point of this page.
// now is threaded as a parameter for the same reason buildOverview takes one.
func buildHealth(ctx context.Context, o Options, now int64) *claudeobsv1.GetHealthResponse {
	resp := &claudeobsv1.GetHealthResponse{Identity: identity()}
	mvm := memoVM{client: o.VM, memo: o.Memo}
	mvl := memoVL{client: o.VL, memo: o.Memo}

	var checks []*claudeobsv1.HealthCheck
	healthy := true

	// VictoriaMetrics reachability and plan-data observations share one
	// query. The freshness rows are
	// appended after the three store-reachability checks below, so the page
	// reads store-first, even though the fetch happens here.
	pollerRes, pollerErr := mvm.Q(ctx, pollerQuery)
	if pollerErr != nil {
		checks = append(checks, &claudeobsv1.HealthCheck{
			Name: "VictoriaMetrics", State: "bad",
			Detail: fmt.Sprintf("%s unreachable: %v", o.VM.BaseURL, pollerErr),
		})
		healthy = false
	} else {
		checks = append(checks, &claudeobsv1.HealthCheck{
			Name: "VictoriaMetrics", State: "ok",
			Detail: fmt.Sprintf("%s reachable", o.VM.BaseURL),
		})
	}

	// VictoriaLogs reachability and 24h log volume share one query too.
	logRows, vlErr := mvl.Query(ctx, logVolumeQuery, logVolumeLimit)
	if vlErr != nil {
		checks = append(checks, &claudeobsv1.HealthCheck{
			Name: "VictoriaLogs", State: "bad",
			Detail: fmt.Sprintf("%s unreachable: %v", o.VL.BaseURL, vlErr),
		})
		healthy = false
	} else {
		checks = append(checks, &claudeobsv1.HealthCheck{
			Name: "VictoriaLogs", State: "ok",
			Detail: fmt.Sprintf("%s reachable", o.VL.BaseURL),
		})
		resp.LogVolume_24H = logVolumeRows(logRows)
	}

	runnerC := runnerCheck(o.Runner)
	checks = append(checks, runnerC)
	if runnerC.State == "bad" {
		healthy = false
	}

	checks = append(checks, planDataChecks(ctx, o, mvm, pollerRes, pollerErr, now)...)

	checks = append(checks, indexerCheck(o.DB, now))

	for _, spec := range expectedSeries {
		res, err := mvm.Q(ctx, spec.Query)
		var check *claudeobsv1.HealthCheck
		if err != nil {
			check = &claudeobsv1.HealthCheck{Name: spec.Name, State: "unknown", Detail: fmt.Sprintf("could not check: %v", err)}
		} else {
			n, ok := vm.Scalar(res)
			present := ok && n > 0
			state, detail := "warn", "no sample in the last hour"
			if present {
				state, detail = "ok", "sample in the last hour"
			}
			check = &claudeobsv1.HealthCheck{Name: spec.Name, State: state, Detail: detail}
		}
		checks = append(checks, check)
		if pollerSeriesNames[spec.Name] && check.State == "warn" {
			healthy = false
		}
	}

	resp.Checks = checks
	resp.VmRetention, resp.VlRetention, resp.RetentionObserved = loadRetention(o.LaunchAgentsDir)
	resp.Healthy = healthy
	return resp
}
