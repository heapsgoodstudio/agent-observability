package installer

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/config"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

const LabelPrefix = "com.agent-observability"

type Inputs struct {
	InstallDir, ProfileDir, ClaudeBin, SelfBin string
	// Assets holds the bin/, notifier/ and config.env.example that stepCopy
	// places in the install dir: agentobservability.Assets, embedded in the
	// binary, so installing needs no source checkout.
	Assets fs.FS
	// ProfileDirs is every profile to instrument. ProfileDir remains the
	// primary one: it is what the flat, pre-multi-account dashboard fields
	// describe and what an install that predates this field still records.
	// Empty means just ProfileDir, so nothing has to set both.
	Home               string
	ExplicitProfiles   bool
	ProfileDirs        []string
	Backfill, SwiftcOK bool
	UID                int
}

// Profiles is every profile this install instruments, primary first and with
// duplicates removed. One accessor rather than two fields read everywhere,
// so no step can accidentally wire only the primary.
func (in Inputs) Profiles() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimRight(p, "/")
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	add(in.ProfileDir)
	for _, p := range in.ProfileDirs {
		add(p)
	}
	return out
}

type Step struct {
	Desc string
	Run  func(x Exec, log func(string)) error
}

func Describe(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Desc
	}
	return out
}

func Execute(steps []Step, x Exec, log func(string)) error {
	for _, s := range steps {
		log("==> " + s.Desc)
		if err := s.Run(x, log); err != nil {
			return fmt.Errorf("%s: %w", s.Desc, err)
		}
	}
	return nil
}

// CopyTree copies every file in src into dst, leaving out any path skip
// names. Embedded files carry no mode, so the name decides it: a ".sh" file
// is executable and everything else is 0644.
func CopyTree(src fs.FS, dst string, skip func(rel string) bool) error {
	return fs.WalkDir(src, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, rel)
		if err != nil {
			return err
		}
		perm := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			perm = 0o755
		}
		return writeFileAtomic(target, data, perm)
	})
}

// writeFileAtomic writes data to path through a temp file in the same
// directory, then renames it into place.
//
// Replacing a file this way matters on every re-install, which is the upgrade
// path. os.WriteFile truncates in place and keeps the inode, and for the
// claude-obs binary that invalidates the code signature of an image the
// running server agent still has mapped: the write succeeds and the *next*
// exec dies with SIGKILL, exit 137 and no output. Verified by overwriting a
// running copy in place. The same hazard in a milder form applies to the
// shell scripts this copies, since bash reads a script incrementally and the
// poller and alert agents may be mid-run when an install truncates one.
//
// Rename rather than remove-then-write: it is atomic, so launchd never sees a
// window in which bin/claude-obs or a script is missing and restarts an agent
// into it. The running process keeps its old inode until it exits.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.new")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

func BuildPlan(in Inputs) []Step {
	var steps []Step
	steps = append(steps,
		stepLayout(in), stepCopy(in), stepProfileRegistry(in), stepConfigEnv(in), stepStores(in),
		stepSqlite(in),
	)
	if in.Backfill {
		steps = append(steps, stepBackfill(in))
	}
	steps = append(steps, stepAgents(in), stepNotifier(in), stepVerify(in))
	return steps
}

func stepProfileRegistry(in Inputs) Step {
	return Step{Desc: "profile registry and telemetry/statusline wiring", Run: func(x Exec, log func(string)) error {
		home := in.Home
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		var explicit []string
		if in.ExplicitProfiles {
			explicit = in.Profiles()
			if explicit == nil {
				explicit = []string{}
			}
		}
		r, err := profiles.New(in.InstallDir).Initialize(home, explicit)
		if err != nil {
			return err
		}
		for _, p := range r.Profiles {
			if p.WiringError != "" {
				log(p.Name + ": " + p.WiringError)
			}
		}
		if len(r.Profiles) == 0 {
			log("No default profiles found. Add profiles in dashboard Settings when ready.")
		}
		return nil
	}}
}

