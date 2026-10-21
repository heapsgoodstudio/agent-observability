package omp

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/payg"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
	"github.com/heapsgoodstudio/agent-observability/internal/workspace"
)

type fileCache struct {
	Info os.FileInfo
	PF   parsedFile
}

// Collector caches parsed per-file results keyed by absolute path, size and
// mtime, the same identity the codex provider's cache uses; an unchanged
// file is not reread. Cross-file dedup is recomputed on every Collect from
// the cached per-file usage records, so an unchanged file need not be
// reparsed just because a sibling file changed underneath it.
type Collector struct {
	mu    sync.Mutex
	files map[string]fileCache
}

func NewCollector() *Collector {
	return &Collector{files: map[string]fileCache{}}
}

// discovered is one JSONL file found under Home/sessions, before it is
// parsed. stem is the associated main transcript's stem even for a nested
// file: "<project>/<stem>/sub1.jsonl" and "<project>/<stem>.jsonl" share it.
type discovered struct {
	path          string
	project, stem string
	nested        bool
}

// fileEntry pairs a discovered file with its parsed content.
type fileEntry struct {
	discovered
	pf parsedFile
}

func (c *Collector) Collect(ctx context.Context, opts Options) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := Result{Complete: true, Sources: map[string]db.SessionSource{}}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if opts.Home == "" || opts.ProfileID == "" {
		return result, fmt.Errorf("omp: home and profile ID are required")
	}
	if c.files == nil {
		c.files = map[string]fileCache{}
	}
	warn := func(s string) { result.Complete = false; result.Warnings = append(result.Warnings, s) }

	sessionsRoot := filepath.Join(opts.Home, "sessions")
	if _, err := os.Stat(sessionsRoot); err != nil {
		return result, fmt.Errorf("omp: sessions directory unavailable: %w", err)
	}

	files := discoverFiles(ctx, sessionsRoot, warn)

	entries := make([]fileEntry, 0, len(files))
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		pf, err := c.readFile(f.path)
		if err != nil {
			warn("A local session file could not be read: " + err.Error())
			continue
		}
		if pf.Malformed {
			warn("A session file has unparseable records: " + f.path)
		}
		entries = append(entries, fileEntry{f, pf})
	}

	// Main-file identity: header id when present, else the file's own stem.
	// Recorded for every (project,stem) pair with a discovered main file, so
	// a nested transcript can resolve its parent's id and cwd; one with no
	// discovered main file falls back to its stem wherever it is looked up.
	mainSid := map[string]string{}
	mainCwd := map[string]string{}
	for _, e := range entries {
		if e.nested {
			continue
		}
		key := mainKey(e.project, e.stem)
		sid := e.stem
		if e.pf.HasHeader && e.pf.HeaderID != "" {
			sid = e.pf.HeaderID
		}
		mainSid[key] = sid
		mainCwd[key] = e.pf.Cwd
	}
	parentSidOf := func(e fileEntry) string {
		if sid, ok := mainSid[mainKey(e.project, e.stem)]; ok {
			return sid
		}
		return e.stem
	}
	sidOf := func(e fileEntry) string {
		if e.pf.HasHeader && e.pf.HeaderID != "" {
			return e.pf.HeaderID
		}
		if !e.nested {
			return e.stem
		}
		base := strings.TrimSuffix(filepath.Base(e.path), ".jsonl")
		return parentSidOf(e) + "/" + base
	}
	cwdOf := func(e fileEntry) string {
		if e.pf.Cwd != "" {
			return e.pf.Cwd
		}
		if e.nested {
			return mainCwd[mainKey(e.project, e.stem)]
		}
		return ""
	}

	// pathSid resolves a fork's parentSession path (an absolute jsonl path
	// from that file's own header) to the row id of the file it names, when
	// that file was itself discovered in this run.
	pathSid := make(map[string]string, len(entries))
	for _, e := range entries {
		pathSid[filepath.Clean(e.path)] = sidOf(e)
	}

	// Dedup ordering, per docs/omp-provider.md: earliest header timestamp
	// first, ties by path. A file with no header sorts as timestamp 0,
	// broken deterministically by the path tiebreak.
	sort.SliceStable(entries, func(i, j int) bool {
		ti, tj := entries[i].pf.HeaderTS, entries[j].pf.HeaderTS
		if ti != tj {
			return ti < tj
		}
		return entries[i].path < entries[j].path
	})

	seen := map[string]bool{}
	buckets := map[[3]string]*payg.Bucket{}
	rows := make([]db.SessionRow, 0, len(entries))
	for _, e := range entries {
		if ctx.Err() != nil {
			break
		}
		sid := sidOf(e)
		cwd := cwdOf(e)
		row := db.SessionRow{
			ID:            SessionID(opts.ProfileID, sid),
			NativeID:      sid,
			Provider:      "omp",
			Profile:       opts.ProfileID,
			Project:       cwd,
			Workspace:     workspace.TailSegments(cwd, 2),
			CostKnown:     true,
			UsageKnown:    true,
			ToolsKnown:    true,
			ErrorsKnown:   true,
			DurationKnown: true,
			HasTranscript: true,
			Tools:         cloneTools(e.pf.Tools),
			ToolCalls:     e.pf.ToolCalls,
			UserTurns:     e.pf.UserTurns,
			ErrorCount:    e.pf.ErrorCount,
		}
		result.Sources[row.ID] = db.SessionSource{ID: row.ID, Path: e.path, Cwd: cwd, Format: "omp"}
		if cwd == "" {
			row.Workspace = ""
		}
		if e.nested {
			row.ParentID = SessionID(opts.ProfileID, parentSidOf(e))
		} else if e.pf.ParentSession != "" {
			if targetSid, ok := pathSid[filepath.Clean(e.pf.ParentSession)]; ok {
				row.ParentID = SessionID(opts.ProfileID, targetSid)
			}
		}
		if e.pf.HasHeader {
			row.Start = e.pf.HeaderTS
		} else {
			row.Start = e.pf.FirstTS
		}
		row.End = e.pf.LastTS
		duration := row.End - row.Start
		if duration < 0 {
			duration = 0
		}
		row.DurationMS = duration * 1000

		var counted []usageRecord
		for _, u := range e.pf.Usage {
			if seen[u.Key] {
				continue
			}
			seen[u.Key] = true
			counted = append(counted, u)
		}
		sort.SliceStable(counted, func(i, j int) bool { return counted[i].Timestamp < counted[j].Timestamp })

		modelCost := map[string]float64{}
		var modelOrder []string
		var fullTrajectory [][3]float64
		for _, u := range counted {
			row.Requests++
			row.Tokens.Input += u.Input
			row.Tokens.Output += u.Output
			row.Tokens.CacheRead += u.CacheRead
			row.Tokens.CacheCreation += u.CacheWrite
			row.ReasoningTokens += u.Reasoning
			if u.CostKnown {
				row.CostUSD += u.CostUSD
			} else {
				row.CostKnown = false
			}
			fullTrajectory = append(fullTrajectory, [3]float64{float64(u.Timestamp), row.CostUSD, float64(u.Input + u.CacheRead + u.CacheWrite)})

			provider, model := payg.CanonicalProvider(u.Provider), payg.CanonicalModel(u.Model)
			modelKey := provider + "/" + model
			if _, ok := modelCost[modelKey]; !ok {
				modelOrder = append(modelOrder, modelKey)
			}
			if u.CostKnown {
				modelCost[modelKey] += u.CostUSD
			}

			day := time.Unix(u.Timestamp, 0).In(time.Local).Format("2006-01-02")
			bk := [3]string{day, provider, model}
			bucket := buckets[bk]
			if bucket == nil {
				bucket = &payg.Bucket{Day: day, Provider: provider, Model: model, CostKnown: true}
				buckets[bk] = bucket
			}
			bucket.Requests++
			bucket.Input += u.Input
			bucket.Output += u.Output
			bucket.CacheRead += u.CacheRead
			bucket.CacheWrite += u.CacheWrite
			bucket.Reasoning += u.Reasoning
			if u.CostKnown {
				bucket.CostUSD += u.CostUSD
			} else {
				bucket.CostKnown = false
			}
		}
		row.CostUSD = round6(row.CostUSD)
		// modelOrder already holds first-appearance order; a stable sort on
		// cost alone leaves ties in that order.
		sort.SliceStable(modelOrder, func(i, j int) bool { return modelCost[modelOrder[i]] > modelCost[modelOrder[j]] })
		for _, name := range modelOrder {
			row.Models = append(row.Models, transcript.ModelCost{Model: name, CostUSD: round6(modelCost[name])})
		}
		row.Trajectory = transcript.Downsample(fullTrajectory, transcript.TrajectoryMax)

		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	result.Sessions = rows

	bucketKeys := make([][3]string, 0, len(buckets))
	for k := range buckets {
		bucketKeys = append(bucketKeys, k)
	}
	sort.Slice(bucketKeys, func(i, j int) bool {
		if bucketKeys[i][0] != bucketKeys[j][0] {
			return bucketKeys[i][0] < bucketKeys[j][0]
		}
		if bucketKeys[i][1] != bucketKeys[j][1] {
			return bucketKeys[i][1] < bucketKeys[j][1]
		}
		return bucketKeys[i][2] < bucketKeys[j][2]
	})
	for _, k := range bucketKeys {
		b := *buckets[k]
		b.CostUSD = round6(b.CostUSD)
		result.Usage = append(result.Usage, b)
	}

	if err := ctx.Err(); err != nil {
		result.Complete = false
		result.Warnings = append(result.Warnings, "Collection canceled; completed local summaries retained")
		return result, err
	}
	return result, nil
}

