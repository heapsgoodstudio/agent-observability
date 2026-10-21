// Package profiles owns the durable roster and reversible agent configuration.
// Readers reopen profiles.json so Settings changes apply without a restart.
package profiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"regexp"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/heapsgoodstudio/agent-observability/internal/config"
)

type Provider string

const (
	Claude Provider = "claude"
	Codex  Provider = "codex"
	// Omp and Opencode profiles are read-only: each harness records usage in
	// its own data (Oh My Pi's session logs, opencode's sqlite database), so
	// nothing is written into its directory and there is nothing to wire.
	Omp      Provider = "omp"
	Opencode Provider = "opencode"
)

// Detected reports whether a provider is a harness whose accounts are the
// inference providers detected in its data rather than a sign-in. Such a
// profile has no name of its own and is never primary; see normalizeDetected.
func Detected(p Provider) bool { return p == Omp || p == Opencode }

// ProviderName is a provider's fixed display name. The frontend's
// providerName map in frontend/src/lib/providers.ts spells the same names and
// must agree.
func ProviderName(p Provider) string {
	switch p {
	case Claude:
		return "Claude"
	case Codex:
		return "Codex"
	case Omp:
		return "Oh My Pi"
	case Opencode:
		return "opencode"
	}
	return string(p)
}

type Profile struct {
	ID           string     `json:"id"`
	Provider     Provider   `json:"provider"`
	Name         string     `json:"name"`
	Directory    string     `json:"directory"`
	Enabled      bool       `json:"enabled"`
	Primary      bool       `json:"primary"`
	WiringStatus string     `json:"wiring_status"`
	WiringError  string     `json:"wiring_error,omitempty"`
	Owned        *Ownership `json:"owned,omitempty"`
}

// Ownership contains only values written by this installation. An operator's
// subsequent edit is never removed or overwritten by disconnect or repair.
type Ownership struct {
	Path       string            `json:"path"`
	Env        map[string]string `json:"env,omitempty"`
	Statusline string            `json:"statusline,omitempty"`
	CodexBlock string            `json:"codex_block,omitempty"`
	Legacy     bool              `json:"legacy,omitempty"`
}

type Registry struct {
	Version      int       `json:"version"`
	Profiles     []Profile `json:"profiles"`
	Disconnected []Profile `json:"disconnected,omitempty"`
	// HiddenAccounts are detected-provider account keys ("opencode:wire") a
	// person chose not to see in the sidebar and Overview. Hiding deletes
	// nothing, collection continues, and the account's own page still opens.
	HiddenAccounts []string `json:"hidden_accounts,omitempty"`
}
type Store struct{ Dir string }

func New(installDir string) *Store { return &Store{Dir: installDir} }

// LegacyName is the historical Claude telemetry label, never a display name.
func LegacyName(dir string) string {
	n := strings.TrimPrefix(strings.TrimPrefix(filepath.Base(dir), ".claude-"), ".claude")
	if n == "" {
		return "default"
	}
	return n
}

func (s *Store) Load() (Registry, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir, "profiles.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s.legacy()
	}
	if err != nil {
		return Registry{}, err
	}
	r := Registry{}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("profile registry: %w", err)
	}
	if r.Version != 1 {
		return r, fmt.Errorf("unsupported profile registry version %d", r.Version)
	}
	if r.Profiles == nil {
		r.Profiles = []Profile{}
	}
	normalizeDetected(&r)
	return r, nil
}

// normalizeDetected derives the name of every detected-provider profile
// (Oh My Pi, opencode) and clears its primary flag. Such a profile has nothing
// a person needs to name: the harness is fixed and its accounts are the
// inference providers detected in its data. So it is called by the harness,
// with its directory added only when a second directory of the same harness
// would make that ambiguous, and it is never primary, since primary picks the
// profile that runs provider actions and these harnesses have none. Load
// applies it, so a name saved before this rule reads correctly without a
// migration.
func normalizeDetected(r *Registry) {
	count := map[Provider]int{}
	for _, p := range r.Profiles {
		if Detected(p.Provider) {
			count[p.Provider]++
		}
	}
	for i := range r.Profiles {
		p := &r.Profiles[i]
		if !Detected(p.Provider) {
			continue
		}
		p.Primary = false
		p.Name = ProviderName(p.Provider)
		if count[p.Provider] > 1 {
			p.Name += " (" + filepath.Join(filepath.Base(filepath.Dir(p.Directory)), filepath.Base(p.Directory)) + ")"
		}
	}
}
// SetAccountHidden hides or shows one detected-provider account. Only an Oh
// My Pi or opencode account can be hidden: Claude and Codex accounts are a
// person's own sign-ins, never something detected on their behalf.
func (s *Store) SetAccountHidden(key string, hidden bool) error {
	harness, inference, ok := strings.Cut(key, ":")
	if !ok || !Detected(Provider(harness)) || inference == "" || strings.Contains(inference, ":") {
		return fmt.Errorf("only an Oh My Pi or opencode account can be hidden")
	}
	return s.locked(func() error {
		r, err := s.Load()
		if err != nil {
			return err
		}
		keep := make([]string, 0, len(r.HiddenAccounts)+1)
		for _, k := range r.HiddenAccounts {
			if k != key {
				keep = append(keep, k)
			}
		}
		if hidden {
			keep = append(keep, key)
		}
		sort.Strings(keep)
		r.HiddenAccounts = keep
		return s.commit(r, nil)
	})
}

