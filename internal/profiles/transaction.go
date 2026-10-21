package profiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type fileEdit struct {
	Path    string
	Before  []byte
	After   []byte
	Existed bool
	Mode    os.FileMode
}

func readEdit(path string) (fileEdit, error) {
	e := fileEdit{Path: path, Mode: 0o600}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return e, nil
	}
	if err != nil {
		return e, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return e, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return e, err
	}
	e.Path = resolved
	e.Before = data
	e.Existed = true
	e.Mode = fi.Mode().Perm()
	return e, nil
}
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".agent-observability-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Chmod(mode); err != nil {
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
	if err := os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// commit stages every output before the first mutation. A durable journal
// permits idempotent roll-forward after interruption; regular failures roll
// back already-applied files. The registry is written last.
func (s *Store) commit(r Registry, edits []fileEdit) error {
	env, err := readEdit(filepath.Join(s.Dir, "config.env"))
	if err != nil {
		return err
	}
	env.After = ProjectConfig(env.Before, r)
	edits = append(edits, env)
	reg, err := readEdit(filepath.Join(s.Dir, "profiles.json"))
	if err != nil {
		return err
	}
	reg.After, err = json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	reg.After = append(reg.After, '\n')
	edits = append(edits, reg)
	changed := []fileEdit{}
	for _, e := range edits {
		if !e.Existed || !bytes.Equal(e.Before, e.After) {
			changed = append(changed, e)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	// Backups retain the original permissions, capped to private because agent
	// configuration can contain credentials and environment secrets.
	for _, e := range changed {
		if e.Existed {
			bak := fmt.Sprintf("%s.agent-observability.bak.%d", e.Path, time.Now().UnixNano())
			if err := atomicWrite(bak, e.Before, 0o600); err != nil {
				return err
			}
		}
	}
	journal, err := json.Marshal(changed)
	if err != nil {
		return err
	}
	path := filepath.Join(s.Dir, ".profiles-transaction.json")
	if err := atomicWrite(path, journal, 0o600); err != nil {
		return err
	}
	for i, e := range changed {
		if err := applyEdit(e); err != nil {
			var rollbackErr error
			for j := i - 1; j >= 0; j-- {
				old := changed[j]
				current, readErr := os.ReadFile(old.Path)
				if readErr != nil || !bytes.Equal(current, old.After) {
					rollbackErr = errors.Join(rollbackErr, fmt.Errorf("%s changed during rollback; external edits were preserved", old.Path))
					continue
				}
				var re error
				if old.Existed {
					re = atomicWrite(old.Path, old.Before, old.Mode)
				} else {
					re = os.Remove(old.Path)
				}
				rollbackErr = errors.Join(rollbackErr, re)
			}
			if rollbackErr == nil {
				os.Remove(path)
			}
			return errors.Join(fmt.Errorf("profile update: %w", err), rollbackErr)
		}
	}
	return os.Remove(path)
}
func applyEdit(e fileEdit) error {
	current, err := os.ReadFile(e.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && bytes.Equal(current, e.After) {
		return nil
	}
	if (err == nil) != e.Existed || !bytes.Equal(current, e.Before) {
		return fmt.Errorf("%s changed while updating profiles; no external edits were overwritten", e.Path)
	}
	return atomicWrite(e.Path, e.After, e.Mode)
}
func (s *Store) recover() error {
	path := filepath.Join(s.Dir, ".profiles-transaction.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var edits []fileEdit
	if err := json.Unmarshal(data, &edits); err != nil {
		return fmt.Errorf("profile transaction journal: %w", err)
	}
	for _, e := range edits {
		if err := applyEdit(e); err != nil {
			return fmt.Errorf("recover interrupted profile update: %w", err)
		}
	}
	return os.Remove(path)
}
func ProjectConfig(existing []byte, r Registry) []byte {
	var lines []string
	for _, line := range strings.Split(string(existing), "\n") {
		key, _, found := strings.Cut(strings.TrimSpace(line), "=")
		if found && (key == "PROFILE_DIR" || key == "PROFILE_DIRS" || key == "PROFILE_IDS" || key == "PROFILES_REGISTRY") {
			continue
		}
		if line == "# Profile projection from profiles.json; manage profiles in Settings." {
			continue
		}
		lines = append(lines, line)
	}
	primary := ""
	var dirs, ids []string
	for _, p := range r.Profiles {
		if p.Provider == Claude && p.Enabled && p.Primary {
			primary = p.Directory
			dirs = append(dirs, p.Directory)
			ids = append(ids, p.ID)
		}
	}
	for _, p := range r.Profiles {
		if p.Provider == Claude && p.Enabled && !p.Primary {
			dirs = append(dirs, p.Directory)
			ids = append(ids, p.ID)
		}
	}
	body := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n\n# Profile projection from profiles.json; manage profiles in Settings.\n"
	body += "PROFILES_REGISTRY=1\nPROFILE_DIR=" + strconv.Quote(primary) + "\nPROFILE_DIRS=" + strconv.Quote(strings.Join(dirs, ":")) + "\nPROFILE_IDS=" + strconv.Quote(strings.Join(ids, ":")) + "\n"
	return []byte(body)
}

// UpdateConfig serializes installer config upgrades with profile mutations.
// Existing profile values are always projected from the latest registry.
func (s *Store) UpdateConfig(update func([]byte) ([]byte, error)) error {
	return s.locked(func() error {
		e, err := readEdit(filepath.Join(s.Dir, "config.env"))
		if err != nil {
			return err
		}
		e.After, err = update(e.Before)
		if err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(s.Dir, "profiles.json")); err == nil {
			r, err := s.Load()
			if err != nil {
				return err
			}
			e.After = ProjectConfig(e.After, r)
		}
		return applyEdit(e)
	})
}
