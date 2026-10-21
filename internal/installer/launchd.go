package installer

import (
	"embed"
	"fmt"
	"strings"
	"time"
)

//go:embed templates/*.plist.tmpl
var templates embed.FS

// Agents in bootstrap order: the stores come up before anything that talks
// to them. Analysis has no agent of its own; the server agent runs it
// in-process.
var Agents = []string{
	"victoriametrics", "victorialogs", "server",
	"limits-poller", "usage-alerts", "session-indexer",
}

func RenderPlist(agent string, vars map[string]string) ([]byte, error) {
	body, err := templates.ReadFile("templates/" + agent + ".plist.tmpl")
	if err != nil {
		return nil, fmt.Errorf("unknown agent %q: %w", agent, err)
	}
	s := string(body)
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{{"+k+"}}", v)
	}
	if strings.Contains(s, "{{") {
		return nil, fmt.Errorf("agent %s: unresolved placeholder", agent)
	}
	return []byte(s), nil
}

func Bootout(exec Exec, uid int, label string) {
	exec.Run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", uid, label))
}

func Bootstrap(exec Exec, uid int, plistPath string) error {
	out, err := exec.Run("launchctl", "bootstrap", fmt.Sprintf("gui/%d", uid), plistPath)
	if err != nil {
		return fmt.Errorf("bootstrap %s: %v: %s", plistPath, err, out)
	}
	return nil
}

func Loaded(exec Exec, label string) bool {
	_, err := exec.Run("launchctl", "list", label)
	return err == nil
}

// launchdSleep is time.Sleep behind a var so tests run the retry loops
// without real waits.
var launchdSleep = time.Sleep

// BootstrapFresh replaces any prior instance of a label with the plist at
// plistPath. Booting out a running KeepAlive job is asynchronous: an
// immediate bootstrap races the teardown and launchd answers with exit 5
// ("Input/output error"), which is exactly how a rerun over a live install
// failed once.
// So: boot out, poll until launchd actually drops the label (bounded), then
// bootstrap with a short bounded retry, surfacing the real error if it
// still fails.
func BootstrapFresh(exec Exec, uid int, label, plistPath string) error {
	Bootout(exec, uid, label)
	for i := 0; i < 20 && Loaded(exec, label); i++ {
		launchdSleep(250 * time.Millisecond)
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = Bootstrap(exec, uid, plistPath); err == nil {
			return nil
		}
		launchdSleep(500 * time.Millisecond)
	}
	return err
}
