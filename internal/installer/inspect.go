package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DiscoverProfiles detects only the default Claude directory. Additional
// directories are explicit Settings declarations, never environment discovery.
func DiscoverProfiles(home, claudeConfigDir, installDir string) []string {
	p := filepath.Join(home, ".claude")
	if filepath.Clean(p) == filepath.Clean(installDir) {
		return nil
	}
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return []string{p}
	}
	return nil
}

var refusedChars = regexp.MustCompile(`["'$` + "`" + `;|&\\()]`)

// shimPrefixes are path prefixes that hold transient binaries; a launchd
// agent pointing there breaks on the next temp cleanup. A package var, not
// a const, so tests can simulate a shim without depending on the host's
// real TMPDIR (t.TempDir() itself lives under /var/folders on macOS).
var shimPrefixes = defaultShimPrefixes()

func defaultShimPrefixes() []string {
	p := []string{"/var/folders/"}
	if t := os.Getenv("TMPDIR"); t != "" {
		p = append(p, t)
	}
	return p
}

func ValidateClaudeBin(path string, exec Exec) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("not an absolute path: %s", path)
	}
	if refusedChars.MatchString(path) {
		return fmt.Errorf("contains characters this installer refuses: %s", path)
	}
	for _, pre := range shimPrefixes {
		if strings.HasPrefix(path, pre) {
			return fmt.Errorf("a temporary shim, not the real binary: %s", path)
		}
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode()&0o111 == 0 {
		return fmt.Errorf("not executable: %s", path)
	}
	if _, err := exec.Run(path, "--version"); err != nil {
		return fmt.Errorf("failed --version: %s", path)
	}
	return nil
}

// ValidateSafePath rejects paths that are unsafe to write into config.env:
// that file is sourced by bash, so %q quoting alone is not a defense.
func ValidateSafePath(name, value string) error {
	if !strings.HasPrefix(value, "/") {
		return fmt.Errorf("%s must be an absolute path: %s", name, value)
	}
	if refusedChars.MatchString(value) {
		return fmt.Errorf("%s contains characters this installer refuses: %s", name, value)
	}
	return nil
}

func PortState(exec Exec, port int, ourLabel string) string {
	if _, err := exec.Run("lsof", "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN"); err != nil {
		return "free"
	}
	if _, err := exec.Run("launchctl", "list", ourLabel); err == nil {
		return "ours"
	}
	return "busy"
}

type Report struct {
	SwiftcOK  bool
	Ports     map[int]string
	Profiles  []string
	Installed bool
}

func Inspect(exec Exec, home, claudeConfigDir, installDir string) Report {
	r := Report{Ports: map[int]string{}}
	_, err := exec.Run("xcrun", "--find", "swiftc")
	r.SwiftcOK = err == nil
	for port, agent := range map[int]string{
		8427: "server", 8428: "victoriametrics", 9428: "victorialogs",
	} {
		r.Ports[port] = PortState(exec, port, LabelPrefix+"."+agent)
	}
	r.Profiles = DiscoverProfiles(home, claudeConfigDir, installDir)
	if _, err := os.Stat(filepath.Join(installDir, "config.env")); err == nil {
		r.Installed = true
	}
	return r
}