func stepLayout(in Inputs) Step {
	return Step{Desc: "create layout under " + in.InstallDir, Run: func(x Exec, log func(string)) error {
		for _, d := range []string{
			"bin", "dashboard", "notifier", "state", "logs",
			"state/vmdata", "state/vldata",
			"dashboard/data/sessions", "dashboard/data/analyses", "dashboard/data/advice",
		} {
			if err := os.MkdirAll(filepath.Join(in.InstallDir, d), 0o755); err != nil {
				return err
			}
		}
		return nil
	}}
}

func stepCopy(in Inputs) Step {
	return Step{Desc: "copy bin, notifier, and config.env.example", Run: func(x Exec, log func(string)) error {
		for _, dir := range []string{"bin", "notifier"} {
			sub, err := fs.Sub(in.Assets, dir)
			if err != nil {
				return err
			}
			if err := CopyTree(sub, filepath.Join(in.InstallDir, dir), nil); err != nil {
				return err
			}
		}
		example, err := fs.ReadFile(in.Assets, "config.env.example")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(in.InstallDir, "config.env.example"), example, 0o644); err != nil {
			return err
		}
		dst := filepath.Join(in.InstallDir, "bin", "claude-obs")
		if filepath.Clean(in.SelfBin) == filepath.Clean(dst) {
			log("claude-obs already in place; skipping self copy")
			return nil
		}
		self, err := os.ReadFile(in.SelfBin)
		if err != nil {
			return err
		}
		// Atomic replace, not os.WriteFile: see writeFileAtomic. On a live
		// machine this is always replacing the binary the server agent is
		// running, and truncating it in place ships a copy macOS SIGKILLs.
		return writeFileAtomic(dst, self, 0o755)
	}}
}

func stepConfigEnv(in Inputs) Step {
	return Step{Desc: "write config.env", Run: func(x Exec, log func(string)) error {
		example, err := os.ReadFile(filepath.Join(in.InstallDir, "config.env.example"))
		if err != nil {
			return err
		}
		return profiles.New(in.InstallDir).UpdateConfig(func(existing []byte) ([]byte, error) {
			merged, kept, extra := MergeConfigEnv(example, existing)
			for _, k := range kept {
				log("keeping this install's " + k)
			}
			if len(extra) > 0 {
				log("keeping keys not in config.env.example: " + strings.Join(extra, ", "))
			}
			block := fmt.Sprintf("\n# Machine-local values written by claude-obs install on %s\n"+
				"PROFILE_DIR=%q\nPROFILE_DIRS=%q\nCLAUDE_BIN=%q\nLABEL_PREFIX=%q\n",
				time.Now().Format("2006-01-02"), in.ProfileDir,
				strings.Join(in.Profiles(), ":"), in.ClaudeBin, LabelPrefix)
			return append(merged, []byte(block)...), nil
		})
	}}
}

func stepStores(in Inputs) Step {
	return Step{Desc: "fetch store binaries", Run: func(x Exec, log func(string)) error {
		stateBin := filepath.Join(in.InstallDir, "state", "bin")
		if err := ensureBinSymlink(stateBin, "../bin"); err != nil {
			return err
		}
		binDir := filepath.Join(in.InstallDir, "bin")
		if execOK(filepath.Join(binDir, "victoria-metrics-prod")) && execOK(filepath.Join(binDir, "victoria-logs-prod")) {
			log("store binaries already present; skipping fetch")
			return nil
		}
		_, err := x.Run("/bin/bash", filepath.Join(in.InstallDir, "bin", "fetch-stores.sh"))
		return err
	}}
}

