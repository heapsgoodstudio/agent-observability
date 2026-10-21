package codex

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/pricing"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
	"github.com/heapsgoodstudio/agent-observability/internal/workspace"
)

type fileCache struct {
	Info   os.FileInfo
	Parsed parsed
}
type historyCache struct {
	UpdatedAt   int64
	Fingerprint string
	Summary     historySummary
}

// Collector caches successful reads only, keyed by file identity/size/mtime and
// thread update time. Replaced/truncated files are fully reparsed. Each result
// contains replacement summaries, never additive deltas: persist with UPSERT.
// The cache is optional and expendable; no ingestion checkpoint is written to
// Codex's home. A canceled or partial run must not mark older rows missing.
type Collector struct {
	mu        sync.Mutex
	files     map[string]fileCache
	histories map[string]historyCache
}

func NewCollector() *Collector {
	return &Collector{files: map[string]fileCache{}, histories: map[string]historyCache{}}
}
func Collect(ctx context.Context, opts Options) (Result, error) {
	return NewCollector().Collect(ctx, opts)
}

func (c *Collector) Collect(ctx context.Context, opts Options) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := Result{Complete: true, Details: map[string]Detail{}}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if opts.Home == "" || opts.ProfileID == "" {
		return result, fmt.Errorf("codex: home and stable profile ID are required")
	}
	home, err := filepath.EvalSymlinks(opts.Home)
	if err != nil {
		return result, fmt.Errorf("codex: home unavailable: %w", err)
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return result, err
	}
	opts.Home = home
	if opts.MaxThreads <= 0 {
		opts.MaxThreads = 10000
	}
	if c.files == nil {
		c.files = map[string]fileCache{}
	}
	if c.histories == nil {
		c.histories = map[string]historyCache{}
	}
	warn := func(s string) { result.Complete = false; result.Warnings = append(result.Warnings, s) }
	var rpc RPC
	metadata := map[string]thread{}
	paths := map[string]bool{}
	for _, sub := range []string{"sessions", "archived_sessions"} {
		root := filepath.Join(home, sub)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				if os.IsNotExist(err) && path == root {
					return nil
				}
				return err
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
				paths[path] = true
				if len(paths) > opts.MaxThreads*2 {
					return fmt.Errorf("rollout collection limit reached")
				}
			}
			return nil
		})
		if err != nil {
			warn("Local rollout coverage is partial: " + err.Error())
		}
	}
	ordered := make([]string, 0, len(paths))
	for p := range paths {
		ordered = append(ordered, p)
	}
	sort.Strings(ordered)
	parsedByID := map[string]parsed{}
	fingerprints := map[string]string{}
	sourcePaths := map[string]string{}
	ingest := func(path string) {
		p, info, err := c.read(ctx, path)
		if err != nil {
			warn("A local rollout could not be read: " + err.Error())
			return
		}
		if p.ID == "" {
			warn("A rollout has no session identity; skipped")
			return
		}
		sourcePaths[p.ID] = path
		fingerprints[p.ID] += fmt.Sprintf("%s:%d:%d;", path, info.Size(), info.ModTime().UnixNano())
		if previous, ok := parsedByID[p.ID]; ok {
			p = mergeParsed(previous, p)
		}
		parsedByID[p.ID] = p
		for _, w := range p.Warnings {
			warn(w)
		}
		result.Capabilities.LocalUsage = result.Capabilities.LocalUsage || p.UsageKnown
		if p.HistoryMode == "paginated" {
			result.Capabilities.PaginatedHistory = true
		} else {
			result.Capabilities.LegacyHistory = true
		}
		if _, ok := metadata[p.ID]; !ok {
			metadata[p.ID] = thread{ID: p.ID, Cwd: p.Cwd, Model: p.Model, CreatedAt: p.Start, UpdatedAt: p.End, ParentID: p.ParentID, ForkedFromID: p.ForkedFromID, HistoryMode: p.HistoryMode, Version: p.Version}
		}
	}
	for _, path := range ordered {
		if ctx.Err() != nil {
			break
		}
		ingest(path)
	}
	if !opts.LocalOnly && ctx.Err() == nil {
		rpc, err = connect(ctx, opts)
		if err != nil {
			warn("App Server unavailable; imported local rollout data only: " + err.Error())
		} else {
			defer rpc.Close()
			result.Capabilities.AppServer = true
			threads, listErr := listThreads(ctx, rpc, opts.MaxThreads)
			if listErr != nil {
				warn("App Server thread coverage is partial: " + listErr.Error())
			} else {
				result.Capabilities.ThreadList = true
			}
			for _, t := range threads {
				metadata[t.ID] = t
			}
			if len(threads) > 0 {
				var probe struct {
					Thread thread `json:"thread"`
				}
				if probeErr := rpc.Call(ctx, "thread/read", map[string]any{"threadId": threads[0].ID, "includeTurns": false}, &probe); probeErr == nil {
					result.Capabilities.ThreadRead = true
				}
			}
		}
	} else {
		warn("App Server skipped; metadata and archived-history coverage may be partial")
	}
	// The API may locate additional logs inside the same configured home.
	for _, t := range metadata {
		if t.Path == "" {
			continue
		}
		if p, ok := safePath(home, t.Path); ok {
			if !paths[p] && ctx.Err() == nil {
				paths[p] = true
				ingest(p)
			}
		} else {
			warn("App Server rollout path is missing or outside the configured home")
		}
	}
	ids := make([]string, 0, len(metadata))
	for id := range metadata {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t := metadata[id]
		p, hasLocal := parsedByID[id]
		if t.HistoryMode == "paginated" {
			result.Capabilities.PaginatedHistory = true
		} else {
			result.Capabilities.LegacyHistory = true
		}
		if result.Capabilities.Version == "" {
			result.Capabilities.Version = t.Version
		}
		if p.Cwd == "" {
			p.Cwd = t.Cwd
		}
		if p.ParentID == "" {
			p.ParentID = t.ParentID
		}
		if p.ForkedFromID == "" {
			p.ForkedFromID = t.ForkedFromID
		}
		if p.Start == 0 {
			p.Start = t.CreatedAt
		}
		if p.End < t.UpdatedAt {
			p.End = t.UpdatedAt
		}
		if p.HistoryMode == "" {
			p.HistoryMode = t.HistoryMode
		}
		detail := Detail{HistoryMode: p.HistoryMode, ForkedFromID: p.ForkedFromID, ToolsKnown: p.ToolsKnown, DurationKnown: p.DurationKnown, ErrorsKnown: p.ErrorsKnown}
		detail.UsageUnavailableReason = p.UsageUnavailableReason
		if !hasLocal {
			detail.UsageUnavailableReason = "missing_rollout"
		}
		if rpc != nil && ctx.Err() == nil {
			key := home + ":" + id
			cached, ok := c.histories[key]
			var h historySummary
			if ok && cached.UpdatedAt == t.UpdatedAt && cached.Fingerprint == fingerprints[id] {
				h = cached.Summary
			} else {
				h, err = readHistory(ctx, rpc, t, &result.Capabilities)
				if err == nil {
					c.histories[key] = historyCache{t.UpdatedAt, fingerprints[id], h}
				}
			}
			if err == nil {
				p.Tools = cloneTools(h.Tools)
				if p.RejectedOnly && (h.ModelActivity || h.Turns > p.Errors) {
					p.UsageKnown, p.RejectedOnly = false, false
					detail.UsageUnavailableReason = "missing_records"
				}
				p.UserTurns = h.UserTurns
				p.Errors = h.Errors
				p.DurationMS = h.DurationMS
				detail.ToolsKnown = true
				detail.ErrorsKnown = true
				detail.DurationKnown = h.DurationKnown
			} else {
				warn("A thread's tool/error history is unavailable: " + err.Error())
				err = nil
			}
		}
		row := db.SessionRow{ID: SessionID(opts.ProfileID, id), NativeID: id, Provider: "codex", Profile: opts.ProfileID, Project: p.Cwd, Workspace: workspace.TailSegments(p.Cwd, 2), Start: p.Start, End: p.End, HasTranscript: hasLocal || detail.ToolsKnown, UsageKnown: p.UsageKnown, CostKnown: p.UsageKnown && len(p.Warnings) == 0, Tools: p.Tools, UserTurns: p.UserTurns, ErrorCount: p.Errors, DurationMS: p.DurationMS}
		row.ToolsKnown = detail.ToolsKnown
		detail.RejectedOnly = p.RejectedOnly
		row.ErrorsKnown = detail.ErrorsKnown
		row.DurationKnown = detail.DurationKnown
		if p.Cwd == "" {
			row.Workspace = ""
		}
		if p.ParentID != "" {
			row.ParentID = SessionID(opts.ProfileID, p.ParentID)
		}
		models := map[string]*ModelUsage{}
		seen := map[string]bool{}
		sort.SliceStable(p.Requests, func(i, j int) bool { return p.Requests[i].Timestamp < p.Requests[j].Timestamp })
		for _, req := range p.Requests {
			if seen[req.Key] || inAncestor(req.Key, p.ForkedFromID, metadata, parsedByID) {
				continue
			}
			seen[req.Key] = true
			u := req.Usage
			// Database input is uncached input, as in Claude. The four database
			// token buckets partition totals; ReasoningTokens remains a subset.
			row.Tokens.Input += u.Input - u.Cached - u.Write
			row.Tokens.CacheRead += u.Cached
			row.Tokens.CacheCreation += u.Write
			row.Tokens.Output += u.Output
			row.ReasoningTokens += u.Reasoning
			row.Requests++
			cost, known := pricing.CodexEstimate(req.Model, u.Input, u.Output, u.Cached, u.Write)
			row.CostUSD += cost
			if req.Timestamp > 0 {
				row.Trajectory = append(row.Trajectory, [3]float64{float64(req.Timestamp), row.CostUSD, float64(u.Input)})
			}
			row.CostKnown = row.CostKnown && known
			m := models[req.Model]
			if m == nil {
				m = &ModelUsage{Model: req.Model, CostKnown: true}
				models[req.Model] = m
			}
			m.Input += u.Input
			m.Output += u.Output
			m.CachedInput += u.Cached
			m.CacheWrite += u.Write
			m.Reasoning += u.Reasoning
			m.Requests++
			m.EstimatedUSD += cost
			m.CostKnown = m.CostKnown && known
		}
		if len(models) == 0 && t.Model != "" {
			models[t.Model] = &ModelUsage{Model: t.Model, CostKnown: row.CostKnown}
		}
		modelNames := make([]string, 0, len(models))
		for name := range models {
			modelNames = append(modelNames, name)
		}
		sort.Strings(modelNames)
		for _, name := range modelNames {
			m := models[name]
			row.Models = append(row.Models, transcript.ModelCost{Model: name, CostUSD: m.EstimatedUSD})
			detail.Models = append(detail.Models, *m)
			if !m.CostKnown {
				detail.UnpricedModels = append(detail.UnpricedModels, name)
			}
		}
		detail.Billing = billingDetail(row.UsageKnown, detail.Models)
		for _, n := range row.Tools {
			row.ToolCalls += n
		}
		if len(row.Trajectory) > transcript.TrajectoryMax {
			full := row.Trajectory
			row.Trajectory = make([][3]float64, transcript.TrajectoryMax)
			for i := range row.Trajectory {
				row.Trajectory[i] = full[i*(len(full)-1)/(len(row.Trajectory)-1)]
			}
		}
		if !row.UsageKnown {
			// Structured per-session coverage is displayed once with the totals.
			result.Complete = false
		}
		detail.TranscriptPath = sourcePaths[id]
		result.Details[row.ID] = detail
		result.Sessions = append(result.Sessions, row)
	}
	// Bound repeated warning volume without exposing thread titles or bodies.
	unique := map[string]bool{}
	warnings := result.Warnings[:0]
	for _, w := range result.Warnings {
		if !unique[w] {
			warnings = append(warnings, w)
			unique[w] = true
		}
	}
	result.Warnings = warnings
	if err := ctx.Err(); err != nil {
		result.Complete = false
		result.Warnings = append(result.Warnings, "Collection canceled; completed local summaries retained")
		return result, err
	}
	return result, nil
}