// HiddenAccounts returns the hidden detected-provider account keys as a set.
func (s *Store) HiddenAccounts() (map[string]bool, error) {
	r, err := s.Load()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(r.HiddenAccounts))
	for _, k := range r.HiddenAccounts {
		out[k] = true
	}
	return out, nil
}

func (s *Store) List() ([]Profile, error) {
	r, err := s.Load()
	if err != nil {
		return nil, err
	}
	for i := range r.Profiles {
		s.inspectWiring(&r.Profiles[i])
	}

	return r.Profiles, nil
}
func (s *Store) legacy() (Registry, error) {
	r := Registry{Version: 1, Profiles: []Profile{}}
	c, err := config.Load(filepath.Join(s.Dir, "config.env"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, nil
		}
		return r, err
	}
	primary := c.Get("PROFILE_DIR", "")
	dirs, present := c.Lookup("PROFILE_DIRS")
	if !present {
		dirs = primary
	}
	seen := map[string]bool{}
	for _, dir := range append([]string{primary}, strings.Split(dirs, ":")...) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		canonical, err := canonicalPath(dir)
		if err != nil {
			return r, err
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		id := LegacyName(dir)
		r.Profiles = append(r.Profiles, Profile{ID: id, Provider: Claude, Name: id, Directory: canonical, Enabled: true, Primary: len(r.Profiles) == 0, WiringStatus: "unverified", Owned: &Ownership{Legacy: true}})
	}
	return r, nil
}

// Initialize migrates explicit legacy configuration once, adding an existing
// default ~/.codex while retaining the legacy Claude roster. Fresh installs
// detect ~/.claude and ~/.codex; an existing registry is never rediscovered.
// A non-nil explicitClaudeDirs intentionally replaces the Claude roster.
func (s *Store) Initialize(home string, explicitClaudeDirs []string) (Registry, error) {
	var out Registry
	err := s.locked(func() error {
		r, err := s.Load()
		if err != nil {
			return err
		}
		_, registryErr := os.Stat(filepath.Join(s.Dir, "profiles.json"))
		_, configErr := os.Stat(filepath.Join(s.Dir, "config.env"))
		if errors.Is(registryErr, os.ErrNotExist) {
			defaults := []Provider{Codex}
			if errors.Is(configErr, os.ErrNotExist) {
				defaults = []Provider{Claude, Codex}
			}
			for _, provider := range defaults {
				dir := filepath.Join(home, "."+string(provider))
				if st, err := os.Stat(dir); err == nil && st.IsDir() {
					id := "default"
					if provider == Codex {
						id = "codex-default"
					}
					canonical, err := canonicalPath(dir)
					if err != nil {
						return err
					}
					r.Profiles = append(r.Profiles, Profile{ID: id, Provider: provider, Name: string(provider) + " default", Directory: canonical, Enabled: true, Primary: true})
				}
			}
		}
		var edits []fileEdit
		if explicitClaudeDirs != nil {
			keep := []Profile{}
			requested := map[string]bool{}
			for _, dir := range explicitClaudeDirs {
				p, err := canonicalPath(dir)
				if err != nil {
					return err
				}
				requested[p] = true
			}
			for _, p := range r.Profiles {
				if p.Provider != Claude || requested[p.Directory] {
					keep = append(keep, p)
				} else {
					e, err := s.unwire(p)
					if err != nil {
						return err
					}
					edits = append(edits, e...)
					r.Disconnected = append(r.Disconnected, disconnected(p))
				}
			}
			r.Profiles = keep
			for _, dir := range explicitClaudeDirs {
				canonical, _ := canonicalPath(dir)
				found := false
				for i := range r.Profiles {
					p := &r.Profiles[i]
					if p.Provider == Claude {
						p.Primary = false
						if p.Directory == canonical {
							found = true
							p.Enabled = true
						}
					}
				}
				if !found {
					r.Profiles = append(r.Profiles, Profile{ID: allocateID(&r, Claude, canonical), Provider: Claude, Name: LegacyName(dir), Directory: canonical, Enabled: true})
				}
			}
			if len(explicitClaudeDirs) > 0 {
				primary, _ := canonicalPath(explicitClaudeDirs[0])
				for i := range r.Profiles {
					if r.Profiles[i].Provider == Claude {
						r.Profiles[i].Primary = r.Profiles[i].Directory == primary
					}
				}
			}
		}
		normalizePrimary(&r)
		if err := validateRegistry(r); err != nil {
			return err
		}
		for i := range r.Profiles {
			e, err := s.wire(&r.Profiles[i])
			if err != nil {
				return err
			}
			edits = append(edits, e...)
		}
		if err := s.commit(r, edits); err != nil {
			return err
		}
		out = r
		return nil
	})
	return out, err
}

