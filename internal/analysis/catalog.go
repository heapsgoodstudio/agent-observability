package analysis

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/codex"
)

// ModelOption contains only picker metadata, never credentials or model prices.
type ModelOption struct {
	ID, Name, Provider string
}

type ModelTarget struct {
	Provider, Directory, Binary string
}

type catalogEntry struct {
	done    chan struct{}
	models  []ModelOption
	err     error
	expires time.Time
}

// ModelCatalog discovers metadata on demand. Requests for the same credential
// home share one bounded process; ordinary UI polling reuses its cached result.
type ModelCatalog struct {
	mu      sync.Mutex
	entries map[ModelTarget]*catalogEntry
	// Injectable only for tests; the default performs metadata operations.
	Discover func(context.Context, ModelTarget) ([]ModelOption, error)
}

func (c *ModelCatalog) List(ctx context.Context, target ModelTarget) ([]ModelOption, error) {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = map[ModelTarget]*catalogEntry{}
	}
	entry := c.entries[target]
	if entry == nil || (!entry.expires.IsZero() && time.Now().After(entry.expires)) {
		entry = &catalogEntry{done: make(chan struct{})}
		c.entries[target] = entry
		discover := c.Discover
		if discover == nil {
			discover = discoverModels
		}
		go func() {
			// A browser navigation must not cancel a lookup shared by other callers.
			bounded, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			models, err := discover(bounded, target)
			c.mu.Lock()
			entry.models, entry.err = normalizeModels(models), err
			ttl := 5 * time.Minute
			if err != nil {
				ttl = 30 * time.Second
			}
			entry.expires = time.Now().Add(ttl)
			close(entry.done)
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-entry.done:
		return append([]ModelOption(nil), entry.models...), entry.err
	}
}

func normalizeModels(models []ModelOption) []ModelOption {
	out := []ModelOption{}
	seen := map[string]bool{}
	for _, m := range models {
		m.ID = strings.TrimSpace(m.ID)
		if m.ID == "" || !ValidModel(m.ID) || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		m.Name = strings.TrimSpace(m.Name)
		if m.Name == "" {
			m.Name = m.ID
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func discoverModels(ctx context.Context, target ModelTarget) ([]ModelOption, error) {
	if target.Provider == "codex" {
		client, err := codex.Open(ctx, codex.Options{Home: target.Directory, Binary: target.Binary})
		if err != nil {
			return nil, err
		}
		defer client.Close()
		return codexModels(ctx, client)
	}
	// Reuse the runner's environment selection, including the default Claude
	// home, OMP named-profile overrides and OpenCode's XDG data directory.
	r := New(Config{Provider: target.Provider, ProfileDir: target.Directory, Binary: target.Binary})
	base, _, cleanup, err := r.command([]string{"-p", ""})
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var args []string
	var input io.Reader
	switch target.Provider {
	case "claude":
		args = []string{"--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "-p", "--settings", `{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"0"}}`}
		// Only an SDK initialization request. Never send a user message/turn.
		input = strings.NewReader("{\"type\":\"control_request\",\"request_id\":\"model-discovery\",\"request\":{\"subtype\":\"initialize\"}}\n")
	case "omp":
		args = []string{"models", "--json"}
	case "opencode":
		args = []string{"models"}
	default:
		return nil, fmt.Errorf("unsupported model catalog")
	}
	dir, err := os.MkdirTemp("", "obs-model-catalog-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	cmd := exec.CommandContext(ctx, base.Path, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, base.Env, input
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var output catalogBuffer
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s model discovery failed", target.Provider)
	}
	return parseModels(target.Provider, output.Bytes())
}

type catalogBuffer struct{ data bytes.Buffer }

func (b *catalogBuffer) Bytes() []byte { return b.data.Bytes() }

func (b *catalogBuffer) Write(p []byte) (int, error) {
	if b.data.Len()+len(p) > 8<<20 {
		return 0, fmt.Errorf("model catalog exceeds size limit")
	}
	return b.data.Write(p)
}

func parseModels(provider string, raw []byte) ([]ModelOption, error) {
	out := []ModelOption{}
	switch provider {
	case "claude":
		scan := bufio.NewScanner(bytes.NewReader(raw))
		scan.Buffer(make([]byte, 4096), 8<<20)
		for scan.Scan() {
			var event struct {
				Type     string `json:"type"`
				Response struct {
					RequestID string `json:"request_id"`
					Subtype   string `json:"subtype"`
					Response  struct {
						Models *[]struct {
							Value string `json:"value"`
							Name  string `json:"displayName"`
						} `json:"models"`
					} `json:"response"`
				} `json:"response"`
			}
			if json.Unmarshal(scan.Bytes(), &event) != nil || event.Type != "control_response" || event.Response.RequestID != "model-discovery" {
				continue
			}
			if event.Response.Subtype != "success" || event.Response.Response.Models == nil {
				return nil, fmt.Errorf("Claude model catalog unavailable")
			}
			for _, m := range *event.Response.Response.Models {
				if m.Value != "default" {
					out = append(out, ModelOption{ID: m.Value, Name: m.Name})
				}
			}
			return out, nil
		}
		return nil, fmt.Errorf("Claude did not return a model catalog")
	case "omp":
		var data struct {
			Models *[]struct{ Provider, ID, Selector, Name, Kind string } `json:"models"`
		}
		if json.Unmarshal(raw, &data) != nil || data.Models == nil {
			return nil, fmt.Errorf("OMP returned an invalid model catalog")
		}
		for _, m := range *data.Models {
			if m.Kind != "" && m.Kind != "chat" {
				continue
			}
			id := m.Selector
			if id == "" {
				id = m.Provider + "/" + m.ID
			}
			out = append(out, ModelOption{ID: id, Name: m.Name, Provider: m.Provider})
		}
	case "opencode":
		for _, line := range strings.Split(string(raw), "\n") {
			id := strings.TrimSpace(line)
			provider, name, ok := strings.Cut(id, "/")
			if ok && name != "" && ValidModel(id) {
				out = append(out, ModelOption{ID: id, Name: name, Provider: provider})
			}
		}
		if len(out) == 0 && len(bytes.TrimSpace(raw)) > 0 {
			return nil, fmt.Errorf("OpenCode returned an invalid model catalog")
		}
	}
	return out, nil
}

func codexModels(ctx context.Context, client codex.RPC) ([]ModelOption, error) {
	out := []ModelOption{}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 20; page++ {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var result struct {
			Data *[]struct {
				Model  string `json:"model"`
				Name   string `json:"displayName"`
				Hidden bool   `json:"hidden"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if err := client.Call(ctx, "model/list", params, &result); err != nil {
			return nil, err
		}
		if result.Data == nil {
			return nil, fmt.Errorf("Codex returned an invalid model catalog")
		}
		for _, m := range *result.Data {
			if !m.Hidden {
				out = append(out, ModelOption{ID: m.Model, Name: m.Name})
			}
		}
		if result.NextCursor == "" {
			return out, nil
		}
		if seen[result.NextCursor] {
			return nil, fmt.Errorf("Codex model catalog repeated a cursor")
		}
		cursor = result.NextCursor
		seen[cursor] = true
	}
	return nil, fmt.Errorf("Codex model catalog exceeds page limit")
}
