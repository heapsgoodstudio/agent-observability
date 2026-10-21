package installer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	agentobservability "github.com/heapsgoodstudio/agent-observability"
)

func TestBuildPlanDescriptions(t *testing.T) {
	in := Inputs{
		InstallDir: "/inst", ProfileDir: "/p",
		ClaudeBin: "/bin/claude", SelfBin: "/repo/dist/claude-obs",
		Backfill: true, SwiftcOK: true, UID: 501,
	}
	descs := Describe(BuildPlan(in))
	joined := strings.Join(descs, "\n")
	for _, want := range []string{
		"layout", "copy", "config.env", "store binaries", "sqlite",
		"telemetry", "statusline", "agents", "notifier", "backfill", "verify",
	} {
		if !strings.Contains(strings.ToLower(joined), want) {
			t.Errorf("plan missing a %q step:\n%s", want, joined)
		}
	}
	if len(BuildPlan(Inputs{Backfill: false})) != len(BuildPlan(Inputs{Backfill: true}))-1 {
		t.Error("backfill=false must drop exactly one step")
	}
}

func TestStepBackfillQueuesBackgroundImport(t *testing.T) {
	in := Inputs{InstallDir: t.TempDir()}
	if err := stepBackfill(in).Run(fakeExec{}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(in.InstallDir, "state", "import-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{\"profile\":\"\"}\n" {
		t.Fatalf("queue = %s", data)
	}
}

func TestIndexVerifyCmdRunsGoBinary(t *testing.T) {
	bin, args := indexVerifyCmd("/inst")
	if bin != "/inst/bin/claude-obs" {
		t.Fatalf("bin = %q, want /inst/bin/claude-obs", bin)
	}
	want := []string{"index", "--once", "--install-dir", "/inst"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v, want %v", args, want)
		}
	}
}

func TestCopyTree(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")
	os.MkdirAll(filepath.Join(src, "data", "sessions"), 0o755)
	os.WriteFile(filepath.Join(src, "app.js"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(src, "run.sh"), []byte("y"), 0o755)
	os.WriteFile(filepath.Join(src, "data", "sessions", "s.json"), []byte("z"), 0o644)

	err := CopyTree(os.DirFS(src), dst, func(rel string) bool { return rel == "data" })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "app.js")); err != nil {
		t.Fatal("file not copied")
	}
	fi, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil || fi.Mode()&0o111 == 0 {
		t.Fatal("exec bit not preserved")
	}
	if _, err := os.Stat(filepath.Join(dst, "data")); !os.IsNotExist(err) {
		t.Fatal("skip not honored")
	}
}

func TestCopyTreeReappliesModeOnOverwrite(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	os.WriteFile(filepath.Join(src, "run.sh"), []byte("new"), 0o755)
	os.WriteFile(filepath.Join(dst, "run.sh"), []byte("old"), 0o644)
	if err := CopyTree(os.DirFS(src), dst, nil); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("exec bit not enforced on overwrite: %v %v", fi.Mode(), err)
	}
}

func TestBackupNamesDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "settings.json")
	os.WriteFile(f, []byte("{}"), 0o644)
	backup(f)
	backup(f)
	matches, _ := filepath.Glob(f + ".bak.*")
	if len(matches) != 2 {
		t.Fatalf("want 2 distinct backups, got %d: %v", len(matches), matches)
	}
}

