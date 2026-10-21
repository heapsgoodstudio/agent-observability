package installer

import (
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"os"
	"path/filepath"
)

const lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

// UninstallSteps takes out the agents, their plists, the notifier bundle, and
// the telemetry/statusline wiring in settings.json. The install dir itself
// stays: state/ holds the stores' data, and purge is what clears that.
func UninstallSteps(installDir string, profileDirs []string, notifierApp string, uid int, purge bool) []Step {
	home, _ := os.UserHomeDir()
	steps := []Step{
		{Desc: "stop agents and remove plists (" + LabelPrefix + ".*)", Run: func(x Exec, log func(string)) error {
			for _, a := range Agents {
				Bootout(x, uid, LabelPrefix+"."+a)
				os.Remove(filepath.Join(home, "Library", "LaunchAgents", LabelPrefix+"."+a+".plist"))
			}
			return nil
		}},
		{Desc: "remove notifier bundle", Run: func(x Exec, log func(string)) error {
			if notifierApp == "" {
				return nil
			}
			if _, err := os.Stat(notifierApp); err != nil {
				return nil
			}
			x.Run(lsregister, "-u", notifierApp)
			if err := os.RemoveAll(notifierApp); err != nil {
				return err
			}
			log("its System Settings notification entry stays; remove it by hand")
			return nil
		}},
		{Desc: "disconnect owned profile telemetry and statusline wiring", Run: func(x Exec, log func(string)) error {
			return profiles.New(installDir).DisconnectAll()
		}},
	}
	if purge {
		steps = append(steps, Step{Desc: "purge store data and binaries", Run: func(x Exec, log func(string)) error {
			for _, p := range []string{
				filepath.Join(installDir, "state", "vmdata"),
				filepath.Join(installDir, "state", "vldata"),
				filepath.Join(installDir, "bin", "victoria-metrics-prod"),
				filepath.Join(installDir, "bin", "victoria-logs-prod"),
			} {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
			}
			return nil
		}})
	}
	return steps
}
