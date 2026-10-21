package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/heapsgoodstudio/agent-observability/internal/config"
	"github.com/heapsgoodstudio/agent-observability/internal/indexer"
)

// cmdRelabel adds a label to the existing cost history in place.
//
// It exists because the repository's other answer to a label-set change,
// `backfill --reset-cost-series`, rebuilds the series from transcripts, and
// transcripts age out. Once the history outlives its own source, a reset
// deletes money it cannot put back. This moves the samples through
// VictoriaMetrics' export and import instead: values round-trip exactly and
// no transcript is consulted.
func cmdRelabel(args []string) int {
	fs := flag.NewFlagSet("migrate-history-labels", flag.ExitOnError)
	installDir := fs.String("install-dir", config.DefaultDir(), "install root (holds config.env)")
	vmURL := fs.String("vm-url", "", "VictoriaMetrics base URL (default: config.env's VM_URL)")
	label := fs.String("label", "profile", "label to add to every historical cost series")
	value := fs.String("value", "", "value for that label; required")
	dryRun := fs.Bool("dry-run", false, "export and rewrite, but neither delete nor import")
	// Without this there is no way to rehearse against anything but the real
	// cost series, and a rehearsal that has to name the real series is not a
	// rehearsal. Its absence is how a "rehearsal" ran against live history.
	metrics := fs.String("metrics", "", "comma-separated metrics to relabel (default: the two cost series)")
	backupDir := fs.String("backup-dir", "", "where to write the pre-delete dump (default: <install-dir>/backups)")
	fs.Parse(args)

	if *value == "" {
		fmt.Fprintln(os.Stderr, "migrate-history-labels: --value is required (the profile every existing sample belongs to)")
		return 2
	}

	vm := *vmURL
	if vm == "" {
		cfg, err := config.Load(filepath.Join(*installDir, "config.env"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "migrate-history-labels:", err)
			return 1
		}
		vm = cfg.Get("VM_URL", "http://127.0.0.1:8428")
	}

	dumps := *backupDir
	if dumps == "" {
		dumps = filepath.Join(*installDir, "backups")
	}

	var only []string
	if *metrics != "" {
		for _, m := range strings.Split(*metrics, ",") {
			if m = strings.TrimSpace(m); m != "" {
				only = append(only, m)
			}
		}
	}

	stats, err := indexer.RelabelHistory(indexer.RelabelOpts{
		VMURL: vm, Label: *label, Value: *value, DryRun: *dryRun,
		Metrics: only, BackupDir: dumps,
	})
	// Stats are printed even on failure: a partial run's numbers are the
	// first thing anyone recovering from one needs.
	for _, st := range stats {
		verb := "relabelled"
		if st.DryRun {
			verb = "would relabel"
		}
		fmt.Printf("%s %s: %d series, %d samples, $%.2f before, $%.2f after",
			verb, st.Metric, st.Series, st.Samples, st.Before, st.After)
		if st.Skipped > 0 {
			fmt.Printf(" (%d series already carried %s)", st.Skipped, *label)
		}
		fmt.Println()
		if st.Backup != "" {
			fmt.Printf("  pre-delete dump: %s\n", st.Backup)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-history-labels:", err)
		return 1
	}
	if *dryRun {
		fmt.Println("dry run: nothing was deleted or imported")
	}
	return 0
}
