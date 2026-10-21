package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"connectrpc.com/connect"
	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/analysis"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/report"
)

// StartAnalysis, GetAnalysis, StartAdvice and GetAdvice: the four RPCs that
// front internal/analysis's Runner. GetMeta's own analysis fields live in
// server.go, since GetMeta itself does.

// analysisIDRE mirrors internal/analysis's own (unexported) uuidRE: a
// lowercase, hyphenated UUID. Duplicated here rather than exported from
// analysis, so this package consumes that one as-is: the
// Runner's own methods (AnalysisStatus, StartAnalysis) do not themselves
// validate this shape for a bare lookup, so GetAnalysis needs its own check
// to answer CodeInvalidArgument for a malformed id before ever calling the
// Runner.
var analysisIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// errRunnerNotConfigured is returned (as CodeInternal) by all four RPCs when
// Options.Runner is nil, rather than letting a nil-pointer call panic.
var errRunnerNotConfigured = fmt.Errorf("analysis: runner not configured")

// runnerBusy reports whether r has a run in flight, of either kind,
// treating a nil Runner as not busy rather than panicking. GetAnalysis and
// GetAdvice both already answer CodeInternal before ever reaching this call
// when Options.Runner is nil (errRunnerNotConfigured, below), so the nil
// case here is a defensive floor rather than a path either RPC actually
// takes: it keeps RunnerBusy's meaning ("no run in flight") coherent on its
// own terms, independent of that earlier guard.
func runnerBusy(r *analysis.Runner) bool {
	if r == nil {
		return false
	}
	return r.Busy()
}

// errAnalysisBusy carries the analyse path's own 409 wording. Runner's
// shared ErrBusy sentinel is worded for the advice path ("another run is
// already in progress"; see analysis.ErrBusy) since both StartAnalysis and
// StartAdvice claim the same lock through it. The analyse path's own message
// has to read "another analysis is already running", so StartAnalysis
// substitutes this in place of the sentinel's own text whenever it sees
// ErrBusy.
var errAnalysisBusy = fmt.Errorf("another analysis is already running")

// sortedKeys returns m's keys in ascending order, used by GetMeta to report
// analysis.Models/analysis.Depths (both map[string]bool sets) as stable,
// sorted lists.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// analysisRunToProto converts one db.AnalysisRun into its wire shape.
// Subagents nil (unknown; never zero, per the Runner's own contract) maps to
// proto3's implicit zero default plus HasSubagents=false, mirroring how
// GetFindingsResponse.LogsStartTs uses a HasLogsStart-style bool alongside a
// value that means nothing on its own.
func analysisRunToProto(r *db.AnalysisRun) *claudeobsv1.AnalysisRun {
	out := &claudeobsv1.AnalysisRun{
		State:    r.State,
		Provider: r.Provider, Profile: r.Profile, SourceProvider: r.SourceProvider, SourceProfile: r.SourceProfile, SourceAccountKey: r.SourceAccountKey,
		Model:        r.Model,
		Depth:        r.Depth,
		StartedAt:    float64(r.StartedAt),
		FinishedAt:   float64(r.FinishedAt),
		Error:        r.Error,
		RunSessionId: r.RunSessionID,
	}
	if r.Subagents != nil {
		out.Subagents = float64(*r.Subagents)
		out.HasSubagents = true
	}
	return out
}

// adviceFindingsJSON assembles StartAdvice's findings payload through the
// exact path GetFindings uses (buildFindings: same loaders, same rules;
// never a second, StartAdvice-only copy of the loaders), then serializes the
// wire Finding shape (id, severity, title, evidence, fix, window, impact;
// claudeobsv1.Finding's own json tags already carry those exact lowercase
// names) with json.MarshalIndent(v, "", " "), the shape the
// {FINDINGS_JSON} placeholder in internal/analysis/prompts/advice-prompt.md
// was written against.
//
// A failure to assemble or serialize degrades to "[]" rather than blocking
// the run, logged as a section-error-style line: an advice run with no
// findings to quote is still worth having, so a findings failure must not be
// able to block one.
func adviceFindingsJSON(ctx context.Context, o Options, scopes ...dashboardScope) []byte {
	resp, err := buildFindings(ctx, o, scopes...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "advise: findings assembly failed (section=findings), degrading to []:", err)
		return []byte("[]")
	}
	data, err := json.MarshalIndent(resp.Findings, "", " ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "advise: findings serialize failed (section=findings), degrading to []:", err)
		return []byte("[]")
	}
	return data
}

