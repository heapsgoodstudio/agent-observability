// Package config reads the machine-local config.env. The shell scripts under
// bin/ source the same file directly, so the parsing here has to agree with
// what bash would do: quoted values, comments, and blank lines.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Dir    string
	values map[string]string
}

func DefaultDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agent-observability")
}

// CheckInstallDir refuses an install root that is really this project's own
// source checkout.
//
// Everything under an install root is written state: config.env, the sqlite
// db, dashboard/data, and the store data the two launchd agents keep. The
// scripts in bin/ compound this, because each resolves its own root as the
// parent of its directory. That is correct for the copy `claude-obs install`
// places in the install dir and wrong for the copy that stays in the
// checkout, so pointing a writer at the checkout does not just write once,
// it turns the repository into an install root that keeps accumulating.
//
// go.mod tells the two apart. It is tracked in the checkout, and the
// installer never copies it: stepCopy takes bin/, notifier/ and
// config.env.example, and stepLayout makes only empty directories.
func CheckInstallDir(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return nil
	}
	return fmt.Errorf("refusing to use the source checkout as an install root: %s holds go.mod. "+
		"Install roots are created by `claude-obs install` (default %s); writing state here would "+
		"leave databases and store data inside the repository", dir, DefaultDir())
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	home, _ := os.UserHomeDir()
	c := &Config{Dir: filepath.Dir(path), values: map[string]string{}}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, val, _ := strings.Cut(line, "=")
		if i := strings.Index(val, " #"); i >= 0 {
			val = val[:i]
		}
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"`)
		val = strings.Trim(val, `'`)
		if strings.Contains(val, "$(") {
			continue
		}
		val = strings.ReplaceAll(val, "${HOME}", home)
		val = strings.ReplaceAll(val, "$HOME", home)
		c.values[strings.TrimSpace(key)] = val
	}
	return c, nil
}

// Lookup distinguishes an explicitly empty setting from an absent one.
func (c *Config) Lookup(key string) (string, bool) {
	v, ok := c.values[key]
	return v, ok
}

func (c *Config) Get(key, def string) string {
	if v, ok := c.values[key]; ok {
		return v
	}
	return def
}

func (c *Config) Path(parts ...string) string {
	return filepath.Join(append([]string{c.Dir}, parts...)...)
}