// ensureBinSymlink makes path a symlink to target, replacing whatever else
// is there; a link already in place is left alone.
func ensureBinSymlink(path, target string) error {
	fi, err := os.Lstat(path)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if err == nil {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(target, path)
}

func execOK(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode()&0o111 != 0
}

func stepSqlite(in Inputs) Step {
	return Step{Desc: "sqlite: open state/claude-obs.db and migrate", Run: func(x Exec, log func(string)) error {
		d, err := db.Open(filepath.Join(in.InstallDir, "state", "claude-obs.db"))
		if err != nil {
			return err
		}
		defer d.Close()
		n, err := db.Migrate(d)
		if err != nil {
			return err
		}
		log(fmt.Sprintf("%d migration(s) applied", n))
		return nil
	}}
}

func stepTelemetry(in Inputs) Step {
	return Step{Desc: "telemetry wiring in settings.json", Run: func(x Exec, log func(string)) error {
		for _, dir := range in.Profiles() {
			if err := wireTelemetry(in, dir, log); err != nil {
				return err
			}
		}
		return nil
	}}
}

func wireTelemetry(in Inputs, profileDir string, log func(string)) error {
	{
		path := filepath.Join(profileDir, "settings.json")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
				return err
			}
		}
		cur, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name := ProfileName(profileDir)
		out, changed, conflicts, err := MergeTelemetry(cur, TelemetryEnv(name))
		if err != nil {
			return err
		}
		if len(conflicts) > 0 {
			return fmt.Errorf("%s/settings.json already sets different values for: %s. Resolve manually, then rerun.", profileDir, strings.Join(conflicts, ", "))
		}
		if !changed {
			log("telemetry env already present for " + name + "; skipping")
			return nil
		}
		if _, err := backup(path); err != nil {
			return err
		}
		log("telemetry env written for " + name)
		return os.WriteFile(path, out, 0o644)
	}
}

func stepStatusline(in Inputs) Step {
	return Step{Desc: "statusline wiring in settings.json", Run: func(x Exec, log func(string)) error {
		for _, dir := range in.Profiles() {
			if err := wireStatusline(in, dir, log); err != nil {
				return err
			}
		}
		return nil
	}}
}

func wireStatusline(in Inputs, profileDir string, log func(string)) error {
	{
		path := filepath.Join(profileDir, "settings.json")
		cur, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		cmd := filepath.Join(in.InstallDir, "bin", "statusline.sh")
		out, existing, err := SetStatusline(cur, cmd)
		if err != nil {
			return err
		}
		// A re-install always finds a statusLine already set, because the
		// previous install set it. Recognising our own entry keeps an upgrade
		// from printing the "integrate it yourself" advice about a script
		// that is already wired up. Only an exact match counts as ours: a
		// command merely ending in statusline.sh could be someone else's, and
		// the cost of guessing wrong is overwriting their statusline.
		name := ProfileName(profileDir)
		if existing == cmd {
			log("statusline already wired to this install for " + name + "; leaving it alone")
			return nil
		}
		if existing != "" {
			log("existing statusLine found for " + name + "; leaving it alone. To integrate, add these two lines to your script:")
			log(fmt.Sprintf(`  printf %%s "$input" | "%s/bin/statusline-tap.sh" 2>/dev/null &`, in.InstallDir))
			// Per profile, because the poller writes one segment per profile
			// now. The tap needs no argument: it reads CLAUDE_CONFIG_DIR,
			// which Claude Code exports to whatever the statusline forks.
			log(fmt.Sprintf(`  seg=$(cat "%s/state/statusline-segment.%s.txt" 2>/dev/null); [ -n "$seg" ] && line+=" $seg"`, in.InstallDir, name))
			return nil
		}
		if _, err := backup(path); err != nil {
			return err
		}
		log("statusline wired for " + name)
		return os.WriteFile(path, out, 0o644)
	}
}

// backup copies path aside before a rewrite. UnixNano, not Unix: two steps
// back up the same file within one second on a first install, so a
// whole-second suffix collides.
func backup(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	bak := fmt.Sprintf("%s.bak.%d", path, time.Now().UnixNano())
	return bak, os.WriteFile(bak, data, 0o644)
}

func stepAgents(in Inputs) Step {
	return Step{Desc: "render and bootstrap launchd agents", Run: func(x Exec, log func(string)) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		for _, agent := range Agents {
			if err := bootstrapAgent(x, in, home, agent); err != nil {
				return err
			}
		}
		return nil
	}}
}