// mainKey identifies a (project,stem) pair's associated main transcript
// across the maps that carry its sid/cwd, independent of whether that main
// file was actually discovered.
func mainKey(project, stem string) string { return project + "\x00" + stem }

// discoverFiles walks Home/sessions once, classifying every ".jsonl" file as
// a main transcript (depth 2) or a nested one (depth 3+, any further depth
// under its stem directory). Everything else, including "*.log" files and
// directories, is ignored; a walk error partway through is reported as
// partial coverage rather than failing the whole collection, since the
// sessions directory itself was already confirmed to exist.
func discoverFiles(ctx context.Context, sessionsRoot string, warn func(string)) []discovered {
	var files []discovered
	err := filepath.WalkDir(sessionsRoot, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		if path == sessionsRoot {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		rel, relErr := filepath.Rel(sessionsRoot, path)
		if relErr != nil {
			return nil
		}
		parts := strings.Split(rel, string(filepath.Separator))
		switch {
		case len(parts) == 2:
			files = append(files, discovered{path: path, project: parts[0], stem: strings.TrimSuffix(parts[1], ".jsonl")})
		case len(parts) >= 3:
			files = append(files, discovered{path: path, project: parts[0], stem: parts[1], nested: true})
		}
		return nil
	})
	if err != nil {
		warn("Local session coverage is partial: " + err.Error())
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files
}

// readFile parses path, or returns the cached result when its identity
// (size and mtime, on the same inode) has not changed since the last read.
func (c *Collector) readFile(path string) (parsedFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return parsedFile{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return parsedFile{}, err
	}
	if !info.Mode().IsRegular() {
		return parsedFile{}, fmt.Errorf("not a regular session file")
	}
	if cached, ok := c.files[path]; ok && os.SameFile(info, cached.Info) && info.Size() == cached.Info.Size() && info.ModTime() == cached.Info.ModTime() {
		return cached.PF, nil
	}
	pf, err := parseFile(path, io.LimitReader(f, info.Size()))
	if err != nil {
		return parsedFile{}, err
	}
	c.files[path] = fileCache{Info: info, PF: pf}
	return pf, nil
}

func cloneTools(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func round6(f float64) float64 { return math.Round(f*1e6) / 1e6 }
