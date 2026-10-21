// Package server is claude-obs serve: the one process on 8427. It serves
// the Connect API plus static content: Options.StaticDir when set (dev/test
// override), else the embedded Next.js build (see static.go), else a 503
// telling the operator to build it.
package server

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"connectrpc.com/connect"
	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1/claudeobsv1connect"
	"github.com/heapsgoodstudio/agent-observability/internal/analysis"
	"github.com/heapsgoodstudio/agent-observability/internal/monitor"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vl"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
	"github.com/heapsgoodstudio/agent-observability/internal/usage"
)

const (
	DashPort = 8427
	VMPort   = 8428
	VLPort   = 9428
)

type Options struct {
	Profiles         *profiles.Store
	Monitor          *monitor.Manager
	RunnerForProfile func(string) *analysis.Runner
	RunnerForTarget  func(analysis.Selection) (*analysis.Runner, error)
	AnalysisBinaries map[string]string
	Addr             string
	StaticDir        string
	InstallDir       string
	Version          string

	VM   *vm.Client
	VL   *vl.Client
	Memo *store.Memo

	// DB is state/claude-obs.db, the sqlite store internal/db and
	// internal/indexer own. ListSessions and GetSession read the sessions
	// table here; a nil DB degrades those two RPCs to live-only data with a
	// SectionError rather than panicking.
	DB *sql.DB

	// DataDir is <install-dir>/dashboard/data: analyses/ and advice/ reports
	// live here, read directly off disk by loadReport (report.go). A run lands
	// either a report.json, the structured document internal/report validates,
	// or a report.md holding output that would not parse as one.
	DataDir string

	// LaunchAgentsDir is where this profile's rendered launchd plists live,
	// normally $HOME/Library/LaunchAgents. An Options field so tests can
	// point it at a temp dir instead of the real one.
	LaunchAgentsDir string

	// Runner drives the four analysis RPCs: StartAnalysis, GetAnalysis,
	// StartAdvice, GetAdvice. A nil Runner (a server built
	// without one) makes those four RPCs answer CodeInternal instead of
	// panicking.
	Runner *analysis.Runner

	// AnalysisTimeout and EvidenceTimeout mirror the same-named fields on
	// the Runner's own analysis.Config: the Runner does not expose its
	// config back out, so GetMeta/GetAnalysis/GetAdvice read the values
	// configured here instead. Zero means the analysis package's own
	// defaults (1500s/2700s), matching analysis.New's own zero-value
	// handling, so a server built without setting these still reports sane
	// timeouts.
	AnalysisTimeout time.Duration
	EvidenceTimeout time.Duration

	// PrimaryProfile is the profile name config.env's PROFILE_DIR resolves
	// to, which is the profile this install was originally pointed at. Its
	// account is what GetOverview reports in the flat, pre-multi-account
	// fields and names as primary_account, so an install instrumenting one
	// profile reads exactly as it did before several were possible.
	PrimaryProfile string

	// Explicit operator attribution of billing from the old single-account
	// collector. Empty disables it. A current primary profile is not evidence
	// of historical ownership. Uses the provider-qualified dashboard account key.
	LegacyClaudeBillingAccount string

	// CodexCreditUSD is config.env's CODEX_CREDIT_USD, the dollar price of
	// one Codex credit. Zero leaves Codex overage in credits with no dollar
	// estimate. OpenAI publishes no fixed price; it depends on the plan.
	CodexCreditUSD float64

	// ProfileDirs is every instrumented profile's config directory, in
	// PROFILE_DIRS order, primary first. GetUsageSnapshot needs the
	// directories rather than the names, because a probe is spawned against
	// a CLAUDE_CONFIG_DIR. Empty disables on-demand probing, which leaves
	// that RPC serving the store, still a correct answer with its own age
	// on it.
	ProfileDirs []string

	// Prober fetches one profile's live usage, for GetUsageSnapshot's
	// max_age_s. Nil disables on-demand probing the same way an empty
	// ProfileDirs does.
	Prober *usage.Prober

	// NowFn returns the current Unix time in seconds; nil means
	// time.Now().Unix(), which is real behaviour. The three RPCs that
	// derive query windows from "now" (GetOverview, GetSpend, GetHealth)
	// read through this instead of calling time.Now() directly, so this
	// package's own tests can pin it to a fixed instant and assert exact
	// window boundaries instead of a moving target.
	NowFn func() int64
}

// now returns o.NowFn() when set, else the real wall clock. The one thing
// every one of GetOverview/GetSpend/GetHealth calls instead of time.Now()
// directly.
func (o Options) now() int64 {
	if o.NowFn != nil {
		return o.NowFn()
	}
	return time.Now().Unix()
}

// analysisTimeout returns o.AnalysisTimeout, or analysis.DefaultAnalysisTimeout
// when unset.
func (o Options) analysisTimeout() time.Duration {
	if o.AnalysisTimeout > 0 {
		return o.AnalysisTimeout
	}
	return analysis.DefaultAnalysisTimeout
}

