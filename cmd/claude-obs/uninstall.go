package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"charm.land/huh/v2"
	"github.com/heapsgoodstudio/agent-observability/internal/config"
	"github.com/heapsgoodstudio/agent-observability/internal/installer"
)

func cmdUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	installDir := fs.String("install-dir", config.DefaultDir(), "install root to remove")
	purge := fs.Bool("purge", false, "also delete store data (vmdata, vldata) and the store binaries")
	yes := fs.Bool("yes", false, "non-interactive; skip the confirmation")
	fs.Parse(args)

	cfg, err := config.Load(filepath.Join(*installDir, "config.env"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "no config.env: nothing installed here?")
		return 1
	}

	if !*yes {
		confirm := false
		if err := huh.NewConfirm().
			Title("Remove Agent Observability? Its launchd agents, notifier, and settings wiring will be removed.").
			Value(&confirm).Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if !confirm {
			fmt.Println("aborted")
			return 1
		}
	}

	x := installer.RealExec{}
	// Every instrumented profile, not just the primary: leaving the others
	// wired would keep them exporting to stores that are no longer running.
	steps := installer.UninstallSteps(*installDir, installer.ConfiguredProfiles(cfg), cfg.Get("NOTIFIER_APP", ""), os.Getuid(), *purge)
	if err := installer.Execute(steps, x, func(s string) { fmt.Println(s) }); err != nil {
		fmt.Fprintln(os.Stderr, "uninstall failed:", err)
		return 1
	}
	fmt.Printf("done. Left in place: %s and its state directory.\n", *installDir)
	return 0
}
