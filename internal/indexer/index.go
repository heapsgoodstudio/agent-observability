// Package indexer wires internal/transcript's parsing to internal/db's
// storage. It lives in its own package rather than internal/transcript
// because internal/db already imports internal/transcript
// (SessionRow.Tokens/Models reuse its types), so internal/transcript
// importing internal/db back would be a cycle; internal/transcript stays a
// pure parsing leaf, and this package depends on both.
//
// The sessions and transcript_state tables in sqlite are the sole source of
// truth: internal/server reads sessions straight from the db (see
// internal/server/index.go's listIndexedSessions). Nothing here writes a
// JSON projection of a session; TestNoJSONWritten pins that absence.
package indexer

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/pricing"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
	"github.com/heapsgoodstudio/agent-observability/internal/workspace"
)

// SchemaVersion gates the parsed-row shape: a mismatch against the meta
// table's sessions_schema value clears transcript_state, forcing one full
// re-parse. Bump it whenever a field this package writes is added or
// changes meaning, so existing rows get rebuilt rather than silently
// carrying the old shape.
//
// A price change is one of those meaning changes, because cost_usd is
// computed at parse time rather than read from the transcript. 5 is the
// bump for claude-fable-5-1: it was absent from the price table, so every
// request against it priced at $0, and its cache reads bill at a quarter of
// what the global CacheReadX charges. Neither figure corrects itself without
// a re-parse. Version 6 includes cache writes in context observations and
// excludes subagent contexts from the main conversation's trajectory.
const SchemaVersion = "6"

// Meta keys this package owns.
const (
	metaSchema      = "sessions_schema"
	metaGeneratedAt = "sessions_generated_at"
)

// IndexOpts configures one RunIndex pass.
// ProfileRoot is one profile's transcript root and the name to attribute its
// sessions to. The name is the same string OTEL_RESOURCE_ATTRIBUTES stamps on
// that profile's telemetry, which is what lets a session row join to its own
// metrics and logs.
type ProfileRoot struct {
	Name         string
	ProjectsRoot string
}

type IndexOpts struct {
	ProjectsRoot string // Claude Code's <profile>/projects, one dir per project, *.jsonl transcripts inside
	// Profiles is every profile to index. Empty means just ProjectsRoot,
	// attributed to no profile, which is what a caller that predates several
	// profiles passes and what keeps its behaviour identical.
	Profiles []ProfileRoot
	OutRoot  string // <install-dir>/dashboard/data: the analyses/ reports live under here
	DB       *sql.DB
	Now      func() int64 // Unix seconds; injected for deterministic tests, matching internal/server's NowFn convention
	// Workspace resolves a session's working directories to its display name.
	// Nil means workspace.NewGitResolver, which shells out to git; tests
	// substitute a resolver that answers from a map instead.
	Workspace workspace.Resolver
}

// roots is every profile root this pass walks. ProjectsRoot alone, unnamed,
// when Profiles is empty: a single-profile caller then behaves exactly as it
// did before profiles were a concept.
func (o IndexOpts) roots() []ProfileRoot {
	if len(o.Profiles) > 0 {
		return o.Profiles
	}
	if o.ProjectsRoot == "" {
		return nil
	}
	return []ProfileRoot{{ProjectsRoot: o.ProjectsRoot}}
}

// Stats reports one pass: sessions is the final row count, parsed is how
// many transcripts were actually (re)parsed this pass, and unpriced lists
// every model seen with no price table entry, sorted.
type Stats struct {
	Sessions int
	Parsed   int
	Unpriced []string
}

