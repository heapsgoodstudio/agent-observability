// claude-obs: Agent Observability. One binary, launchd-scheduled subcommands.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/analysis"
	"github.com/heapsgoodstudio/agent-observability/internal/config"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/installer"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/server"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vl"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
	usagepkg "github.com/heapsgoodstudio/agent-observability/internal/usage"
)

var version = "dev" // set via -ldflags "-X main.version=..."

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(cmdServe(os.Args[2:]))
	case "install":
		os.Exit(cmdInstall(os.Args[2:]))
	case "uninstall":
		os.Exit(cmdUninstall(os.Args[2:]))
	case "doctor":
		os.Exit(cmdDoctor(os.Args[2:]))
	case "index":
		os.Exit(cmdIndex(os.Args[2:]))
	case "backfill":
		os.Exit(cmdBackfill(os.Args[2:]))
	case "migrate-history-labels":
		os.Exit(cmdRelabel(os.Args[2:]))
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: claude-obs <serve|install|uninstall|doctor|index|backfill|migrate-history-labels|version> [flags]")
}

// lookPath is exec.LookPath behind a var so tests can resolve a fake claude
// without depending on the host's PATH.
var lookPath = func(name string) (string, error) { return exec.LookPath(name) }

// envSeconds reads key from the OS environment as a whole number of seconds;
// an absent or unparseable value falls back to def.
// usageProber returns a prober only when USAGE_ON_DEMAND_PROBE is 1. Nil
// leaves GetUsageSnapshot serving the poller's store and spawning nothing,
// and the response says so through on_demand_probe_enabled rather than
// ignoring max_age_s silently.
func usageProber(cfg *config.Config, claudeBin string) *usagepkg.Prober {
	if cfg == nil || cfg.Get("USAGE_ON_DEMAND_PROBE", "0") != "1" {
		return nil
	}
	return &usagepkg.Prober{ClaudeBin: claudeBin}
}

// creditPrice reads CODEX_CREDIT_USD. Anything but a positive finite number,
// an empty value included, means no price, so Codex overage stays in credits.
func creditPrice(v string) float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || n <= 0 || math.IsInf(n, 0) || math.IsNaN(n) {
		return 0
	}
	return n
}