// StartAnalysis validates and dispatches one analyse run through the Runner.
// Sentinel-to-code mapping (brief): ErrBadSessionID/ErrBadModel ->
// InvalidArgument, ErrNoTranscript/ErrNotIndexed -> NotFound, ErrBusy ->
// AlreadyExists (with the analyse path's own wording, not the sentinel's
// shared one), everything else -> Internal.
func (s *obsService) StartAnalysis(ctx context.Context, req *connect.Request[claudeobsv1.StartAnalysisRequest]) (*connect.Response[claudeobsv1.StartAnalysisResponse], error) {
	if strings.HasPrefix(req.Msg.Id, "codex:") {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("analysis is currently available for Claude sessions"))
	}
	if s.o.RunnerForTarget != nil {
		if s.o.DB == nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("session database unavailable"))
		}
		row, ok, err := db.GetSession(s.o.DB, req.Msg.Id)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
		if !ok {
			return nil, connect.NewError(connect.CodeNotFound, analysis.ErrNotIndexed)
		}
		o := s.o
		o.Runner, err = o.RunnerForTarget(analysis.Selection{Provider: req.Msg.Provider, Profile: req.Msg.Profile, SourceProvider: row.Provider, SourceProfile: row.Profile})
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		o.RunnerForTarget = nil
		o.RunnerForProfile = nil
		return (&obsService{o: o}).StartAnalysis(ctx, req)
	}
	if s.o.RunnerForProfile != nil {
		o := s.o
		if o.DB == nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("session database unavailable"))
		}
		row, ok, err := db.GetSession(o.DB, req.Msg.Id)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read session index: %w", err))
		}
		if !ok {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not indexed"))
		}
		o.Runner = o.RunnerForProfile(row.Profile)
		o.RunnerForProfile = nil
		return (&obsService{o: o}).StartAnalysis(ctx, req)
	}
	if s.o.Runner == nil {
		return nil, connect.NewError(connect.CodeInternal, errRunnerNotConfigured)
	}
	switch err := s.o.Runner.StartAnalysis(req.Msg.Id, req.Msg.Model); err {
	case nil:
		return connect.NewResponse(&claudeobsv1.StartAnalysisResponse{}), nil
	case analysis.ErrBadSessionID, analysis.ErrBadModel:
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	case analysis.ErrNoTranscript, analysis.ErrNotIndexed:
		return nil, connect.NewError(connect.CodeNotFound, err)
	case analysis.ErrBusy:
		return nil, connect.NewError(connect.CodeAlreadyExists, errAnalysisBusy)
	default:
		return nil, connect.NewError(connect.CodeInternal, err)
	}
}

// GetAnalysis reads back one session's analyse run. A malformed id is
// rejected before ever reaching the Runner; a well-formed id the Runner has
// never heard of (no db row, no status.json) is CodeNotFound.
func (s *obsService) GetAnalysis(ctx context.Context, req *connect.Request[claudeobsv1.GetAnalysisRequest]) (*connect.Response[claudeobsv1.GetAnalysisResponse], error) {
	id := req.Msg.Id
	if !analysis.ValidSessionID(id) {
		return nil, connect.NewError(connect.CodeInvalidArgument, analysis.ErrBadSessionID)
	}
	if s.o.Runner == nil {
		return nil, connect.NewError(connect.CodeInternal, errRunnerNotConfigured)
	}
	row, ok, err := s.o.Runner.AnalysisStatus(id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("analysis: no run recorded for %s", id))
	}

	resp := &claudeobsv1.GetAnalysisResponse{
		Identity:   identity(),
		Run:        analysisRunToProto(row),
		TimeoutS:   int32(s.o.analysisTimeout().Seconds()),
		RunnerBusy: runnerBusy(s.o.Runner),
	}
	if row.State == "done" {
		resp.Report, resp.ReportMd = loadReport(filepath.Join(s.o.DataDir, "analyses", id))
		reclassifyBlockedReport(resp.Run, &resp.ReportMd)
	}
	return connect.NewResponse(resp), nil
}

