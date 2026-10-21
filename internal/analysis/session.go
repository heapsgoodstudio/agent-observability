package analysis

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
)

// Only activity facts enter the analysis prompt. Compatibility cost fields in
// SQLite or old JSON files are not a bill and are deliberately omitted.
type sessionFacts struct {
	Provider   string          `json:"provider"`
	UsageKnown bool            `json:"usage_known"`
	ToolsKnown bool            `json:"tools_known"`
	ID         string          `json:"id"`
	Project    string          `json:"project"`
	Workspace  string          `json:"workspace,omitempty"`
	Profile    string          `json:"profile,omitempty"`
	Start      int64           `json:"start"`
	End        int64           `json:"end"`
	Requests   int             `json:"requests"`
	Tokens     sessionTokens   `json:"tokens"`
	ToolCalls  int             `json:"tool_calls"`
	UserTurns  int             `json:"user_turns"`
	Models     []string        `json:"models"`
	Tools      map[string]int  `json:"tools"`
	Skills     []string        `json:"skills"`
	MCPServers []string        `json:"mcp_servers"`
	Context    []contextSample `json:"context"`
}

type contextSample struct {
	Timestamp   float64 `json:"timestamp"`
	InputTokens float64 `json:"input_tokens"`
}

type sessionTokens struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// loadSessionFacts uses the same index as the dashboard. Legacy files are
// supported only by explicitly file-only runners, never as a fallback for a
// missing row or a database error in an installed application.
func (r *Runner) loadSessionFacts(sid string) (sessionFacts, error) {
	if r.cfg.DB != nil {
		row, ok, err := db.GetSession(r.cfg.DB, sid)
		if err != nil {
			return sessionFacts{}, fmt.Errorf("analysis: read session index: %w", err)
		}
		if !ok {
			return sessionFacts{}, ErrNotIndexed
		}
		return factsFromSession(*row), nil
	}
	data, err := os.ReadFile(filepath.Join(r.cfg.DataDir, "sessions", sid+".json"))
	if os.IsNotExist(err) {
		return sessionFacts{}, ErrNotIndexed
	}
	if err != nil {
		return sessionFacts{}, fmt.Errorf("analysis: read legacy session: %w", err)
	}
	var legacy struct {
		sessionFacts
		Models     []transcript.ModelCost `json:"models"`
		Trajectory [][3]float64           `json:"trajectory"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return sessionFacts{}, fmt.Errorf("analysis: decode legacy session: %w", err)
	}
	legacy.ID = sid
	legacy.addModelsAndContext(legacy.Models, legacy.Trajectory)
	return legacy.sessionFacts, nil
}

func (f *sessionFacts) addModelsAndContext(models []transcript.ModelCost, trajectory [][3]float64) {
	for _, model := range models {
		f.Models = append(f.Models, model.Model)
	}
	for _, point := range trajectory {
		f.Context = append(f.Context, contextSample{Timestamp: point[0], InputTokens: point[2]})
	}
}

// Use the session's own existing working directory so profile hooks and
// project settings see the workspace being analysed. A missing historical
// directory is skipped; never reconstruct paths from encoded project names.
func (r *Runner) analysisWorkDir(path string) (string, error) {
	scanned, err := transcript.Scan(path, nil)
	if err != nil {
		return "", fmt.Errorf("analysis: read transcript workspace: %w", err)
	}
	for _, cwd := range scanned.Cwds {
		if !filepath.IsAbs(cwd) {
			continue
		}
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			return cwd, nil
		}
	}
	return r.cfg.WorkDir, nil
}

func factsFromSession(row db.SessionRow) sessionFacts {
	facts := sessionFacts{
		Provider: row.Provider, UsageKnown: row.Provider == "" || row.Provider == "claude" || row.UsageKnown, ToolsKnown: row.Provider == "" || row.Provider == "claude" || row.ToolsKnown,
		ID: row.ID, Project: row.Project, Workspace: row.Workspace, Profile: row.Profile,
		Start: row.Start, End: row.End, Requests: row.Requests,
		Tokens:    sessionTokens{row.Tokens.Input, row.Tokens.Output, row.Tokens.CacheRead, row.Tokens.CacheCreation},
		ToolCalls: row.ToolCalls, UserTurns: row.UserTurns, Tools: row.Tools,
		Skills: row.Skills, MCPServers: row.MCPServers,
	}
	facts.addModelsAndContext(row.Models, row.Trajectory)
	return facts
}

func (r *Runner) findTranscript(sid string) (string, bool) {
	dir := r.cfg.SourceDir
	if dir == "" {
		dir = r.cfg.ProfileDir
	}
	native, provider := sid, "claude"
	if r.cfg.DB != nil {
		source, ok, err := db.GetSessionSource(r.cfg.DB, sid)
		if err != nil {
			return "", false
		}
		if ok {
			if row, found, e := db.GetSession(r.cfg.DB, sid); e != nil || !found || !row.HasTranscript {
				return "", false
			}
			st, e := os.Stat(source.Path)
			return source.Path, e == nil && !st.IsDir()
		}
		row, ok, err := db.GetSession(r.cfg.DB, sid)
		if err != nil {
			return "", false
		}
		if !ok {
			matches, _ := filepath.Glob(filepath.Join(dir, "projects", "*", native+".jsonl"))
			if len(matches) > 0 {
				return matches[0], true
			}
			return "", false
		}
		if root := r.cfg.Sources[row.Profile]; root != "" {
			dir = root
		}
		if row.NativeID != "" {
			native = row.NativeID
		}
		if row.Provider != "" {
			provider = row.Provider
		}
	}
	if provider == "claude" {
		matches, _ := filepath.Glob(filepath.Join(dir, "projects", "*", native+".jsonl"))
		if len(matches) > 0 {
			return matches[0], true
		}
		return "", false
	}
	if provider == "codex" || provider == "omp" {
		for _, sub := range []string{"sessions", "archived_sessions"} {
			found := ""
			filepath.WalkDir(filepath.Join(dir, sub), func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if !d.IsDir() && d.Type()&os.ModeSymlink == 0 && (d.Name() == native+".jsonl" || strings.HasSuffix(d.Name(), "-"+native+".jsonl") || strings.HasSuffix(d.Name(), "_"+native+".jsonl")) {
					found = path
					return fs.SkipAll
				}
				return nil
			})
			if found != "" {
				return found, true
			}
		}
	}
	return "", false
}