// RunIndex performs one indexing pass:
// glob every primary transcript, check its (mtime, size, file_count)
// signature against transcript_state, parse only what changed (or has never
// been parsed), and write the sessions table. Rows are permanent: a
// transcript that disappears keeps its row with has_transcript false rather
// than losing history.
func RunIndex(opts IndexOpts) (Stats, error) {
	analysesDir := filepath.Join(opts.OutRoot, "analyses")

	// Built once per pass, not per session: the resolver memoises git's
	// answers per directory, and hundreds of sessions share a few dozen
	// directories between them.
	resolver := opts.Workspace
	if resolver == nil {
		resolver = workspace.NewGitResolver()
	}

	analysed, err := analysedIDs(analysesDir)
	if err != nil {
		return Stats{}, fmt.Errorf("indexer: analysed ids: %w", err)
	}

	// Schema gate: a mismatch (including "never set") invalidates every
	// transcript's freshness signature, forcing a full re-parse this pass.
	// Rows themselves are untouched; only transcript_state is cleared.
	schemaVal, schemaOK, err := db.MetaGet(opts.DB, metaSchema)
	if err != nil {
		return Stats{}, fmt.Errorf("indexer: get schema meta: %w", err)
	}
	if !schemaOK || schemaVal != SchemaVersion {
		if err := db.StatePruneMissing(opts.DB, map[string]bool{}); err != nil {
			return Stats{}, fmt.Errorf("indexer: clear transcript_state: %w", err)
		}
	}

	existing, err := db.ListSessions(opts.DB, "")
	if err != nil {
		return Stats{}, fmt.Errorf("indexer: list sessions: %w", err)
	}
	rows := make(map[string]db.SessionRow, len(existing))
	for _, r := range existing {
		if r.Provider != "" && r.Provider != "claude" {
			continue
		}
		rows[r.ID] = r
	}

	// One flat list across every profile, each path carrying the profile it
	// came from. Session ids are uuids, so two profiles cannot collide on
	// one; the profile travels with the path purely so the row can say where
	// it came from.
	type profilePath struct {
		profile string
		path    string
	}
	var paths []profilePath
	for _, root := range opts.roots() {
		found, err := globSorted(root.ProjectsRoot, "*", "*.jsonl")
		if err != nil {
			return Stats{}, fmt.Errorf("indexer: glob transcripts for %s: %w", root.Name, err)
		}
		for _, f := range found {
			paths = append(paths, profilePath{profile: root.Name, path: f})
		}
	}

	seen := make(map[string]bool, len(paths))
	parsed := 0
	unpriced := map[string]bool{}

	for _, pp := range paths {
		path, profile := pp.path, pp.profile
		sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		seen[sid] = true

		nested, err := globSorted(filepath.Join(filepath.Dir(path), sid, "subagents"), "*.jsonl")
		if err != nil {
			return Stats{}, fmt.Errorf("indexer: glob subagents for %s: %w", sid, err)
		}

		fi, err := os.Stat(path)
		if err != nil {
			return Stats{}, fmt.Errorf("indexer: stat %s: %w", path, err)
		}
		mtime := mtimeSeconds(fi)
		size := fi.Size()
		for _, np := range nested {
			nfi, err := os.Stat(np)
			if err != nil {
				return Stats{}, fmt.Errorf("indexer: stat %s: %w", np, err)
			}
			if m := mtimeSeconds(nfi); m > mtime {
				mtime = m
			}
			size += nfi.Size()
		}
		fileCount := int64(len(nested) + 1)

		prev, prevOK, err := db.StateGet(opts.DB, path)
		if err != nil {
			return Stats{}, fmt.Errorf("indexer: state get %s: %w", path, err)
		}
		fresh := prevOK && prev.Mtime == mtime && prev.Size == size && prev.FileCount == fileCount

		row, hasRow := rows[sid]
		if fresh && (hasRow || (prevOK && prev.Status == "empty")) {
			// hasRow's has_transcript/analysed only need a write when either
			// actually changed (a dead session's transcript reappeared, or an
			// analysis report landed since the last pass): the row is already
			// otherwise up to date, so an unconditional UpsertSession here
			// would just be a redundant write on every steady-state pass.
			if hasRow && (!row.HasTranscript || row.Analysed != analysed[sid] || row.Profile != profile) {
				row.HasTranscript = true
				row.Analysed = analysed[sid]
				// A row indexed before the profile column existed is fresh by
				// signature but still unattributed, so this is what fills it
				// in without forcing a re-parse of a transcript that has not
				// changed.
				row.Profile = profile
				if err := db.UpsertSession(opts.DB, row); err != nil {
					return Stats{}, fmt.Errorf("indexer: refresh %s: %w", sid, err)
				}
			}
			continue
		}

		summary, err := transcript.Parse(path, nested)
		if err != nil {
			return Stats{}, fmt.Errorf("indexer: parse %s: %w", path, err)
		}
		status := "ok"
		if summary == nil {
			status = "empty"
		}
		if err := db.StatePut(opts.DB, db.TranscriptState{
			Path: path, Mtime: mtime, Size: size, FileCount: fileCount, Status: status,
		}); err != nil {
			return Stats{}, fmt.Errorf("indexer: state put %s: %w", path, err)
		}
		parsed++
		if summary == nil {
			continue
		}

		for _, m := range summary.Unpriced {
			unpriced[m] = true
		}

		project := filepath.Base(filepath.Dir(path))
		newRow := db.SessionRow{
			ID:               sid,
			Project:          project,
			Workspace:        workspaceName(summary.Cwds, project, resolver),
			Profile:          profile,
			Start:            summary.Start,
			End:              summary.End,
			CostUSD:          summary.CostUSD,
			Requests:         summary.Requests,
			Tokens:           summary.Tokens,
			SidechainCostUSD: summary.SidechainCostUSD,
			ToolCalls:        summary.ToolCalls,
			UserTurns:        summary.UserTurns,
			Models:           summary.Models,
			Skills:           summary.Skills,
			MCPServers:       summary.MCPServers,
			Tools:            summary.Tools,
			Trajectory:       summary.Trajectory,
			HasTranscript:    true,
			Analysed:         analysed[sid],
		}
		if err := db.UpsertSession(opts.DB, newRow); err != nil {
			return Stats{}, fmt.Errorf("indexer: upsert %s: %w", sid, err)
		}
	}

	// Permanence: every row not seen this pass loses has_transcript (and gets
	// its analysed flag refreshed) but is never deleted.
	if err := db.MarkMissing(opts.DB, seen, analysed); err != nil {
		return Stats{}, fmt.Errorf("indexer: mark missing: %w", err)
	}

	keep := make(map[string]bool, len(paths))
	for _, pp := range paths {
		keep[pp.path] = true
	}
	if err := db.StatePruneMissing(opts.DB, keep); err != nil {
		return Stats{}, fmt.Errorf("indexer: prune transcript_state: %w", err)
	}
	if err := db.MetaSet(opts.DB, metaSchema, SchemaVersion); err != nil {
		return Stats{}, fmt.Errorf("indexer: set schema meta: %w", err)
	}

	final, err := db.ListSessions(opts.DB, "")
	if err != nil {
		return Stats{}, fmt.Errorf("indexer: final list: %w", err)
	}

	generatedAt := opts.Now()
	if err := db.MetaSet(opts.DB, metaGeneratedAt, strconv.FormatInt(generatedAt, 10)); err != nil {
		return Stats{}, fmt.Errorf("indexer: set generated_at meta: %w", err)
	}

	unpricedList := make([]string, 0, len(unpriced))
	for m := range unpriced {
		unpricedList = append(unpricedList, m)
	}
	sort.Strings(unpricedList)

	return Stats{Sessions: len(final), Parsed: parsed, Unpriced: unpricedList}, nil
}

