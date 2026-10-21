package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/heapsgoodstudio/agent-observability/internal/config"
	"github.com/heapsgoodstudio/agent-observability/internal/indexer"
)

// cmdBackfill parses every transcript under PROFILE_DIR/projects into the
// four claude_hist_* series
// and (unless --dry-run) import them into VictoriaMetrics, reconciling the
// imported total against the computed one.
func cmdBackfill(args []string) int {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	installDir := fs.String("install-dir", config.DefaultDir(), "install root (holds config.env)")
	projectsRoot := fs.String("projects-root", "", "Claude Code projects dir to scan (default: config.env's PROFILE_DIR/projects)")
	vmURL := fs.String("vm-url", "", "VictoriaMetrics base URL (default: config.env's VM_URL)")
	dryRun := fs.Bool("dry-run", false, "compute and print totals only; skip the VictoriaMetrics import")
	profile := fs.String("profile", "", "label every cost sample with this profile (default: none, the unlabelled series)")
	force := fs.Bool("i-know-this-deletes-history", false, "allow --reset-cost-series even when it would delete history no transcript can rebuild")
	reset := fs.Bool("reset-cost-series", false, "delete claude_hist_cost_usd and claude_hist_daily_cost_usd before importing; needed only after a label-set change, since new labels make new series that the old ones would be double counted alongside")
	fs.Parse(args)

	projects, vm := *projectsRoot, *vmURL
	if projects == "" || vm == "" {
		home, _ := os.UserHomeDir()
		cfg, err := config.Load(filepath.Join(*installDir, "config.env"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "backfill:", err)
			return 1
		}
		if projects == "" {
			projects = filepath.Join(cfg.Get("PROFILE_DIR", filepath.Join(home, ".claude")), "projects")
		}
		if vm == "" {
			vm = cfg.Get("VM_URL", "http://127.0.0.1:8428")
		}
	}

	// The guard on --reset-cost-series. The flag rebuilds the series from
	// transcripts, which only works while the transcripts that produced them
	// still exist; Claude Code ages them out, so a long-lived series outlives
	// its own source and a reset silently deletes money it cannot restore.
	if *reset && !*force {
		safety, err := indexer.CheckResetSafety(vm, []indexer.ProfileRoot{{ProjectsRoot: projects}})
		if err != nil {
			// Fails open: this guards a known hazard, it is not an authority
			// on whether the store is reachable.
			fmt.Fprintln(os.Stderr, "backfill: could not check reset safety:", err)
		} else if !safety.Safe() {
			fmt.Fprintln(os.Stderr, "backfill:", safety.Explain())
			return 1
		}
	}

	stats, err := indexer.RunBackfill(indexer.BackfillOpts{
		ProjectsRoot: projects, VMURL: vm, DryRun: *dryRun, ResetCostSeries: *reset,
		Profile: *profile,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "backfill:", err)
		return 1
	}

	fmt.Printf("parsed %d requests, computed total $%.2f (%d cost samples, %d token samples, %d daily cost samples, %d daily token samples)\n",
		stats.Requests, stats.Total, stats.CostSamples, stats.TokenSamples, stats.DailyCostSamples, stats.DailyTokenSamples)
	if stats.Reset {
		fmt.Println("deleted claude_hist_cost_usd and claude_hist_daily_cost_usd before importing")
	}
	if stats.Reconciled {
		fmt.Printf("VM raw total $%.2f vs computed $%.2f\n", stats.VMTotal, stats.Total)
		if stats.Warning {
			fmt.Println("WARNING: VM total differs from computed total by more than 1% (pre-existing history or a partial import)")
		}
	}
	return 0
}