// bootstrapAgent boots out any prior instance of one agent, renders its
// plist, and bootstraps the new one.
func bootstrapAgent(x Exec, in Inputs, home, agent string) error {
	label := LabelPrefix + "." + agent
	vars := map[string]string{
		"OBS_DIR":      in.InstallDir,
		"LABEL_PREFIX": LabelPrefix,
		"CLAUDE_OBS":   filepath.Join(in.InstallDir, "bin", "claude-obs"),
	}
	body, err := RenderPlist(agent, vars)
	if err != nil {
		return err
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	if err := os.WriteFile(plistPath, body, 0o644); err != nil {
		return err
	}
	if _, err := x.Run("plutil", "-lint", plistPath); err != nil {
		return fmt.Errorf("plutil -lint %s: %w", plistPath, err)
	}
	return BootstrapFresh(x, in.UID, label, plistPath)
}

func stepNotifier(in Inputs) Step {
	return Step{Desc: "build and register the notifier", Run: func(x Exec, log func(string)) error {
		if !in.SwiftcOK {
			log("swiftc not found (install the Xcode Command Line Tools); skipping the notifier.")
			return nil
		}
		if out, err := x.Run("/bin/bash", filepath.Join(in.InstallDir, "notifier", "build.sh")); err != nil {
			// The script's own stderr is the diagnosis (a bare exit status
			// once hid a poisoned-SDK failure from the person installing).
			return fmt.Errorf("notifier build.sh: %w\n%s", err, strings.TrimSpace(out))
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		cfg, err := config.Load(filepath.Join(in.InstallDir, "config.env"))
		if err != nil {
			return err
		}
		notifierApp := cfg.Get("NOTIFIER_APP", "")
		if notifierApp == "" {
			return nil
		}
		bin := filepath.Join(notifierApp, "Contents", "MacOS", "AgentObsNotifier")
		if _, err := x.Run(bin, "--request"); err != nil {
			log("permission not granted; alerts will fall back to plain banners.")
			return nil
		}
		x.Run(bin, "Agent Observability", "Installed. Click this banner to open the dashboard.", "http://localhost:8427/#now")

		// An untouched default notifier path is migrated by MergeConfigEnv.
		// Remove the previous bundle only after the newly named one has built,
		// registered, and successfully handled the permission request.
		legacyApp := filepath.Join(home, "Applications", "Claude Observability.app")
		defaultApp := filepath.Join(home, "Applications", "Agent Observability.app")
		if notifierApp == defaultApp {
			if _, err := os.Stat(legacyApp); err == nil {
				x.Run(lsregister, "-u", legacyApp)
				if err := os.RemoveAll(legacyApp); err != nil {
					return err
				}
				log("removed the notifier bundle at its previous default path")
			}
		}
		return nil
	}}
}

func stepBackfill(in Inputs) Step {
	return Step{Desc: "queue background history backfill", Run: func(x Exec, log func(string)) error {
		if err := os.MkdirAll(filepath.Join(in.InstallDir, "state"), 0o755); err != nil {
			return err
		}
		return writeFileAtomic(filepath.Join(in.InstallDir, "state", "import-request.json"), []byte("{\"profile\":\"\"}\n"), 0o600)
	}}
}

// indexVerifyCmd is the one indexer pass stepVerify runs after the endpoint
// and agent checks pass. It is broken out so the exact invocation is
// unit-testable without standing up the live HTTP endpoints and launchd
// agents that precede it in stepVerify. --install-dir is explicit for the
// same reason stepBackfill passes it: claude-obs index would otherwise
// resolve config.env via config.DefaultDir(), which may not be this
// install's InstallDir.
func indexVerifyCmd(installDir string) (string, []string) {
	return filepath.Join(installDir, "bin", "claude-obs"), []string{"index", "--once", "--install-dir", installDir}
}

func stepVerify(in Inputs) Step {
	return Step{Desc: "verify endpoints and agents (history indexes in the background)", Run: func(x Exec, log func(string)) error {
		urls := []string{
			"http://127.0.0.1:8428/health", "http://127.0.0.1:9428/health",
			"http://127.0.0.1:8427/",
		}
		client := &http.Client{Timeout: 3 * time.Second}
		for _, u := range urls {
			var ok bool
			for i := 0; i < 10; i++ {
				if resp, err := client.Get(u); err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode < 400 {
						ok = true
						break
					}
				}
				time.Sleep(time.Second)
			}
			if !ok {
				return fmt.Errorf("endpoint not healthy: %s", u)
			}
		}
		for _, a := range Agents {
			if !Loaded(x, LabelPrefix+"."+a) {
				return fmt.Errorf("agent not loaded: %s.%s", LabelPrefix, a)
			}
		}
		log("The session indexer is running in the background. Import progress is available in Settings.")
		return nil
	}}
}
