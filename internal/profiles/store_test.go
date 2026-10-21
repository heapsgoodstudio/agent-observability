package profiles

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/heapsgoodstudio/agent-observability/internal/config"
	"github.com/pelletier/go-toml/v2"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func TestInitializeOnlyDefaultsAndExplicitEmpty(t *testing.T) {
	home := t.TempDir()
	inst := t.TempDir()
	for _, d := range []string{".claude", ".codex", ".claude-custom", ".codex-custom"} {
		os.MkdirAll(filepath.Join(home, d), 0o755)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude-custom"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex-custom"))
	s := New(inst)
	r, err := s.Initialize(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Profiles) != 2 || r.Profiles[0].ID != "default" || r.Profiles[1].ID != "codex-default" {
		t.Fatalf("roster: %+v", r)
	}
	empty := New(t.TempDir())
	r, err = empty.Initialize(t.TempDir(), nil)
	if err != nil || len(r.Profiles) != 0 {
		t.Fatalf("empty: %+v %v", r, err)
	}
	c, err := config.Load(filepath.Join(empty.Dir, "config.env"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Get("PROFILE_DIR", "wrong") != "" || c.Get("PROFILE_DIRS", "wrong") != "" {
		t.Fatal("empty projection reverted to default")
	}
	// A later default directory must not silently repopulate an explicit empty registry.
	r, err = empty.Initialize(home, nil)
	if err != nil || len(r.Profiles) != 0 {
		t.Fatalf("empty upgrade: %+v %v", r, err)
	}
}
func TestLegacyMigrationKeepsLabelsPrimaryAndConfig(t *testing.T) {
	inst := t.TempDir()
	home := t.TempDir()
	primary := filepath.Join(home, "outside", ".claude-work")
	extra := filepath.Join(home, ".claude")
	os.MkdirAll(primary, 0o755)
	os.MkdirAll(extra, 0o755)
	write(t, filepath.Join(inst, "config.env"), "PROFILE_DIR=\"ignored\"\nPROFILE_DIR=\""+primary+"\"\nPROFILE_DIRS=\""+extra+":"+primary+"\"\nDAILY_BUDGET_USD=123\n")
	desired, _ := json.Marshal(map[string]any{"env": ClaudeTelemetry("work"), "statusLine": map[string]any{"command": filepath.Join(inst, "bin", "statusline.sh")}})
	write(t, filepath.Join(primary, "settings.json"), string(desired))
	s := New(inst)
	r, err := s.Initialize(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Profiles) != 2 || r.Profiles[0].ID != "work" || !r.Profiles[0].Primary || r.Profiles[1].ID != "default" {
		t.Fatalf("migrated %+v", r)
	}
	if !strings.Contains(read(t, filepath.Join(inst, "config.env")), "DAILY_BUDGET_USD=123") {
		t.Fatal("lost operator config")
	}
	if err := s.Remove("work"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, filepath.Join(primary, "settings.json")), "OTEL_") {
		t.Fatal("legacy owned wiring not removed")
	}
}
func TestNamesAndDirectoryEditsKeepIdentityAndRemoveOwnedValues(t *testing.T) {
	s := New(t.TempDir())
	dir := filepath.Join(t.TempDir(), ".claude-work")
	os.MkdirAll(dir, 0o755)
	write(t, filepath.Join(dir, "settings.json"), `{"model":"opus","statusLine":{"command":"/my/statusline"},"env":{"KEEP":"yes"}}`)
	p, err := s.Save(Profile{Provider: Claude, Name: "Work", Directory: dir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	id := p.ID
	p.Name = "Renamed"
	p, err = s.Save(p)
	if err != nil || p.ID != id {
		t.Fatalf("rename: %+v %v", p, err)
	}
	newDir := filepath.Join(t.TempDir(), "different-name")
	os.MkdirAll(newDir, 0o755)
	p.Directory = newDir
	p, err = s.Save(p)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != id || !strings.Contains(read(t, filepath.Join(newDir, "settings.json")), "profile="+id) {
		t.Fatal("directory edit relabeled profile")
	}
	old := read(t, filepath.Join(dir, "settings.json"))
	if strings.Contains(old, "OTEL_") || !strings.Contains(old, "/my/statusline") || !strings.Contains(old, "KEEP") {
		t.Fatal(old)
	}
	// An operator's edit after installation stays on disconnect.
	path := filepath.Join(newDir, "settings.json")
	data := strings.Replace(read(t, path), "\"15000\"", "\"30000\"", 1)
	write(t, path, data)
	write(t, filepath.Join(s.Dir, "state", "history.fixture"), "history")
	if err := s.Remove(id); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, path), "30000") || strings.Contains(read(t, path), "CLAUDE_CODE_ENABLE_TELEMETRY") {
		t.Fatal(read(t, path))
	}
	if read(t, filepath.Join(s.Dir, "state", "history.fixture")) != "history" {
		t.Fatal("history removed")
	}
}
func TestConflictsAreVisibleAndNeverOverwriteDestinations(t *testing.T) {
	for _, provider := range []Provider{Claude, Codex} {
		t.Run(string(provider), func(t *testing.T) {
			s := New(t.TempDir())
			dir := t.TempDir()
			filename, body := "settings.json", `{"env":{"OTEL_LOGS_EXPORTER":"console"},"model":"opus"}`
			if provider == Codex {
				filename = "config.toml"
				body = "model = \"gpt-5\"\n[otel]\nexporter = \"console\"\n"
			}
			path := filepath.Join(dir, filename)
			write(t, path, body)
			p, err := s.Save(Profile{Provider: provider, Name: "Test", Directory: dir, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			if p.WiringStatus != "conflict" || p.WiringError == "" {
				t.Fatalf("missing conflict: %+v", p)
			}
			if got := read(t, path); got != body {
				t.Fatalf("conflict overwritten: %s", got)
			}
			if err := s.Remove(p.ID); err != nil {
				t.Fatal(err)
			}
			if read(t, path) != body {
				t.Fatal("unowned config removed")
			}
		})
	}
}
func TestCodexMergeRepairAndDisconnectPreserveExactConfig(t *testing.T) {
	s := New(t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := "# keep comment\nmodel = \"gpt-5\"\n[projects.\"/some/project\"]\ntrust_level = \"trusted\"\n"
	write(t, path, original)
	p, err := s.Save(Profile{Provider: Codex, Name: "Codex", Directory: dir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := toml.Unmarshal([]byte(read(t, path)), &decoded); err != nil {
		t.Fatal(err)
	}
	body := read(t, path)
	if !strings.HasPrefix(body, original) || !strings.Contains(body, "log_user_prompt = false") || !strings.Contains(body, "/ingest/codex/"+p.ID+"/v1/logs") {
		t.Fatal(body)
	}
	p, err = s.Repair(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, path) != body {
		t.Fatal("repair was not idempotent")
	}
	p.Enabled = false
	if _, err = s.Save(p); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != original {
		t.Fatal("disconnect changed unrelated TOML")
	}
}
func TestCodexMissingFinalNewlineIsNotAWiringConflict(t *testing.T) {
	for _, action := range []string{"repair", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			s := New(t.TempDir())
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			original := "# operator config\nmodel = \"gpt-5\"\n"
			write(t, path, original)
			p, err := s.Save(Profile{Provider: Codex, Name: "Codex", Directory: dir, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			wired := read(t, path)
			withoutNewline := strings.TrimSuffix(wired, "\n")
			write(t, path, withoutNewline)
			listed, err := s.List()
			if err != nil || len(listed) != 1 || listed[0].WiringStatus != "connected" || listed[0].WiringError != "" {
				t.Fatalf("inspection: %+v, %v", listed, err)
			}
			if read(t, path) != withoutNewline {
				t.Fatal("inspection modified config")
			}
			if action == "repair" {
				p, err = s.Repair(p.ID)
				if err != nil || p.WiringStatus != "connected" || read(t, path) != wired {
					t.Fatalf("repair: %+v, %v", p, err)
				}
			} else {
				if err := s.Remove(p.ID); err != nil {
					t.Fatal(err)
				}
				if read(t, path) != original {
					t.Fatal("disconnect changed unrelated config")
				}
			}
		})
	}
}

func TestCodexMissingFinalNewlineDoesNotAllowChangedTelemetry(t *testing.T) {
	s := New(t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	p, err := s.Save(Profile{Provider: Codex, Name: "Codex", Directory: dir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.TrimSuffix(strings.ReplaceAll(read(t, path), "http://127.0.0.1:8427", "https://collector.example.test"), "\n")
	write(t, path, edited)
	listed, err := s.List()
	if err != nil || len(listed) != 1 || listed[0].WiringStatus != "conflict" {
		t.Fatalf("inspection: %+v, %v", listed, err)
	}
	p, err = s.Repair(p.ID)
	if err != nil || p.WiringStatus != "conflict" {
		t.Fatalf("repair accepted changed telemetry: %+v, %v", p, err)
	}
	if err := s.Remove(p.ID); err == nil {
		t.Fatal("disconnect accepted changed telemetry")
	}
	if read(t, path) != edited {
		t.Fatal("operator edits were overwritten")
	}
}

func TestCanonicalDuplicateAndConcurrentSaves(t *testing.T) {
	s := New(t.TempDir())
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(Profile{Provider: Claude, Name: "Original", Directory: dir, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(Profile{Provider: Codex, Name: "Alias", Directory: alias, Enabled: true}); err == nil {
		t.Fatal("symlink duplicate accepted")
	}
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		path := t.TempDir()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := New(s.Dir).Save(Profile{Provider: Codex, Name: "Concurrent", Directory: path, Enabled: true})
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Load()
	if err != nil || len(r.Profiles) != 9 {
		t.Fatalf("lost saves: %d %v", len(r.Profiles), err)
	}
	primary := map[Provider]int{}
	for _, p := range r.Profiles {
		if p.Primary {
			primary[p.Provider]++
		}
	}
	if primary[Claude] != 1 || primary[Codex] != 1 {
		t.Fatalf("primaries: %v", primary)
	}
}
func TestInterruptedTransactionRecoveryAndExternalEditProtection(t *testing.T) {
	s := New(t.TempDir())
	path := filepath.Join(t.TempDir(), "settings.json")
	write(t, path, "before")
	edits := []fileEdit{{Path: path, Before: []byte("before"), After: []byte("after"), Existed: true, Mode: 0o600}}
	journal, _ := json.Marshal(edits)
	write(t, filepath.Join(s.Dir, ".profiles-transaction.json"), string(journal))
	if err := s.locked(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != "after" {
		t.Fatal("journal not recovered")
	}
	write(t, filepath.Join(s.Dir, ".profiles-transaction.json"), string(journal))
	write(t, path, "operator edit")
	if err := s.locked(func() error { return nil }); err == nil {
		t.Fatal("concurrent edit overwritten")
	}
	if read(t, path) != "operator edit" {
		t.Fatal("lost operator edit")
	}
}

func TestExistingCodexOTELTablePreservesCommentsAndUnrelatedFields(t *testing.T) {
	s := New(t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := "# initial comment\n[otel] # deployment telemetry\nenvironment = \"local\"\nlog_user_prompt = false\n\n[projects.\"/work\"]\ntrust_level = \"trusted\"\n"
	write(t, path, original)
	p, err := s.Save(Profile{Provider: Codex, Name: "Codex", Directory: dir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.WiringStatus != "connected" {
		t.Fatalf("merge: %+v", p)
	}
	var decoded map[string]any
	if err := toml.Unmarshal([]byte(read(t, path)), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["otel"].(map[string]any)["environment"] != "local" {
		t.Fatal("lost existing telemetry metadata")
	}
	if err := s.Remove(p.ID); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != original {
		t.Fatal("changed unrelated TOML")
	}
}
func TestFailedSaveLeavesRegistryAndConfigurationUntouched(t *testing.T) {
	s := New(t.TempDir())
	dir := t.TempDir()
	p, err := s.Save(Profile{Provider: Claude, Name: "Original", Directory: dir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	registry := read(t, filepath.Join(s.Dir, "profiles.json"))
	settings := read(t, filepath.Join(dir, "settings.json"))
	invalid := filepath.Join(t.TempDir(), "directory")
	os.MkdirAll(filepath.Join(invalid, "settings.json"), 0o755)
	p.Directory = invalid
	if _, err := s.Save(p); err == nil {
		t.Fatal("expected settings path failure")
	}
	if read(t, filepath.Join(s.Dir, "profiles.json")) != registry || read(t, filepath.Join(dir, "settings.json")) != settings {
		t.Fatal("failed save changed active profile")
	}
}
func TestReadOnlyLoadDoesNotDiscoverOrWrite(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	t.Setenv("HOME", home)
	inst := t.TempDir()
	r, err := New(inst).Load()
	if err != nil || len(r.Profiles) != 0 {
		t.Fatalf("load: %+v %v", r, err)
	}
	entries, err := os.ReadDir(inst)
	if err != nil || len(entries) != 0 {
		t.Fatalf("load wrote files: %+v %v", entries, err)
	}
}

func TestDisconnectedIdentitiesAreReservedAndRestored(t *testing.T) {
	s := New(t.TempDir())
	first := filepath.Join(t.TempDir(), ".claude-work")
	os.MkdirAll(first, 0o755)
	p, err := s.Save(Profile{Provider: Claude, Name: "Original", Directory: first, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(p.ID); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), ".claude-work")
	os.MkdirAll(other, 0o755)
	replacement, err := s.Save(Profile{Provider: Claude, Name: "Different", Directory: other, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == p.ID {
		t.Fatal("different directory inherited historical identity")
	}
	restored, err := s.Save(Profile{Provider: Claude, Name: "Restored", Directory: first, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != p.ID {
		t.Fatalf("reconnected identity = %s want %s", restored.ID, p.ID)
	}
}

func TestConfigSymlinkIsPreserved(t *testing.T) {
	s := New(t.TempDir())
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "managed-settings.json")
	write(t, target, `{"model":"opus"}`)
	path := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	p, err := s.Save(Profile{Provider: Claude, Name: "Shared settings", Directory: dir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("config symlink was replaced")
	}
	if err := s.Remove(p.ID); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, target), "OTEL_") {
		t.Fatal("owned wiring remained")
	}
}

func TestLegacyMigrationAddsDefaultCodexOnce(t *testing.T) {
	for _, action := range []string{"disable", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			home, inst := t.TempDir(), t.TempDir()
			primary := filepath.Join(home, "elsewhere", ".claude-work")
			extra := filepath.Join(home, "elsewhere", ".claude-personal")
			for _, directory := range []string{primary, extra, filepath.Join(home, ".claude"), filepath.Join(home, ".codex"), filepath.Join(home, ".codex-custom"), filepath.Join(home, ".claude-custom")} {
				if err := os.MkdirAll(directory, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CODEX_HOME", filepath.Join(home, ".codex-custom"))
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude-custom"))
			write(t, filepath.Join(inst, "config.env"), "PROFILE_DIR=\""+primary+"\"\nPROFILE_DIRS=\""+extra+":"+primary+"\"\n")
			s := New(inst)
			registry, err := s.Initialize(home, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(registry.Profiles) != 3 {
				t.Fatalf("expected exact legacy Claude roster and default Codex: %+v", registry.Profiles)
			}
			if registry.Profiles[0].ID != "work" || !registry.Profiles[0].Primary || registry.Profiles[1].ID != "personal" || registry.Profiles[1].Primary {
				t.Fatalf("legacy Claude identities or primary changed: %+v", registry.Profiles)
			}
			codex := registry.Profiles[2]
			expectedDirectory, err := canonicalPath(filepath.Join(home, ".codex"))
			if err != nil {
				t.Fatal(err)
			}
			if codex.ID != "codex-default" || codex.Provider != Codex || codex.Directory != expectedDirectory || !codex.Enabled || !codex.Primary {
				t.Fatalf("default Codex not configured during migration: %+v", codex)
			}
			if action == "disable" {
				codex.Enabled = false
				if _, err := s.Save(codex); err != nil {
					t.Fatal(err)
				}
			} else if err := s.Remove(codex.ID); err != nil {
				t.Fatal(err)
			}
			registry, err = s.Initialize(home, nil)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, p := range registry.Profiles {
				if p.Provider == Codex {
					found = true
					if action == "disconnect" || p.Enabled {
						t.Fatalf("upgrade rediscovered a %s profile: %+v", action, p)
					}
				}
			}
			if action == "disable" && !found {
				t.Fatal("disabled Codex profile was removed")
			}
			expectedCount := 2
			if action == "disable" {
				expectedCount = 3
			}
			if len(registry.Profiles) != expectedCount {
				t.Fatalf("upgrade changed roster: %+v", registry.Profiles)
			}
		})
	}
}

func TestOmpProfileIsReadOnlyAndNamesItsDefault(t *testing.T) {
	s := New(t.TempDir())
	home := t.TempDir()
	agent := filepath.Join(home, ".omp", "agent")
	if err := os.MkdirAll(filepath.Join(agent, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A name and primary flag sent from a client are ignored: an omp profile
	// is called by its harness and never runs provider actions.
	p, err := s.Save(Profile{Provider: Omp, Name: "my name", Directory: agent, Enabled: true, Primary: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "omp-default" || p.Name != "Oh My Pi" || p.WiringStatus != "connected" || p.Owned != nil || p.Primary {
		t.Fatalf("saved = %+v", p)
	}
	entries, err := os.ReadDir(agent)
	if err != nil || len(entries) != 1 {
		t.Fatalf("omp home must be left untouched, found %d entries (%v)", len(entries), err)
	}
	if err := s.Remove(p.ID); err != nil {
		t.Fatal(err)
	}
}

func TestOmpProfileWithoutSessionsNamesTheAgentHome(t *testing.T) {
	s := New(t.TempDir())
	dir := t.TempDir()
	p, err := s.Save(Profile{Provider: Omp, Directory: dir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.WiringStatus != "missing" || !strings.Contains(p.WiringError, "~/.omp/agent") {
		t.Fatalf("saved = %+v", p)
	}
}

func TestOmpNamesAreDerivedAndDisambiguated(t *testing.T) {
	s := New(t.TempDir())
	var dirs []string
	for _, rel := range []string{"one/.omp/agent", "two/.omp-alt/agent"} {
		dir := filepath.Join(t.TempDir(), rel)
		if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
		if _, err := s.Save(Profile{Provider: Omp, Directory: dir, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	ps, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range ps {
		names[p.Name] = true
	}
	if !names["Oh My Pi (.omp/agent)"] || !names["Oh My Pi (.omp-alt/agent)"] {
		t.Fatalf("names = %v; two omp directories must be told apart", names)
	}
}

func TestOmpNameSavedBeforeTheRuleReadsAsTheHarness(t *testing.T) {
	s := New(t.TempDir())
	dir := t.TempDir()
	write(t, filepath.Join(s.Dir, "profiles.json"), `{"version":1,"profiles":[{"id":"omp-default","provider":"omp","name":"omp","directory":"`+dir+`","enabled":true,"primary":true}]}`)
	r, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Profiles[0]; got.Name != "Oh My Pi" || got.Primary {
		t.Fatalf("loaded = %+v", got)
	}
}

func TestOpencodeProfileIsReadOnlyNamelessAndNamesItsDefault(t *testing.T) {
	s := New(t.TempDir())
	dir := filepath.Join(t.TempDir(), ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// No database yet: the error names the directory that holds it.
	p, err := s.Save(Profile{Provider: Opencode, Name: "ignored", Directory: dir, Enabled: true, Primary: true})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "opencode-default" || p.Name != "opencode" || p.Primary || p.WiringStatus != "missing" || !strings.Contains(p.WiringError, "opencode.db") {
		t.Fatalf("saved = %+v", p)
	}
	write(t, filepath.Join(dir, "opencode.db"), "")
	ps, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if ps[0].WiringStatus != "connected" {
		t.Fatalf("with a database = %+v", ps[0])
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("opencode's directory must be left untouched, found %d entries", len(entries))
	}
}

func TestHiddenAccountsSurviveProfileEditsAndRejectNonDetectedKeys(t *testing.T) {
	s := New(t.TempDir())
	for _, key := range []string{"claude:abc", "codex:x", "opencode", "opencode:", "omp:a:b"} {
		if err := s.SetAccountHidden(key, true); err == nil {
			t.Fatalf("%q hidden; only an Oh My Pi or opencode account can be", key)
		}
	}
	if err := s.SetAccountHidden("opencode:wire", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountHidden("opencode:w", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountHidden("opencode:wire", true); err != nil { // idempotent
		t.Fatal(err)
	}
	if _, err := s.Save(Profile{Provider: Claude, Name: "work", Directory: t.TempDir(), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	hidden, err := s.HiddenAccounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 2 || !hidden["opencode:wire"] || !hidden["opencode:w"] {
		t.Fatalf("hidden = %v; a profile save must keep the list", hidden)
	}
	if err := s.SetAccountHidden("opencode:wire", false); err != nil {
		t.Fatal(err)
	}
	if hidden, _ = s.HiddenAccounts(); len(hidden) != 1 || hidden["opencode:wire"] {
		t.Fatalf("after showing wire again = %v", hidden)
	}
}
