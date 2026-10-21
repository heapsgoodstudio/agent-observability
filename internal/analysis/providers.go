package analysis

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Provider identifies the CLI doing the evaluation, independently of the source.
func (r *Runner) provider() string {
	if r.cfg.Provider == "" {
		return "claude"
	}
	return r.cfg.Provider
}

var modelRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@+\[\]\-]{0,199}$`)
var sessionIDRE = regexp.MustCompile(`^(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|(?:codex|omp|opencode):[a-zA-Z0-9_. -]+:[a-zA-Z0-9_-]+(?:/[a-zA-Z0-9_-]+)*)$`)

func ValidSessionID(id string) bool { return sessionIDRE.MatchString(id) }
func ValidModel(model string) bool  { return model == "" || modelRE.MatchString(model) }

// Binary resolves installed CLIs without assuming launchd has an interactive PATH.
func Binary(provider, configured string) string {
	if configured != "" {
		if p, err := exec.LookPath(configured); err == nil {
			return p
		}
		return ""
	}
	if p, err := exec.LookPath(provider); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, dir := range []string{filepath.Join(home, ".local/bin"), filepath.Join(home, ".bun/bin"), filepath.Join(home, ".opencode/bin"), "/opt/homebrew/bin", "/usr/local/bin"} {
		p := filepath.Join(dir, provider)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0111 != 0 {
			return p
		}
	}
	return ""
}

// command translates the shared prompt to each CLI's non-interactive contract.
// Prompts travel on stdin: large evidence bundles must not hit argv limits.
func (r *Runner) command(claudeArgs []string) (*exec.Cmd, func() (string, error), func(), error) {
	provider := r.provider()
	prompt, model := claudeArgs[1], ""
	for i := 2; i+1 < len(claudeArgs); i++ {
		if claudeArgs[i] == "--model" {
			model = claudeArgs[i+1]
		}
	}
	binary := r.cfg.Binary
	if binary == "" && provider == "claude" {
		binary = r.cfg.ClaudeBin
	}
	binary = Binary(provider, binary)
	if binary == "" {
		return nil, nil, nil, fmt.Errorf("%s CLI is not installed", provider)
	}
	env := os.Environ()
	// launchd has a minimal PATH; npm/Bun entry points still need their runtime.
	home, _ := os.UserHomeDir()
	searchPath := strings.Join([]string{filepath.Dir(binary), filepath.Join(home, ".bun", "bin"), filepath.Join(home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin", os.Getenv("PATH")}, string(os.PathListSeparator))
	env = mergeEnv(env, "PATH", searchPath)
	args := []string{}
	var readOutput func() (string, error)
	cleanup := func() {}
	switch provider {
	case "claude":
		args = append([]string{"-p"}, claudeArgs[2:]...)
		// An empty model delegates to the selected profile's configured default.
		if model == "" {
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--model" {
					args = append(args[:i], args[i+2:]...)
					break
				}
			}
		}
		env = spawnEnv(env, r.cfg.ProfileDir)
	case "codex":
		f, err := os.CreateTemp("", "obs-analysis-*.txt")
		if err != nil {
			return nil, nil, nil, err
		}
		f.Close()
		cleanup = func() { os.Remove(f.Name()) }
		args = []string{"exec", "--sandbox", "read-only", "--skip-git-repo-check", "--color", "never", "--output-last-message", f.Name(), "-"}
		if model != "" {
			args = append(args, "--model", model)
		}
		env = mergeEnv(env, "CODEX_HOME", r.cfg.ProfileDir)
		readOutput = func() (string, error) { b, e := os.ReadFile(f.Name()); return string(b), e }
	case "omp":
		args = []string{"--print", "--mode", "text", "--tools", "read,grep,glob", "--no-title"}
		if model != "" {
			args = append(args, "--model", model)
		}
		env = dropEnv(dropEnv(env, "OMP_PROFILE"), "PI_PROFILE")
		env = mergeEnv(env, "PI_CODING_AGENT_DIR", r.cfg.ProfileDir)
	case "opencode":
		args = []string{"run", "--format", "json"}
		if model != "" {
			args = append(args, "--model", model)
		}
		// Select the credential/history store via its XDG data root.
		if filepath.Base(r.cfg.ProfileDir) != "opencode" {
			return nil, nil, nil, fmt.Errorf("OpenCode analysis requires a data directory named opencode under an XDG data root")
		}
		env = mergeEnv(env, "XDG_DATA_HOME", filepath.Dir(r.cfg.ProfileDir))
		env = mergeEnv(env, "OPENCODE_PERMISSION", `{"*":"deny","read":"allow","glob":"allow","grep":"allow","list":"allow","external_directory":"allow"}`)
	default:
		return nil, nil, nil, fmt.Errorf("unsupported analysis provider %q", provider)
	}
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(prompt)
	return cmd, readOutput, cleanup, nil
}

// OpenCode stdout is a stream of events, never the report itself.
func openCodeOutput(raw string) (string, error) {
	var out strings.Builder
	scan := bufio.NewScanner(strings.NewReader(raw))
	scan.Buffer(make([]byte, 4096), 16<<20)
	for scan.Scan() {
		var e struct {
			Type string `json:"type"`
			Part struct {
				Text string `json:"text"`
			} `json:"part"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(scan.Bytes(), &e); err != nil {
			return "", fmt.Errorf("invalid OpenCode event: %w", err)
		}
		if e.Type == "error" {
			return "", fmt.Errorf("OpenCode execution failed: %s", e.Error)
		}
		if e.Type == "text" {
			out.WriteString(e.Part.Text)
			out.WriteByte('\n')
		}
	}
	return out.String(), scan.Err()
}

func (r *Runner) subagentCount(id string) *int64 {
	if r.provider() != "claude" {
		return nil
	}
	return countSubagents(r.cfg.VLURL, id)
}

// Selection never takes a filesystem path from an RPC caller. Profile IDs are
// resolved against the configured registry by the server's runner factory.
type Selection struct {
	Provider, Profile, SourceProvider, SourceProfile, SourceAccountKey string
	SourceProfiles                                                     []string
}