// StartAdvice assembles this run's findings server-side (adviceFindingsJSON)
// and dispatches through the Runner. Sentinel-to-code mapping (brief):
// ErrBadModel/ErrBadDepth -> InvalidArgument, ErrBusy -> AlreadyExists (the
// sentinel's own wording this time: "another run is already in progress" is
// already the advice path's message), ErrFindingsTooLarge ->
// ResourceExhausted, everything else -> Internal.
func (s *obsService) StartAdvice(ctx context.Context, req *connect.Request[claudeobsv1.StartAdviceRequest]) (*connect.Response[claudeobsv1.StartAdviceResponse], error) {
	scope, err := resolveDashboardScope(ctx, s.o, req.Msg.Provider, req.Msg.AccountKey, req.Msg.Profile, s.o.now())
	if err != nil {
		return nil, err
	}
	if !analysis.ValidModel(req.Msg.Model) {
		return nil, connect.NewError(connect.CodeInvalidArgument, analysis.ErrBadModel)
	}
	if req.Msg.Depth != "" && !analysis.Depths[req.Msg.Depth] {
		return nil, connect.NewError(connect.CodeInvalidArgument, analysis.ErrBadDepth)
	}
	evidenceScope, err := scopedAdviceEvidence(s.o, scope)
	if err != nil {
		return nil, err
	}
	runner := s.o.Runner
	if s.o.RunnerForTarget != nil {
		if req.Msg.ExecutionProfile == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("select an enabled execution profile"))
		}
		runner, err = s.o.RunnerForTarget(analysis.Selection{Provider: req.Msg.ExecutionProvider, Profile: req.Msg.ExecutionProfile, SourceProvider: scope.Provider, SourceProfile: scope.Profile, SourceAccountKey: scope.AccountKey, SourceProfiles: findingsProfiles(scope)})
		if err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
	} else if s.o.Profiles != nil {
		if req.Msg.ExecutionProfile == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("select a configured Claude execution profile before generating advice; the run consumes its allowance"))
		}
		ps, err := s.o.Profiles.List()
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
		eligible := false
		for _, p := range ps {
			if p.ID == req.Msg.ExecutionProfile && p.Provider == profiles.Claude && p.Enabled {
				eligible = true
			}
		}
		if !eligible {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("execution profile must be a configured enabled Claude connection"))
		}
		if s.o.RunnerForProfile == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("profile-specific runner is unavailable"))
		}
		runner = s.o.RunnerForProfile(req.Msg.ExecutionProfile)
	} else if req.Msg.ExecutionProfile != "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("execution profile cannot be verified without the profile registry"))
	} else if s.o.RunnerForProfile != nil {
		runner = s.o.RunnerForProfile("")
	}
	if runner == nil {
		return nil, connect.NewError(connect.CodeInternal, errRunnerNotConfigured)
	}
	findingsJSON := adviceFindingsJSON(ctx, s.o, scope)
	_, err = runner.StartScopedAdvice(req.Msg.Model, req.Msg.Depth, findingsJSON, evidenceScope, req.Msg.ExecutionProfile)
	switch err {
	case nil:
		return connect.NewResponse(&claudeobsv1.StartAdviceResponse{}), nil
	case analysis.ErrBadModel, analysis.ErrBadDepth:
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	case analysis.ErrBusy:
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	case analysis.ErrFindingsTooLarge:
		return nil, connect.NewError(connect.CodeResourceExhausted, err)
	default:
		return nil, connect.NewError(connect.CodeInternal, err)
	}
}