func (s *Store) Save(p Profile) (Profile, error) {
	var out Profile
	err := s.locked(func() error {
		r, err := s.Load()
		if err != nil {
			return err
		}
		directory, err := canonicalPath(p.Directory)
		if err != nil {
			return err
		}
		p.Directory = directory
		if p.Provider != Claude && p.Provider != Codex && !Detected(p.Provider) {
			return fmt.Errorf("provider must be claude, codex, omp or opencode")
		}
		p.Name = strings.TrimSpace(p.Name)
		if Detected(p.Provider) {
			// Derived by normalizeDetected below; whatever was sent is ignored.
			p.Name = ProviderName(p.Provider)
			p.Primary = false
		}
		if p.Name == "" {
			return fmt.Errorf("display name is required")
		}
		idx := -1
		var old Profile
		if p.ID != "" {
			for i, existing := range r.Profiles {
				if existing.ID == p.ID {
					idx = i
					old = existing
					break
				}
			}
			if idx < 0 {
				return fmt.Errorf("profile %q not found", p.ID)
			}
			if p.Provider != old.Provider {
				return fmt.Errorf("provider cannot change; disconnect and add a new profile")
			}
			p.Owned = old.Owned
		} else {
			p.Owned = nil
			p.ID = allocateID(&r, p.Provider, p.Directory)
		}

		var edits []fileEdit
		if idx >= 0 && (old.Directory != p.Directory || !p.Enabled) {
			e, err := s.unwire(old)
			if err != nil {
				return err
			}
			edits = append(edits, e...)
			p.Owned = nil
		}
		if p.Primary {
			for i := range r.Profiles {
				if r.Profiles[i].Provider == p.Provider {
					r.Profiles[i].Primary = false
				}
			}
		}
		if idx < 0 {
			idx = len(r.Profiles)
			r.Profiles = append(r.Profiles, p)
		} else {
			r.Profiles[idx] = p
		}
		normalizeDetected(&r)
		normalizePrimary(&r)
		if err := validateRegistry(r); err != nil {
			return err
		}
		e, err := s.wire(&r.Profiles[idx])
		if err != nil {
			return err
		}
		edits = append(edits, e...)
		if err := s.commit(r, edits); err != nil {
			return err
		}
		out = r.Profiles[idx]
		return nil
	})
	return out, err
}
func (s *Store) Remove(id string) error {
	return s.locked(func() error {
		r, err := s.Load()
		if err != nil {
			return err
		}
		for i, p := range r.Profiles {
			if p.ID == id {
				edits, err := s.unwire(p)
				if err != nil {
					return err
				}
				r.Disconnected = append(r.Disconnected, disconnected(p))
				r.Profiles = append(r.Profiles[:i], r.Profiles[i+1:]...)
				normalizePrimary(&r)
				return s.commit(r, edits)
			}
		}
		return fmt.Errorf("profile %q not found", id)
	})
}
func (s *Store) Repair(id string) (Profile, error) {
	var out Profile
	err := s.locked(func() error {
		r, err := s.Load()
		if err != nil {
			return err
		}
		for i := range r.Profiles {
			if r.Profiles[i].ID == id {
				edits, err := s.wire(&r.Profiles[i])
				if err != nil {
					return err
				}
				if err := s.commit(r, edits); err != nil {
					return err
				}
				out = r.Profiles[i]
				return nil
			}
		}
		return fmt.Errorf("profile %q not found", id)
	})
	return out, err
}

