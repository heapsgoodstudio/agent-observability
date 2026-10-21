package server

import (
	"database/sql"
	"errors"

	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/pricing"
	"github.com/heapsgoodstudio/agent-observability/internal/rules"
)

// errNoDB is returned wherever a database-backed read below has no *sql.DB
// to read from (Options.DB is nil until the first successful sqlite open;
// main.go degrades to that rather than failing the whole server). It plays
// the same role readIndex's os.ErrNotExist used to: "there is nothing to
// read yet."
var errNoDB = errors.New("no database configured")

// errNeverIndexed is returned wherever a database-backed read below has a
// real db but no completed indexing pass yet: the sessions_generated_at
// meta row RunIndex writes at the end of every pass (regardless of how many
// sessions it found) is absent. This is the other half of readIndex's old
// os.ErrNotExist: a migrated-but-never-indexed db has zero rows and no
// error from db.ListSessions alone, which would otherwise be
// indistinguishable from a legitimately empty, already-completed index (a
// present meta row, zero sessions) and silently turn a missing-index
// failure into a quiet success.
var errNeverIndexed = errors.New("no index yet: has claude-obs index run?")

// listIndexedSessions is db.ListSessions with the two guards every caller in
// this file needs: a nil *sql.DB (errNoDB) and a db that has never completed
// an indexing pass (errNeverIndexed, checked via sessionsGeneratedAtKey,
// the same meta row health.go's indexerCheck already gates on) both report
// as "nothing to read yet", matching readIndex's pre-Phase-2d ENOENT for a
// missing sessions/index.json. Only a db that has actually completed at
// least one pass (even with zero sessions) is treated as a real, empty
// read. Shared by GetHealth (session count and generation age, health.go's
// indexerCheck computes this itself rather than through this function, but
// against the same meta key), GetSpend (the session.id -> project join,
// spend.go's buildDelivery and buildProjectAttribution) and GetFindings
// (the per-session facts the rules read from, findings.go's buildFindings).
func listIndexedSessions(d *sql.DB, profile string) ([]db.SessionRow, error) {
	if d == nil {
		return nil, errNoDB
	}
	if _, ok, err := db.MetaGet(d, sessionsGeneratedAtKey); err != nil {
		return nil, err
	} else if !ok {
		return nil, errNeverIndexed
	}
	return db.ListSessions(d, profile)
}

// projectsFromSessions builds the session.id -> project join GetSpend's
// delivery panel and by-project attribution both need. Every row from
// db.ListSessions already has a non-empty id (the table's primary key), so
// unlike the JSON-era indexToProjects this never has a malformed row to
// skip.
func projectsFromSessions(rows []db.SessionRow) map[string]string {
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Project
	}
	return out
}

// workspacesFromSessions is the same join keyed on the descriptive name
// instead, for panels whose figures come only from live counters.
//
// It cannot replace projectsFromSessions in the by-project cost attribution,
// and the reason is worth stating: that attribution merges the backfilled
// history series, which the backfill labelled with the encoded project
// directory, against the live counter, which it joins through this map. One
// project directory holds sessions belonging to several workspaces (a
// workspace root with three repos under it is one directory and three names),
// so there is no way to relabel the historical half to match. Keying the live
// half on workspace while history stayed on project would split one project
// into two rows, half its money in each.
//
// A row indexed before the workspace column existed falls back to its project
// key, so a stale row groups where it always did rather than under "".
func workspacesFromSessions(rows []db.SessionRow) map[string]string {
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if r.Workspace != "" {
			out[r.ID] = r.Workspace
			continue
		}
		out[r.ID] = pricing.ShortProject(r.Project)
	}
	return out
}

// sessionFactsFromSessions projects the sessions table down to just what the
// rules read. Models
// converts db.SessionRow's ordered []transcript.ModelCost into the
// model-name -> dollars map rules.SessionFacts wants; MCPServers and the two
// cache token fields carry straight across.
func sessionFactsFromSessions(rows []db.SessionRow) rules.IndexFacts {
	sessions := make([]rules.SessionFacts, 0, len(rows))
	for _, r := range rows {
		if providerOf(r) != "claude" {
			continue
		}
		models := make(map[string]float64, len(r.Models))
		for _, m := range r.Models {
			models[m.Model] = m.CostUSD
		}
		sessions = append(sessions, rules.SessionFacts{
			CacheRead:     float64(r.Tokens.CacheRead),
			CacheCreation: float64(r.Tokens.CacheCreation),
			Models:        models,
			MCPServers:    r.MCPServers,
		})
	}
	return rules.IndexFacts{Sessions: sessions}
}
