package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentobservability "github.com/heapsgoodstudio/agent-observability"
	"github.com/heapsgoodstudio/agent-observability/internal/config"
	"github.com/heapsgoodstudio/agent-observability/internal/installer"
)

func cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	installDir := fs.String("install-dir", config.DefaultDir(), "where to install")
	profileDir := fs.String("profile-dir", "", "explicit Claude profile directory (otherwise retain configured profiles or detect defaults)")
	claudeBinFlag := fs.String("claude-bin", "", "claude binary to record (default: the installed one, else detection)")
	profileDirs := fs.String("profile-dirs", "", "colon-separated extra profiles to instrument alongside --profile-dir")
	yes := fs.Bool("yes", false, "non-interactive; accept every default")
	upgrade := fs.Bool("upgrade", false, "reinstall over an existing install, reusing its settings and asking nothing; fails if not installed")
	dryRun := fs.Bool("dry-run", false, "print the plan and change nothing")
	backfill := fs.Bool("backfill", false, "force the transcript backfill on")
	noBackfill := fs.Bool("no-backfill", false, "force the transcript backfill off")
	fs.Parse(args)

	// Before anything else, including the dry run: installing into the
	// checkout is what writes a config.env there, and a config.env is what
	// arms every script in bin/ to keep writing state beside it.
	if err := config.CheckInstallDir(*installDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	x := installer.RealExec{}
	home, _ := os.UserHomeDir()
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot locate own binary:", err)
		return 1
	}

	report := installer.Inspect(x, home, "", *installDir)
	explicitProfiles := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "profile-dir" || f.Name == "profile-dirs" {
			explicitProfiles = true
		}
	})
	for port, state := range report.Ports {
		if state == "busy" {
			fmt.Fprintf(os.Stderr, "port %d is in use by something other than this stack\n", port)
			return 1
		}
	}

	// A re-install already knows everything the wizard would ask for: the
	// install's own config.env records the profile it instruments. Reusing
	// that is what makes an upgrade answer-free, and it is also the correct
	// answer rather than merely the convenient one. Without it, a
	// non-interactive run falls back to DiscoverProfiles' first hit, which is
	// CLAUDE_CONFIG_DIR or ~/.claude; a profile living anywhere else is not
	// even discoverable, so the upgrade would silently re-point the whole
	// stack at a different profile. An explicit --profile-dir still wins, so
	// moving an install to another profile stays possible.
	var installed *config.Config
	if report.Installed {
		if c, err := config.Load(filepath.Join(*installDir, "config.env")); err == nil {
			installed = c
		}
	}
	if *upgrade && installed == nil {
		fmt.Fprintf(os.Stderr, "--upgrade: no readable install at %s; run `claude-obs install` first\n", *installDir)
		return 1
	}
	if *upgrade {
		*yes = true
	}
	if *profileDir == "" && installed != nil {
		if p := installed.Get("PROFILE_DIR", ""); p != "" {
			*profileDir = p
			fmt.Printf("reusing the installed profile: %s\n", p)
		}
	}
	// The list is reused on the same argument as the primary: none of these
	// profiles need live anywhere DiscoverProfiles looks, so what the install
	// already records is the only thing that finds them again.
	var extraProfiles []string
	if *profileDirs != "" {
		extraProfiles = splitProfiles(*profileDirs)
	} else if installed != nil {
		extraProfiles = installer.ConfiguredProfiles(installed)
		if len(extraProfiles) > 1 {
			fmt.Printf("reusing the installed profile list: %s\n", strings.Join(extraProfiles, ", "))
		}
	}

	// Resolving the claude binary, in priority order:
	//
	//  1. --claude-bin, when given. The explicit answer always wins, which is
	//     what makes deliberately moving an install to another binary possible.
	//  2. The binary this install already records, when it still validates.
	//     This is the same correctness argument --upgrade already makes for
	//     PROFILE_DIR, for the same reason: detection reads PATH, and PATH
	//     differs between an interactive shell and whatever runs an upgrade.
	//     An install that has been working is better evidence than a fresh
	//     lookup in an environment nobody chose. A recorded path that has
	//     stopped validating falls through, so a moved binary still self-heals.
	//  3. Detection: the canonical location, then PATH. See detectClaude.
	var claudeBin string
	var claudeErr error
	recorded := ""
	if installed != nil {
		recorded = installed.Get("CLAUDE_BIN", "")
	}
	switch {
	case *claudeBinFlag != "":
		claudeBin = *claudeBinFlag
		claudeErr = installer.ValidateClaudeBin(claudeBin, x)
	case recorded != "" && installer.ValidateClaudeBin(recorded, x) == nil:
		claudeBin = recorded
		fmt.Printf("reusing the installed claude binary: %s\n", recorded)
	default:
		claudeBin, claudeErr = detectClaude(home, x)
	}
	if claudeErr != nil && *claudeBinFlag != "" {
		fmt.Fprintln(os.Stderr, "claude binary", claudeErr)
		return 1
	}
	if claudeErr != nil {
		claudeBin = ""
		fmt.Println("Claude executable unavailable; local history and dashboard setup remain available.")
	}
	in := installer.Inputs{
		Assets: agentobservability.Assets, InstallDir: *installDir, ProfileDir: *profileDir,
		ProfileDirs: extraProfiles, Home: home, ExplicitProfiles: explicitProfiles,
		ClaudeBin: claudeBin, SelfBin: self,
		SwiftcOK: report.SwiftcOK, UID: os.Getuid(),
	}
	backfillFlag := "auto"
	if *backfill {
		backfillFlag = "yes"
	}
	if *noBackfill {
		backfillFlag = "no"
	}

	if *dryRun {
		if claudeErr != nil {
			fmt.Fprintln(os.Stderr, "warning: claude binary:", claudeErr)
		}
		in.Backfill = backfillFlag != "no" && (!report.Installed || backfillFlag == "yes")
		if in.ProfileDir == "" && len(report.Profiles) > 0 {
			in.ProfileDir = report.Profiles[0]
		}
		for _, d := range installer.Describe(installer.BuildPlan(in)) {
			fmt.Println("would:", d)
		}
		return 0
	}
	if err := installer.RunWizard(report, &in, *yes, backfillFlag); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Every profile is validated, not just the primary: config.env is sourced
	// by bash, so one unsafe path anywhere in the list is the same hazard the
	// single-profile check was written for.
	for _, dir := range in.Profiles() {
		if err := installer.ValidateSafePath("profile dir", dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if fi, err := os.Stat(dir); in.ExplicitProfiles && (err != nil || !fi.IsDir()) {
			fmt.Fprintln(os.Stderr, "profile dir not found:", dir)
			return 1
		}
	}
	if err := installer.Execute(installer.BuildPlan(in), x, func(s string) { fmt.Println(s) }); err != nil {
		fmt.Fprintln(os.Stderr, "install failed:", err)
		return 1
	}
	fmt.Println("installed. Dashboard: http://localhost:8427")
	fmt.Println("note: telemetry starts with your next agent session. Manage profiles and history imports in Settings.")
	return 0
}