func envSeconds(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return time.Duration(n) * time.Second
}

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	installDir := fs.String("install-dir", config.DefaultDir(), "install root (holds config.env, dashboard/, state/)")
	addr := fs.String("addr", fmt.Sprintf("127.0.0.1:%d", server.DashPort), "listen address, loopback only")
	dir := fs.String("dir", "", "static dashboard dir override (default: serve the embedded frontend)")
	fs.Parse(args)

	// serve opens <install-dir>/state/claude-obs.db and hands
	// <install-dir>/dashboard/data to the analysis runner as its OutRoot, so
	// the checkout would collect a database and real reports. Point a dev run
	// at a scratch dir instead; one has no go.mod, so this never fires on it.
	if err := config.CheckInstallDir(*installDir); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		return 1
	}

	static := *dir
	home, _ := os.UserHomeDir()

	// A missing config.env degrades every field it would have supplied
	// (ClaudeBin, ProfileDir) to their own fallback below rather than
	// stopping the server: the same "a fresh install's serve must work
	// before the rest of the install machinery has run" rule the db open
	// just below already follows.
	cfg, cfgErr := config.Load(filepath.Join(*installDir, "config.env"))
	if cfgErr != nil {
		fmt.Fprintln(os.Stderr, "serve: load config:", cfgErr)
	}

	// CLAUDE_BIN resolution, in order: config.env's CLAUDE_BIN, else `claude`
	// resolved on PATH, else $HOME/.local/bin/claude.
	claudeBin := ""
	if cfg != nil {
		claudeBin = cfg.Get("CLAUDE_BIN", "")
	}
	if claudeBin == "" {
		if p, err := lookPath("claude"); err == nil {
			claudeBin = p
		} else {
			claudeBin = filepath.Join(home, ".local", "bin", "claude")
		}
	}
	profileDir := filepath.Join(home, ".claude")
	var profileDirs []string
	if cfg != nil {
		profileDir = cfg.Get("PROFILE_DIR", profileDir)
		profileDirs = installer.ConfiguredProfiles(cfg)
	}

	// Opened (and migrated, idempotently) here rather than assumed present:
	// a fresh install's serve must work even before the first `index` pass
	// has run. A failure here degrades ListSessions/GetSession to live-only
	// data (Options.DB stays nil) rather than stopping the whole server, so
	// the rest of the dashboard keeps working.
	if err := os.MkdirAll(filepath.Join(*installDir, "state"), 0700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	statePath := filepath.Join(*installDir, "state", "claude-obs.db")
	var stateDB *sql.DB
	if d, err := db.Open(statePath); err != nil {
		fmt.Fprintln(os.Stderr, "serve: open db:", err)
	} else if _, err := db.Migrate(d); err != nil {
		fmt.Fprintln(os.Stderr, "serve: migrate db:", err)
		d.Close()
	} else {
		stateDB = d
	}

	dataDir := filepath.Join(*installDir, "dashboard", "data")
	vlURL := fmt.Sprintf("http://127.0.0.1:%d", server.VLPort)

	analysisTimeout := envSeconds("ANALYSIS_TIMEOUT_S", analysis.DefaultAnalysisTimeout)
	evidenceTimeout := envSeconds("ADVICE_EVIDENCE_TIMEOUT_S", analysis.DefaultEvidenceTimeout)

	flightGate := &analysis.FlightGate{}
	runner := analysis.New(analysis.Config{
		FlightGate: flightGate,
		ClaudeBin:  claudeBin,
		ProfileDir: profileDir,
		DataDir:    dataDir,
		// Advice and sessions without a surviving recorded cwd use the install
		// directory. Session analysis prefers its own recorded workspace, so
		// profile hooks and project settings see the correct repository.
		WorkDir:         *installDir,
		DB:              stateDB,
		VLURL:           vlURL,
		AnalysisTimeout: analysisTimeout,
		EvidenceTimeout: evidenceTimeout,
	})

	// Before serving a single request: any run still recorded as "running"
	// belongs to a process that no longer exists, since this one has not
	// started any. Left alone, such a status claims a run is in flight until
	// the frontend's stuck rule times it out half an hour later. A failure
	// here is logged rather than fatal: an unrepaired status is a wrong label
	// on one run, and refusing to serve over it would be worse.
	if n, err := runner.ReapOrphanedRuns(); err != nil {
		fmt.Fprintln(os.Stderr, "serve: reap orphaned runs:", err)
	} else if n > 0 {
		fmt.Printf("serve: marked %d interrupted run(s) as failed\n", n)
	}

	profileStore := profiles.New(*installDir)
	manager := monitor.New(*installDir, stateDB)
	codexBin := filepath.Join(home, ".local", "bin", "codex")
	if _, err := os.Stat(codexBin); err != nil {
		codexBin, _ = lookPath("codex")
	}
	if cfg != nil {
		codexBin = cfg.Get("CODEX_BIN", codexBin)
	}
	codexBin = analysis.Binary("codex", codexBin)
	manager.CodexBinary = codexBin

	binaries := map[string]string{"claude": claudeBin, "codex": codexBin}
	for _, provider := range []string{"omp", "opencode"} {
		configured := ""
		if cfg != nil {
			configured = cfg.Get(strings.ToUpper(provider)+"_BIN", "")
		}
		binaries[provider] = analysis.Binary(provider, configured)
	}
	runnerForTarget := func(selection analysis.Selection) (*analysis.Runner, error) {
		ps, err := profileStore.List()
		if err != nil {
			return nil, err
		}
		provider := selection.Provider
		if provider == "" {
			provider = "claude"
		}
		var selected *profiles.Profile
		sources := map[string]string{}
		sourceDir := ""
		sourceFound := selection.SourceProfile == ""
		for i := range ps {
			p := &ps[i]
			if p.Enabled {
				sources[p.ID] = p.Directory
			}
			if p.ID == selection.SourceProfile {
				sourceFound = true
				sourceDir = p.Directory
				if selection.SourceProvider != "" && selection.SourceProvider != string(p.Provider) {
					return nil, fmt.Errorf("source profile does not belong to selected provider")
				}
				selection.SourceProvider = string(p.Provider)
			}
			if p.Enabled && string(p.Provider) == provider && ((selection.Profile != "" && p.ID == selection.Profile) || (selection.Profile == "" && p.Primary)) {
				selected = p
			}
		}
		if !sourceFound {
			return nil, fmt.Errorf("source profile is not configured")
		}
		if selection.SourceProvider != "" && selection.SourceProvider != "claude" && selection.SourceProvider != "codex" && selection.SourceProvider != "omp" && selection.SourceProvider != "opencode" {
			return nil, fmt.Errorf("unknown source provider")
		}
		if selected == nil {
			return nil, fmt.Errorf("select an enabled %s profile in Settings", provider)
		}
		if st, err := os.Stat(selected.Directory); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("selected profile directory is unavailable")
		}
		binary := analysis.Binary(provider, binaries[provider])
		if binary == "" {
			return nil, fmt.Errorf("%s CLI is not installed", provider)
		}
		return analysis.New(analysis.Config{FlightGate: flightGate, Provider: provider, ProfileID: selected.ID, Binary: binary, ProfileDir: selected.Directory, SourceDir: sourceDir, SourceProvider: selection.SourceProvider, SourceProfile: selection.SourceProfile, SourceProfiles: selection.SourceProfiles, SourceAccountKey: selection.SourceAccountKey, Sources: sources, DataDir: dataDir, WorkDir: *installDir, DB: stateDB, VLURL: vlURL, AnalysisTimeout: analysisTimeout, EvidenceTimeout: evidenceTimeout}), nil
	}
	go manager.Run(context.Background())
	legacyBillingAccount := ""
	if cfg != nil {
		legacyBillingAccount = cfg.Get("LEGACY_CLAUDE_BILLING_ACCOUNT", "")
	}
	codexCreditUSD := 0.0
	if cfg != nil {
		codexCreditUSD = creditPrice(cfg.Get("CODEX_CREDIT_USD", ""))
	}
	h := server.Handler(server.Options{
		Profiles: profileStore, Monitor: manager, RunnerForTarget: runnerForTarget, AnalysisBinaries: binaries,
		Addr: *addr, StaticDir: static, InstallDir: *installDir, Version: version,
		VM:              &vm.Client{BaseURL: fmt.Sprintf("http://127.0.0.1:%d", server.VMPort)},
		VL:              &vl.Client{BaseURL: vlURL},
		Memo:            store.New(store.TTL),
		DB:              stateDB,
		DataDir:         dataDir,
		LaunchAgentsDir: filepath.Join(home, "Library", "LaunchAgents"),
		Runner:          runner,
		AnalysisTimeout: analysisTimeout,
		EvidenceTimeout: evidenceTimeout,
		// The profile PROFILE_DIR names, which is the one this install was
		// originally pointed at. GetOverview reports its account in the flat
		// fields and as primary_account, so an install instrumenting a single
		// profile reads exactly as it did before several were possible.
		PrimaryProfile:             installer.ProfileName(profileDir),
		LegacyClaudeBillingAccount: legacyBillingAccount,
		CodexCreditUSD:             codexCreditUSD,
		// Every instrumented profile, primary first, read through the same
		// accessor bin/limits-poller.sh's PROFILE_DIRS fallback uses, so the
		// set GetUsageSnapshot can probe is exactly the set the poller polls.
		// Empty (no config.env) leaves on-demand probing off and that RPC
		// serving the store, which is still a correct answer.
		ProfileDirs: profileDirs,
		// Off unless config.env asks for it. A probe is a full Claude Code
		// startup, hooks and all, and a server that spawns those on request is
		// a bigger thing than an observability read should be. The poller
		// already refreshes every profile every five minutes, which is fresh
		// enough for what this serves.
		Prober: usageProber(cfg, claudeBin),
	})
	staticDesc := static
	if staticDesc == "" {
		staticDesc = "embedded frontend"
	}
	fmt.Printf("claude-obs %s serving %s on %s\n", version, staticDesc, *addr)
	if err := http.ListenAndServe(*addr, h); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		return 1
	}
	return 0
}