// workspaceName resolves one session's display name, falling back to its
// project key with the encoded "-Users-<name>-" prefix stripped when the
// transcript recorded no working directory at all. That fallback is the same
// string the dashboard showed before this field existed, so a session with no
// cwd reads exactly as it used to rather than going blank.
func workspaceName(cwds []string, project string, r workspace.Resolver) string {
	if name := workspace.Name(cwds, r); name != "" {
		return name
	}
	return pricing.ShortProject(project)
}

// analysedIDs returns the set of session ids with a completed analysis report:
// <analysesDir>/<sid>/report.json (the structured report) or report.md (the
// fallback the runner writes when a report will not parse, and the only format
// reports written before the structured one exist in) must exist as a regular
// file. A missing analyses directory yields an empty (non-nil) set rather than
// an error, since no analysis having run yet is not a failure.
func analysedIDs(analysesDir string) (map[string]bool, error) {
	entries, err := os.ReadDir(analysesDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", analysesDir, err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, name := range []string{"report.json", "report.md"} {
			if fi, err := os.Stat(filepath.Join(analysesDir, e.Name(), name)); err == nil && !fi.IsDir() {
				out[e.Name()] = true
				break
			}
		}
	}
	return out, nil
}

// globSorted joins elem into a glob pattern and returns its matches sorted
// lexicographically, so a pass visits transcripts in a stable order.
func globSorted(elem ...string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(elem...))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// mtimeSeconds reports fi's modification time as Unix seconds with
// fractional precision. It is a pure
// function of fi.ModTime(), so two Stat calls against an untouched file
// return bit-identical results, which is what the signature comparison in
// RunIndex relies on for equality rather than an epsilon.
func mtimeSeconds(fi os.FileInfo) float64 {
	return float64(fi.ModTime().UnixNano()) / 1e9
}
