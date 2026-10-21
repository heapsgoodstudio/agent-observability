package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/config"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/indexer"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
)

func cmdIndex(args []string) int {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	installDir := fs.String("install-dir", config.DefaultDir(), "install root (holds config.env, dashboard/, state/)")
	projectsRoot := fs.String("projects-root", "", "Claude Code projects dir to scan (default: config.env's PROFILE_DIR/projects)")
	out := fs.String("out", "", "dashboard data dir to write (default <install-dir>/dashboard/data)")
	dbPath := fs.String("db", "", "sqlite db path (default <install-dir>/state/claude-obs.db)")
	once := fs.Bool("once", false, "run a single indexing pass and exit")
	fs.Parse(args)

	if !*once {
		fmt.Fprintln(os.Stderr, "usage: claude-obs index --once [--install-dir D | --projects-root P --out O --db F]")
		return 2
	}

	projects, outRoot, statePath := *projectsRoot, *out, *dbPath
	var codexBin string
	if projects == "" || outRoot == "" || statePath == "" {
		// Only when --install-dir is what fills the blanks. Passing all three
		// of --projects-root, --out and --db explicitly stays an escape hatch
		// for dev runs writing wherever they were told to.
		if err := config.CheckInstallDir(*installDir); err != nil {
			fmt.Fprintln(os.Stderr, "index:", err)
			return 1
		}
		home, _ := os.UserHomeDir()
		cfg, err := config.Load(filepath.Join(*installDir, "config.env"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "index:", err)
			return 1
		}
		if projects == "" {
			projects = filepath.Join(cfg.Get("PROFILE_DIR", filepath.Join(home, ".claude")), "projects")
		}
		codexBin = filepath.Join(home, ".local", "bin", "codex")
		if _, err := os.Stat(codexBin); err != nil {
			if found, err := exec.LookPath("codex"); err == nil {
				codexBin = found
			}
		}
		codexBin = cfg.Get("CODEX_BIN", codexBin)
		if outRoot == "" {
			outRoot = filepath.Join(*installDir, "dashboard", "data")
		}
		if statePath == "" {
			statePath = filepath.Join(*installDir, "state", "claude-obs.db")
		}
	}

	d, err := db.Open(statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "index: open db:", err)
		return 1
	}
	defer d.Close()
	if _, err := db.Migrate(d); err != nil {
		fmt.Fprintln(os.Stderr, "index: migrate:", err)
		return 1
	}

	if *projectsRoot == "" {
		m := monitor.New(*installDir, d)
		m.CodexBinary = codexBin
		m.OutRoot = outRoot
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		n, err := m.RefreshScheduled(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "index:", err)
			return 1
		}
		fmt.Printf("indexed %d sessions\n", n)
		return 0
	}
	stats, err := indexer.RunIndex(indexer.IndexOpts{
		ProjectsRoot: projects, OutRoot: outRoot, DB: d,
		Now: func() int64 { return time.Now().Unix() },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "index:", err)
		return 1
	}

	msg := fmt.Sprintf("indexed %d sessions, re-parsed %d", stats.Sessions, stats.Parsed)
	if len(stats.Unpriced) > 0 {
		msg += fmt.Sprintf(", WARNING unpriced models: %s", strings.Join(stats.Unpriced, ", "))
	}
	fmt.Println(msg)
	return 0
}