func TestStepCopySkipsSelfWhenInstalled(t *testing.T) {
	repo := t.TempDir()
	inst := t.TempDir()
	for _, d := range []string{"bin", "dashboard", "notifier"} {
		os.MkdirAll(filepath.Join(repo, d), 0o755)
	}
	os.WriteFile(filepath.Join(repo, "config.env.example"), []byte("X=1\n"), 0o644)
	os.MkdirAll(filepath.Join(inst, "bin"), 0o755)
	self := filepath.Join(inst, "bin", "claude-obs")
	os.WriteFile(self, []byte("running binary"), 0o755)

	in := Inputs{Assets: os.DirFS(repo), InstallDir: inst, SelfBin: self}
	if err := stepCopy(in).Run(RealExec{}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(self)
	if string(got) != "running binary" {
		t.Fatalf("self binary was rewritten: %q", got)
	}
}

// A Homebrew install has only the binary, so everything stepCopy places must
// come from the copies embedded in it, with the scripts executable.
func TestStepCopyInstallsTheEmbeddedFiles(t *testing.T) {
	inst := t.TempDir()
	self := filepath.Join(t.TempDir(), "claude-obs")
	os.WriteFile(self, []byte("binary"), 0o755)

	in := Inputs{Assets: agentobservability.Assets, InstallDir: inst, SelfBin: self}
	if err := stepCopy(in).Run(RealExec{}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	for _, script := range []string{"bin/fetch-stores.sh", "bin/limits-poller.sh", "bin/statusline-tap.sh", "bin/statusline.sh", "bin/usage-alert.sh", "notifier/build.sh"} {
		fi, err := os.Stat(filepath.Join(inst, script))
		if err != nil || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s not installed executable: %v %v", script, fi, err)
		}
	}
	for _, file := range []string{"notifier/main.swift", "notifier/icon.swift", "notifier/Info.plist.tmpl", "config.env.example"} {
		fi, err := os.Stat(filepath.Join(inst, file))
		if err != nil || fi.Mode().Perm() != 0o644 || fi.Size() == 0 {
			t.Errorf("%s not installed: %v %v", file, fi, err)
		}
	}
}

func TestExecuteStopsAtFirstError(t *testing.T) {
	ran := []string{}
	steps := []Step{
		{Desc: "a", Run: func(Exec, func(string)) error { ran = append(ran, "a"); return nil }},
		{Desc: "b", Run: func(Exec, func(string)) error { return os.ErrPermission }},
		{Desc: "c", Run: func(Exec, func(string)) error { ran = append(ran, "c"); return nil }},
	}
	if err := Execute(steps, RealExec{}, func(string) {}); err == nil {
		t.Fatal("want error")
	}
	if len(ran) != 1 || ran[0] != "a" {
		t.Fatalf("ran = %v", ran)
	}
}

// A re-install replaces the binary the launchd server agent is running.
// Truncating it in place keeps the inode, which invalidates the code
// signature of the mapped image and makes the next exec die with SIGKILL
// (exit 137, no output). The replacement must therefore land on a new inode.
func TestStepCopyReplacesTheBinaryOnANewInode(t *testing.T) {
	repo := t.TempDir()
	inst := t.TempDir()
	for _, d := range []string{"bin", "notifier"} {
		os.MkdirAll(filepath.Join(repo, d), 0o755)
	}
	os.WriteFile(filepath.Join(repo, "config.env.example"), []byte("X=1\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "bin", "statusline.sh"), []byte("#!/bin/bash\necho new\n"), 0o755)
	os.MkdirAll(filepath.Join(inst, "bin"), 0o755)

	// The previous install's copies, as a live machine would have them.
	dst := filepath.Join(inst, "bin", "claude-obs")
	os.WriteFile(dst, []byte("old binary"), 0o755)
	script := filepath.Join(inst, "bin", "statusline.sh")
	os.WriteFile(script, []byte("#!/bin/bash\necho old\n"), 0o755)
	beforeBin := inodeOf(t, dst)
	beforeScript := inodeOf(t, script)

	self := filepath.Join(repo, "claude-obs")
	os.WriteFile(self, []byte("new binary"), 0o755)

	in := Inputs{Assets: os.DirFS(repo), InstallDir: inst, SelfBin: self}
	if err := stepCopy(in).Run(RealExec{}, func(string) {}); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(dst); string(got) != "new binary" {
		t.Fatalf("binary not updated: %q", got)
	}
	if inodeOf(t, dst) == beforeBin {
		t.Error("binary was truncated in place (same inode); macOS SIGKILLs the result")
	}
	if fi, err := os.Stat(dst); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("binary not executable after replace: %v %v", fi.Mode(), err)
	}
	// Same hazard, milder: bash reads a script incrementally, and the poller
	// and alert agents can be mid-run when an install replaces one.
	if inodeOf(t, script) == beforeScript {
		t.Error("script was truncated in place (same inode)")
	}
	// No temp file left behind.
	ents, _ := os.ReadDir(filepath.Join(inst, "bin"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") && strings.HasSuffix(e.Name(), ".new") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no inode available on this platform")
	}
	return uint64(st.Ino)
}

// The previous install set statusLine, so a re-install always finds one. It
// must recognise its own entry rather than advising the user to integrate a
// script that is already wired up.
func TestStepStatuslineRecognisesItsOwnEntry(t *testing.T) {
	inst := t.TempDir()
	profile := t.TempDir()
	ours := filepath.Join(inst, "bin", "statusline.sh")
	settings := fmt.Sprintf(`{"statusLine":{"type":"command","command":%q}}`, ours)
	if err := os.WriteFile(filepath.Join(profile, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	var logged []string
	in := Inputs{InstallDir: inst, ProfileDir: profile}
	if err := stepStatusline(in).Run(RealExec{}, func(s string) { logged = append(logged, s) }); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(logged, "\n")
	if strings.Contains(joined, "add these two lines") {
		t.Errorf("re-install advised manual integration of its own statusline:\n%s", joined)
	}
	if !strings.Contains(joined, "already wired") {
		t.Errorf("want an already-wired log line, got:\n%s", joined)
	}
	// A foreign statusline still gets the advice, and is still left alone.
	foreign := `{"statusLine":{"type":"command","command":"/opt/mine/line.sh"}}`
	os.WriteFile(filepath.Join(profile, "settings.json"), []byte(foreign), 0o644)
	logged = nil
	if err := stepStatusline(in).Run(RealExec{}, func(s string) { logged = append(logged, s) }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(logged, "\n"), "add these two lines") {
		t.Errorf("foreign statusline lost its integration advice: %v", logged)
	}
	got, _ := os.ReadFile(filepath.Join(profile, "settings.json"))
	if !strings.Contains(string(got), "/opt/mine/line.sh") {
		t.Errorf("foreign statusline was overwritten: %s", got)
	}
}

// --- multi profile -----------------------------------------------------

// TestProfilesPutsPrimaryFirstAndDedupes: the list is what every wiring step
// iterates, so its order and uniqueness decide which profile the flat
// dashboard fields describe and whether a profile gets wired twice.
func TestProfilesPutsPrimaryFirstAndDedupes(t *testing.T) {
	in := Inputs{
		ProfileDir:  "/c/.claude-acme",
		ProfileDirs: []string{"/c/.claude-hg", "/c/.claude-acme/", "", "/c/.claude-hg"},
	}
	got := in.Profiles()
	want := []string{"/c/.claude-acme", "/c/.claude-hg"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestProfilesFallsBackToTheSingleProfile keeps an install that predates the
// list working: ProfileDirs empty must mean "just the primary", not "none".
func TestProfilesFallsBackToTheSingleProfile(t *testing.T) {
	got := Inputs{ProfileDir: "/c/.claude"}.Profiles()
	if len(got) != 1 || got[0] != "/c/.claude" {
		t.Fatalf("got %v, want [/c/.claude]", got)
	}
}