// GetAdvice reads the selected scope, retaining the legacy aggregate slot. timeout_s follows the
// same rule the JS stuck-thresholds encode today (see root CLAUDE.md's
// "Stuck thresholds and runner timeouts" entry): the evidence timeout when
// the recorded depth is "evidence", the analysis timeout otherwise.
func (s *obsService) GetAdvice(ctx context.Context, req *connect.Request[claudeobsv1.GetAdviceRequest]) (*connect.Response[claudeobsv1.GetAdviceResponse], error) {
	scope, err := resolveDashboardScope(ctx, s.o, req.Msg.Provider, req.Msg.AccountKey, req.Msg.Profile, s.o.now())
	if err != nil {
		return nil, err
	}
	evidenceScope := analysis.AdviceScope{Provider: scope.Provider, AccountKey: scope.AccountKey, Profile: scope.Profile}
	if s.o.Runner == nil {
		return nil, connect.NewError(connect.CodeInternal, errRunnerNotConfigured)
	}
	row, ok, err := s.o.Runner.ScopedAdviceStatus(evidenceScope)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("advice: no run has started for this scope"))
	}

	timeout := s.o.analysisTimeout()
	if row.Depth == "evidence" {
		timeout = s.o.evidenceTimeout()
	}
	resp := &claudeobsv1.GetAdviceResponse{
		Identity: identity(), Provider: scope.Provider, AccountKey: scope.AccountKey, Profile: scope.Profile, ScopeNote: findingsScopeNote(scope), ExecutionProfile: row.ExecutionProfile,
		Run:        analysisRunToProto(row),
		TimeoutS:   int32(timeout.Seconds()),
		RunnerBusy: runnerBusy(s.o.Runner),
	}
	if row.ScopeJSON == "" && row.SourceProvider == "claude" {
		resp.ScopeNote = "This saved report covers Claude evidence only. Generate a new report to review the current source scope."
	}
	if row.ScopeJSON != "" {
		var savedScope analysis.AdviceScope
		if json.Unmarshal([]byte(row.ScopeJSON), &savedScope) == nil {
			if savedScope.Note != "" {
				resp.ScopeNote = savedScope.Note
			}
			if len(savedScope.Profiles) > 0 {
				resp.ScopeNote += " Saved evidence profiles: " + strings.Join(savedScope.Profiles, ", ") + "."
			}
		}
	}
	if row.State == "done" {
		resp.Report, resp.ReportMd = loadReport(analysis.AdviceReportDir(s.o.DataDir, evidenceScope))
		reclassifyBlockedReport(resp.Run, &resp.ReportMd)
	}
	return connect.NewResponse(resp), nil
}

// Older runners saved zero-exit hook failures as successful markdown reports.
// Surface the real failure without returning the echoed prompt to the browser
// or rewriting the user's saved artifact while handling a read request.
func reclassifyBlockedReport(run *claudeobsv1.AnalysisRun, markdown *string) {
	if err := report.ExecutionError([]byte(*markdown)); err != nil {
		run.State, run.Error = "error", err.Error()
		*markdown = ""
	}
}

// Configuration evidence is selected by the same roster membership as session
// evidence, independently from the profile which authenticates the paid run.
func scopedAdviceEvidence(o Options, s dashboardScope) (analysis.AdviceScope, error) {
	scope := analysis.AdviceScope{Provider: s.Provider, AccountKey: s.AccountKey, Profile: s.Profile, Profiles: findingsProfiles(s), Note: findingsScopeNote(s)}
	if o.Profiles == nil {
		return scope, nil
	}
	ps, err := o.Profiles.List()
	if err != nil {
		return scope, connect.NewError(connect.CodeUnavailable, err)
	}
	for _, p := range ps {
		if scope.Provider != "" && string(p.Provider) != scope.Provider {
			continue
		}
		allowed := scope.Profiles == nil
		for _, id := range scope.Profiles {
			if p.ID == id {
				allowed = true
			}
		}
		if allowed {
			scope.Configurations = append(scope.Configurations, analysis.EvidenceProfile{ID: p.ID, Provider: string(p.Provider), Directory: p.Directory})
		}
	}
	return scope, nil
}
