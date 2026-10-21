package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"charm.land/lipgloss/v2"
	"github.com/heapsgoodstudio/agent-observability/internal/config"
	"github.com/heapsgoodstudio/agent-observability/internal/installer"
)

var (
	okStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	failStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
)

func cmdDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	installDir := fs.String("install-dir", config.DefaultDir(), "install root to check")
	fs.Parse(args)

	cfg, err := config.Load(filepath.Join(*installDir, "config.env"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "no config.env: nothing installed here?")
		return 1
	}

	checks := installer.Doctor(installer.RealExec{}, cfg)
	exit := 0
	for _, c := range checks {
		line := fmt.Sprintf("%s (%s)", c.Name, c.Detail)
		if c.OK {
			lipgloss.Println(okStyle.Render("ok " + line))
			continue
		}
		lipgloss.Println(failStyle.Render("FAIL " + line))
		if c.Name != "notifier" {
			exit = 1
		}
	}
	return exit
}