// detectClaude resolves the claude binary to record in config.env.
//
// The canonical install path is tried BEFORE PATH, which is the opposite of
// what it used to do and is the whole point. A bare lookPath("claude") takes
// whatever sits earliest on PATH, and a wrapper placed there deliberately to
// intercept subprocess invocations is exactly the kind of thing that sits
// earliest. One such wrapper refused every usage probe for five minutes on a
// live install: it demands an in-scope working directory, the poller's launchd
// job had none, and the probe exited 3 with the billed figure silently frozen.
//
// ValidateClaudeBin cannot be the defence, because a wrapper passes every one
// of its checks: it is an absolute path, it is executable, and it answers
// --version, which such a wrapper passes straight through by design. Nor can
// "reject shell scripts": claude is a native binary in some installs and a
// script in others. Preferring the real install location is the rule that
// holds without knowing anything about what else is on PATH.
//
// PATH is still consulted, so a claude installed somewhere else entirely is
// found; it is just no longer preferred over the canonical one.
// splitProfiles parses a colon-separated profile list, the same spelling
// config.env uses and bin/limits-poller.sh reads.
func splitProfiles(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ":") {
		if p = strings.TrimRight(strings.TrimSpace(p), "/"); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func detectClaude(home string, x installer.Exec) (string, error) {
	return detectClaudeWith(home, func(p string) error {
		return installer.ValidateClaudeBin(p, x)
	})
}

// detectClaudeWith is detectClaude's ordering, with validation injected.
// Split out so the preference can be tested without ValidateClaudeBin's own
// rules getting in the way: it refuses any path under TMPDIR as a temporary
// shim, which is where a test's scratch directories live.
func detectClaudeWith(home string, validate func(string) error) (string, error) {
	canonical := filepath.Join(home, ".local", "bin", "claude")
	if err := validate(canonical); err == nil {
		return canonical, nil
	}
	candidate, err := lookPath("claude")
	if err != nil {
		candidate = canonical
	}
	return candidate, validate(candidate)
}
