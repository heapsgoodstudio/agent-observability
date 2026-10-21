package analysis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/localhistory"
)

// AdviceScope separates the evidence selection from the authenticated profile
// executing the job. Profiles is nil for provider-wide history, an exact
// allowlist otherwise. Configuration paths come only from the current roster.
type AdviceScope struct {
	Provider       string            `json:"provider,omitempty"`
	AccountKey     string            `json:"account_key,omitempty"`
	Profile        string            `json:"profile,omitempty"`
	Profiles       []string          `json:"profiles"`
	Note           string            `json:"note,omitempty"`
	Configurations []EvidenceProfile `json:"configurations,omitempty"`
}

type EvidenceProfile struct {
	Provider  string `json:"provider,omitempty"`
	ID        string `json:"id"`
	Directory string `json:"directory"`
}

// Key intentionally excludes the changing roster and execution profile: a
// saved account report retains its identity as connections are added/removed.
func (s AdviceScope) Key() string {
	if s.Provider == "" && s.AccountKey == "" && s.Profile == "" {
		return "advice"
	}
	data, _ := json.Marshal([]string{s.Provider, s.AccountKey, s.Profile})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func AdviceReportDir(dataDir string, scope AdviceScope) string {
	return adviceDirForKey(dataDir, scope.Key())
}
func adviceDirForKey(dataDir, key string) string {
	if key == "advice" {
		return filepath.Join(dataDir, "advice")
	}
	// Only generated hashes are valid directory names. A damaged DB row must
	// never turn orphan recovery into a write outside this report directory.
	if len(key) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(key); err != nil {
		return ""
	}
	return filepath.Join(dataDir, "advice", "scopes", key)
}

func (r *Runner) ScopedAdviceStatus(scope AdviceScope) (*db.AnalysisRun, bool, error) {
	return r.status("advice", scope.Key(), filepath.Join(AdviceReportDir(r.cfg.DataDir, scope), "status.json"))
}

// adviceEvidence is disposable input for one invocation. It is never an
// alternate session index and never includes compatibility monetary fields.
type adviceEvidence struct {
	TotalSessions   int             `json:"total_sessions"`
	SampledSessions int             `json:"sampled_sessions"`
	Selection       string          `json:"selection"`
	Scope           AdviceScope     `json:"scope"`
	Coverage        string          `json:"coverage"`
	Sessions        []adviceSession `json:"sessions"`
}
type adviceSession struct {
	sessionFacts
	Transcript string `json:"transcript,omitempty"`
}

func (r *Runner) buildAdviceEvidence(scope AdviceScope, depths ...string) (string, error) {
	depth := "evidence"
	if len(depths) > 0 {
		depth = depths[0]
	}
	provider := scope.Provider
	var rows []db.SessionRow
	var err error
	if r.cfg.DB != nil {
		rows, err = db.ListAdviceSessions(r.cfg.DB, provider, scope.Profiles)
		if err != nil {
			return "", err
		}
	} else if scope.Key() != "advice" {
		return "", fmt.Errorf("scoped advice requires the session database")
	}
	rows = db.FilterAdviceSessions(rows, scope.AccountKey)
	bundle := adviceEvidence{Scope: scope, Coverage: "Evidence uses the selected provider and currently linked profile history. Historical account ownership is not verified. Mixed-provider sessions retain whole-session totals.", Sessions: []adviceSession{}, TotalSessions: len(rows), Selection: "Up to 200 most recent sessions; evidence transcript paths limited to the first 10. This sample favors recent activity."}
	dir, err := os.MkdirTemp("", "claude-obs-advice-evidence-")
	if err != nil {
		return "", err
	}
	complete := false
	defer func() {
		if !complete {
			os.RemoveAll(dir)
		}
	}()
	dirs := map[string]string{}
	for _, p := range scope.Configurations {
		dirs[p.ID] = p.Directory
	}
	readerConfig := r.cfg
	readerConfig.Sources = dirs
	reader := New(readerConfig)
	for _, row := range rows {
		if len(bundle.Sessions) >= 200 {
			break
		}
		evidence := adviceSession{sessionFacts: factsFromSession(row)}
		if depth == "shallow" {
			evidence.Tools = nil
			evidence.Context = nil
		}
		if depth == "evidence" && len(bundle.Sessions) < 10 && row.HasTranscript {
			if path, ok := reader.findTranscript(row.ID); ok {
				if row.Provider == "opencode" {
					export := filepath.Join(dir, row.ID+".jsonl")
					if err := localhistory.ExportOpenCode(context.Background(), path, row.NativeID, export); err == nil {
						evidence.Transcript = export
					}
				} else {
					evidence.Transcript = path
				}
			}
		}
		bundle.Sessions = append(bundle.Sessions, evidence)
	}
	bundle.SampledSessions = len(bundle.Sessions)
	if depth == "shallow" {
		bundle.Selection += " Shallow inline evidence is additionally capped at 128 KiB."
		for len(bundle.Sessions) > 0 {
			encoded, err := json.MarshalIndent(bundle, "", "  ")
			if err != nil {
				return "", err
			}
			if len(encoded) <= 128<<10 {
				break
			}
			bundle.Sessions = bundle.Sessions[:len(bundle.Sessions)-1]
			bundle.SampledSessions = len(bundle.Sessions)
		}
	}
	if r.cfg.DB == nil {
		bundle.Coverage += " No SQLite database is configured, so no session evidence is available."
	}
	if err := writeJSONAtomic(filepath.Join(dir, "evidence.json"), bundle); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	if err := writeJSONAtomic(filepath.Join(dir, "configurations.json"), scope.Configurations); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	complete = true
	return dir, nil
}