func safePath(home, path string) (string, bool) {
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(home, p)
	return p, err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (c *Collector) read(ctx context.Context, path string) (parsed, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return parsed{}, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return parsed{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return parsed{}, nil, fmt.Errorf("not a regular rollout file")
	}
	if cached, ok := c.files[path]; ok && os.SameFile(info, cached.Info) && info.Size() == cached.Info.Size() && info.ModTime() == cached.Info.ModTime() {
		return cloneParsed(cached.Parsed), info, nil
	}
	p, err := parseRollout(ctx, io.LimitReader(f, info.Size()))
	if err == nil {
		c.files[path] = fileCache{info, p}
	}
	return cloneParsed(p), info, err
}

func cloneTools(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
func cloneParsed(p parsed) parsed {
	p.Tools = cloneTools(p.Tools)
	p.Requests = append([]request(nil), p.Requests...)
	p.Warnings = append([]string(nil), p.Warnings...)
	return p
}

func mergeParsed(a, b parsed) parsed {
	seen := map[string]bool{}
	for _, r := range a.Requests {
		seen[r.Key] = true
	}
	for _, r := range b.Requests {
		if !seen[r.Key] {
			a.Requests = append(a.Requests, r)
			seen[r.Key] = true
		}
	}
	for name, n := range b.Tools {
		if n > a.Tools[name] {
			a.Tools[name] = n
		}
	}
	if a.RejectedOnly && !b.RejectedOnly {
		a.UsageKnown = b.UsageKnown
		a.UsageUnavailableReason = b.UsageUnavailableReason
	} else if !b.RejectedOnly {
		a.UsageKnown = a.UsageKnown || b.UsageKnown
	}
	a.RejectedOnly = a.RejectedOnly && b.RejectedOnly
	if b.Start < a.Start {
		a.Start = b.Start
	}
	if b.End > a.End {
		a.End = b.End
	}
	if b.UserTurns > a.UserTurns {
		a.UserTurns = b.UserTurns
	}
	return a
}

func inAncestor(key, parent string, meta map[string]thread, local map[string]parsed) bool {
	seen := map[string]bool{}
	for parent != "" && !seen[parent] {
		seen[parent] = true
		for _, r := range local[parent].Requests {
			if r.Key == key {
				return true
			}
		}
		next := meta[parent].ForkedFromID
		if next == "" {
			next = local[parent].ForkedFromID
		}
		parent = next
	}
	return false
}