// evidenceTimeout returns o.EvidenceTimeout, or analysis.DefaultEvidenceTimeout
// when unset.
func (o Options) evidenceTimeout() time.Duration {
	if o.EvidenceTimeout > 0 {
		return o.EvidenceTimeout
	}
	return analysis.DefaultEvidenceTimeout
}

type obsService struct {
	o            Options
	modelCatalog analysis.ModelCatalog
}

func (s *obsService) GetMeta(ctx context.Context, req *connect.Request[claudeobsv1.GetMetaRequest]) (*connect.Response[claudeobsv1.GetMetaResponse], error) {
	return connect.NewResponse(&claudeobsv1.GetMetaResponse{
		Identity:               identity(),
		Version:                s.o.Version,
		InstallDir:             s.o.InstallDir,
		DashPort:               DashPort,
		VmPort:                 VMPort,
		VlPort:                 VLPort,
		AnalysisTimeoutS:       int32(s.o.analysisTimeout().Seconds()),
		AdviceEvidenceTimeoutS: int32(s.o.evidenceTimeout().Seconds()),
		AnalysisModels:         sortedKeys(analysis.Models),
		AnalysisTargets:        s.analysisTargets(ctx), RunnerBusy: runnerBusy(s.o.Runner),
		AdviceDepths: sortedKeys(analysis.Depths),
	}), nil
}

func (s *obsService) GetOverview(ctx context.Context, req *connect.Request[claudeobsv1.GetOverviewRequest]) (*connect.Response[claudeobsv1.GetOverviewResponse], error) {
	resp, err := buildOverview(ctx, s.options(), req.Msg.Profile, s.o.now())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(resp), nil
}

func (s *obsService) GetSpend(ctx context.Context, req *connect.Request[claudeobsv1.GetSpendRequest]) (*connect.Response[claudeobsv1.GetSpendResponse], error) {
	resp, err := buildSpend(ctx, s.options(), req.Msg.Period, req.Msg.Measure, req.Msg.Project, req.Msg.Profile, s.o.now())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(resp), nil
}

func (s *obsService) GetFindings(ctx context.Context, req *connect.Request[claudeobsv1.GetFindingsRequest]) (*connect.Response[claudeobsv1.GetFindingsResponse], error) {
	scope, err := resolveDashboardScope(ctx, s.o, req.Msg.Provider, req.Msg.AccountKey, req.Msg.Profile, s.o.now())
	if err != nil {
		return nil, err
	}
	resp, err := buildFindings(ctx, s.o, scope)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(resp), nil
}

func (s *obsService) GetHealth(ctx context.Context, req *connect.Request[claudeobsv1.GetHealthRequest]) (*connect.Response[claudeobsv1.GetHealthResponse], error) {
	return connect.NewResponse(buildConfiguredHealth(ctx, s.options(), s.o.now())), nil
}

func (s *obsService) ListSessions(ctx context.Context, req *connect.Request[claudeobsv1.ListSessionsRequest]) (*connect.Response[claudeobsv1.ListSessionsResponse], error) {
	if err := checkProvider(req.Msg.Provider); err != nil {
		return nil, err
	}
	resp, err := buildScopedSessions(ctx, s.options(), req.Msg, s.o.now())
	if err != nil {
		return nil, dashboardError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *obsService) ListProfiles(ctx context.Context, req *connect.Request[claudeobsv1.ListProfilesRequest]) (*connect.Response[claudeobsv1.ListProfilesResponse], error) {
	resp, err := buildConfiguredProfiles(ctx, s.options(), s.o.now())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(resp), nil
}

func (s *obsService) GetUsageSnapshot(ctx context.Context, req *connect.Request[claudeobsv1.GetUsageSnapshotRequest]) (*connect.Response[claudeobsv1.GetUsageSnapshotResponse], error) {
	resp, err := buildFilteredUsageSnapshot(ctx, s.options(), req.Msg, s.o.now())
	if err != nil {
		return nil, dashboardError(err)
	}
	return connect.NewResponse(resp), nil
}

func (s *obsService) GetSession(ctx context.Context, req *connect.Request[claudeobsv1.GetSessionRequest]) (*connect.Response[claudeobsv1.GetSessionResponse], error) {
	resp, err := buildProviderSession(ctx, s.options(), req.Msg.Id, s.o.now())
	if err != nil {
		if isSessionNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(resp), nil
}

// StartAnalysis, GetAnalysis, StartAdvice and GetAdvice live in analysis.go.

func Handler(o Options) http.Handler {
	mux := http.NewServeMux()
	path, h := claudeobsv1connect.NewObsServiceHandler(&obsService{o: o})
	mux.Handle(path, h)
	mux.Handle("/", staticHandler(o, embeddedRoot()))
	mux.Handle("/ingest/codex/", codexIngest(o))
	return protectSettingsWrites(mux)
}