// DisconnectAll disables wiring without deleting registry entries or history.
func (s *Store) DisconnectAll() error {
	return s.locked(func() error {
		r, err := s.Load()
		if err != nil {
			return err
		}
		var edits []fileEdit
		for i := range r.Profiles {
			e, err := s.unwire(r.Profiles[i])
			if err != nil {
				return err
			}
			edits = append(edits, e...)
			r.Profiles[i].Enabled = false
			r.Profiles[i].Primary = false
			r.Profiles[i].Owned = nil
			r.Profiles[i].WiringStatus = "disabled"
			r.Profiles[i].WiringError = ""
		}
		return s.commit(r, edits)
	})
}

var validID = regexp.MustCompile(`^[A-Za-z0-9_. -]+$`)

func canonicalPath(dir string) (string, error) {
	if !filepath.IsAbs(dir) || strings.ContainsAny(dir, "\x00\r\n:\"'$`;|&\\()") {
		return "", fmt.Errorf("profile directory must be an absolute safe path without colons or shell metacharacters")
	}
	dir = filepath.Clean(dir)
	if dir == string(filepath.Separator) {
		return "", fmt.Errorf("a filesystem root cannot be a profile")
	}
	if st, err := os.Stat(dir); err == nil && !st.IsDir() {
		return "", fmt.Errorf("profile path is not a directory: %s", dir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	suffix := []string{}
	base := dir
	for {
		resolved, err := filepath.EvalSymlinks(base)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			if strings.ContainsAny(resolved, "\x00\r\n:\"'$`;|&\\()") {
				return "", fmt.Errorf("resolved profile directory contains unsafe characters")
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(base)
		if parent == base {
			return "", err
		}
		suffix = append(suffix, filepath.Base(base))
		base = parent
	}
}
func validateRegistry(r Registry) error {
	ids, dirs := map[string]bool{}, map[string]bool{}
	for _, p := range r.Profiles {
		if !validID.MatchString(p.ID) {
			return fmt.Errorf("invalid profile ID %q", p.ID)
		}
		if ids[p.ID] {
			return fmt.Errorf("duplicate legacy profile label %q; these profiles cannot be migrated without relabeling history", p.ID)
		}
		ids[p.ID] = true
		if dirs[p.Directory] {
			return fmt.Errorf("profile directory already configured: %s", p.Directory)
		}
		dirs[p.Directory] = true
	}
	return nil
}
func normalizePrimary(r *Registry) {
	for _, provider := range []Provider{Claude, Codex} {
		primary := -1
		for i := range r.Profiles {
			p := &r.Profiles[i]
			if p.Provider != provider {
				continue
			}
			if !p.Enabled {
				p.Primary = false
				continue
			}
			if p.Primary && primary < 0 {
				primary = i
			} else {
				p.Primary = false
			}
		}
		if primary < 0 {
			for i := range r.Profiles {
				if r.Profiles[i].Provider == provider && r.Profiles[i].Enabled {
					r.Profiles[i].Primary = true
					break
				}
			}
		}
	}
}
func (s *Store) locked(fn func() error) error {
	if err := config.CheckInstallDir(s.Dir); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, ".profiles.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if err := s.recover(); err != nil {
		return err
	}
	return fn()
}

// Keep an identity reservation when disconnecting so a later unrelated profile
// cannot inherit the label attached to retained historical records.
func disconnected(p Profile) Profile {
	p.Enabled = false
	p.Primary = false
	p.Owned = nil
	p.WiringStatus = "disconnected"
	p.WiringError = ""
	return p
}

func allocateID(r *Registry, provider Provider, directory string) string {
	for i, prior := range r.Disconnected {
		if prior.Provider == provider && prior.Directory == directory {
			r.Disconnected = append(r.Disconnected[:i], r.Disconnected[i+1:]...)
			return prior.ID
		}
	}
	candidate := ""
	if provider == Claude {
		candidate = LegacyName(directory)
	}
	if provider == Codex && filepath.Base(directory) == ".codex" {
		candidate = "codex-default"
	}
	if provider == Omp && filepath.Base(directory) == "agent" && filepath.Base(filepath.Dir(directory)) == ".omp" {
		candidate = "omp-default"
	}
	if provider == Opencode && filepath.Base(directory) == "opencode" && filepath.Base(filepath.Dir(directory)) == "share" {
		candidate = "opencode-default"
	}
	if validID.MatchString(candidate) {
		taken := false
		for _, p := range append(append([]Profile{}, r.Profiles...), r.Disconnected...) {
			if p.ID == candidate {
				taken = true
				break
			}
		}
		if !taken {
			return candidate
		}
	}
	return string(provider) + "-" + uuid.NewString()
}
