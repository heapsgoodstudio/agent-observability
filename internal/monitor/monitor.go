// Package monitor schedules local indexing and read-only account snapshots.
// Persisted snapshots let the dashboard survive restarts and show stale data
// honestly when an agent or its account is temporarily unavailable.
package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/indexer"
	"github.com/heapsgoodstudio/agent-observability/internal/omp"
	"github.com/heapsgoodstudio/agent-observability/internal/opencode"
	"github.com/heapsgoodstudio/agent-observability/internal/payg"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

type ImportStatus struct {
	State      string `json:"state"`
	Profile    string `json:"profile"`
	Message    string `json:"message"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
	Sessions   int    `json:"sessions"`
}

type Snapshot struct {
	CollectedAt       int64             `json:"collected_at"`
	Error             string            `json:"error,omitempty"`
	Warnings          []string          `json:"warnings,omitempty"`
	UsageGaps         map[string]string `json:"usage_gaps,omitempty"`
	Usage             codex.Usage       `json:"usage"`
	UsageError        string            `json:"usage_error,omitempty"`
	UsageAttemptedAt  int64             `json:"usage_attempted_at,omitempty"`
	UsageDirectory    string            `json:"usage_directory,omitempty"`
	NotificationError string            `json:"notification_error,omitempty"`
}

type Manager struct {
	queueMu       sync.Mutex
	wake          chan struct{}
	Dir           string
	DB            *sql.DB
	Store         *profiles.Store
	CodexBinary   string
	VMURL         string
	OutRoot       string
	mu            sync.Mutex
	running       bool
	status        ImportStatus
	workers       sync.WaitGroup
	runContext    context.Context
	closed        bool
	quotaInterval time.Duration
	now           func() time.Time
	// Injectable collectors keep integration tests independent of local accounts.
	Collect   func(context.Context, codex.Options) (codex.Result, error)
	ReadUsage func(context.Context, codex.Options) (codex.Usage, error)
	// Spend holds one collector per pay-as-you-go harness (Oh My Pi,
	// opencode), keyed by the profile provider it collects.
	Spend            map[profiles.Provider]SpendCollector
	SendNotification NotificationSender
}

// SpendResult is one collection of a pay-as-you-go harness profile.
type SpendResult struct {
	Sources  map[string]db.SessionSource
	Sessions []db.SessionRow
	Usage    []payg.Bucket
	Warnings []string
}

// SpendCollector collects one pay-as-you-go harness profile from its
// directory. Each harness package supplies one through an adapter below.
type SpendCollector func(ctx context.Context, dir, profileID string) (SpendResult, error)

// SpendSnapshot is one pay-as-you-go profile's last collection. Usage holds
// the per day, provider and model totals the dashboard reads, so no view needs
// a second pass over the harness's data.
type SpendSnapshot struct {
	CollectedAt int64         `json:"collected_at"`
	Error       string        `json:"error,omitempty"`
	Warnings    []string      `json:"warnings,omitempty"`
	Directory   string        `json:"directory,omitempty"`
	Usage       []payg.Bucket `json:"usage,omitempty"`
}

// SpendSnapshotKey is where a pay-as-you-go profile's snapshot lives in the
// meta table: "omp.snapshot.<id>", "opencode.snapshot.<id>". The omp form is
// the key the first release wrote, so it must not change.
func SpendSnapshotKey(provider profiles.Provider, profileID string) string {
	return string(provider) + ".snapshot." + profileID
}

func ompSpend(c *omp.Collector) SpendCollector {
	return func(ctx context.Context, dir, id string) (SpendResult, error) {
		r, err := c.Collect(ctx, omp.Options{Home: dir, ProfileID: id})
		return SpendResult{Sources: r.Sources, Sessions: r.Sessions, Usage: r.Usage, Warnings: r.Warnings}, err
	}
}

func opencodeSpend(c *opencode.Collector) SpendCollector {
	return func(ctx context.Context, dir, id string) (SpendResult, error) {
		r, err := c.Collect(ctx, opencode.Options{Dir: dir, ProfileID: id})
		return SpendResult{Sources: r.Sources, Sessions: r.Sessions, Usage: r.Usage, Warnings: r.Warnings}, err
	}
}

func New(dir string, d *sql.DB) *Manager {
	return &Manager{
		Dir: dir, DB: d, Store: profiles.New(dir), VMURL: "http://127.0.0.1:8428",
		Collect: codex.NewCollector().Collect, ReadUsage: codex.ReadUsage,
		Spend: map[profiles.Provider]SpendCollector{
			profiles.Omp:      ompSpend(omp.NewCollector()),
			profiles.Opencode: opencodeSpend(opencode.NewCollector()),
		},
		wake: make(chan struct{}, 1),
	}
}

func (m *Manager) Status() ImportStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return m.status
	}
	var st ImportStatus
	if m.DB != nil {
		ReadJSON(m.DB, "history_import", &st)
	}
	if st.State == "running" {
		st.State = "failed"
		st.Message = "The previous import was interrupted. Import again to resume."
	}
	if st.State == "" {
		st.State = "idle"
	}
	return st
}

func (m *Manager) setStatus(st ImportStatus) {
	m.mu.Lock()
	m.status = st
	m.mu.Unlock()
	if m.DB != nil {
		WriteJSON(m.DB, "history_import", st)
	}
}

func ReadJSON(d *sql.DB, key string, v any) error {
	if d == nil {
		return errors.New("database unavailable")
	}
	raw, ok, err := db.MetaGet(d, key)
	if err != nil {
		return err
	}
	if !ok {
		return os.ErrNotExist
	}
	return json.Unmarshal([]byte(raw), v)
}
func WriteJSON(d *sql.DB, key string, v any) error {
	if d == nil {
		return errors.New("database unavailable")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return db.MetaSet(d, key, string(raw))
}

// Start returns immediately; only this job can own the import status at a time.
func (m *Manager) Start(profile string, backfill bool) (ImportStatus, error) {
	m.mu.Lock()
	ctx := m.runContext
	m.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return m.start(ctx, profile, backfill, false)
}

func (m *Manager) start(parent context.Context, profile string, backfill, queued bool) (ImportStatus, error) {
	if m.DB == nil {
		return ImportStatus{}, errors.New("session database unavailable")
	}
	ps, err := m.Store.List()
	if err != nil {
		return ImportStatus{}, err
	}
	if profile != "" {
		found := false
		for _, p := range ps {
			if p.ID == profile && p.Enabled {
				found = true
			}
		}
		if !found {
			return ImportStatus{}, errors.New("enabled profile not found")
		}
	}
	m.mu.Lock()
	if m.closed || parent.Err() != nil {
		m.mu.Unlock()
		return ImportStatus{}, errors.New("monitor is shutting down")
	}
	if m.running {
		st := m.status
		m.mu.Unlock()
		return st, errors.New("an import is already running")
	}
	if backfill && !queued {
		if err := m.Enqueue(profile); err != nil {
			m.mu.Unlock()
			return ImportStatus{}, err
		}
	}
	m.running = true
	st := ImportStatus{State: "running", Profile: profile, StartedAt: time.Now().Unix(), Message: "Reading local session history"}
	m.status = st
	m.workers.Add(1)
	m.mu.Unlock()
	WriteJSON(m.DB, "history_import", st)
	go func(st ImportStatus) {
		defer m.workers.Done()
		ctx, cancel := context.WithTimeout(parent, 20*time.Minute)
		defer cancel()
		n, err := m.Refresh(ctx, profile, backfill)
		st.Sessions = n
		st.FinishedAt = time.Now().Unix()
		st.State = "complete"
		st.Message = "History is up to date"
		if err != nil {
			st.State = "failed"
			st.Message = err.Error()
		}
		m.setStatus(st)
		// Keep interrupted work queued for the next process; completed attempts
		// (including visible failures) release the queue for other profiles.
		if backfill && parent.Err() == nil {
			m.acknowledge(profile)
		}
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
		var pending []string
		if ReadJSON(m.DB, "monitor.pending", &pending) == nil && len(pending) > 0 {
			m.notify()
		}
	}(st)
	return st, nil
}

// Enqueue durably records a requested backfill even during another import.
func (m *Manager) Enqueue(profile string) error {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	var pending []string
	ReadJSON(m.DB, "monitor.pending", &pending)
	for _, id := range pending {
		if id == profile {
			m.notify()
			return nil
		}
	}
	pending = append(pending, profile)
	if err := WriteJSON(m.DB, "monitor.pending", pending); err != nil {
		return err
	}
	m.notify()
	return nil
}
func (m *Manager) notify() {
	if m.wake != nil {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
}

// daemonLease lets the scheduled CLI act as a fallback when the server is down.
func (m *Manager) daemonLease() (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(m.Dir, "state", "monitor.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func releaseLease(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }

// RefreshScheduled skips work already owned by the running server.
func (m *Manager) RefreshScheduled(ctx context.Context) (int, error) {
	lease, err := m.daemonLease()
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer releaseLease(lease)
	quotaErr := m.RefreshUsage(ctx)
	n, historyErr := m.Refresh(ctx, "", false)
	return n, errors.Join(quotaErr, historyErr)
}

func (m *Manager) acknowledge(profile string) {
	m.queueMu.Lock()
	defer m.queueMu.Unlock()
	var pending []string
	if ReadJSON(m.DB, "monitor.pending", &pending) != nil {
		return
	}
	for i, id := range pending {
		if id == profile {
			WriteJSON(m.DB, "monitor.pending", append(pending[:i], pending[i+1:]...))
			return
		}
	}
}

func (m *Manager) Run(ctx context.Context) {
	if m.DB == nil {
		return
	}
	// A timed CLI pass may own the lease during server startup. Wait for it
	// rather than abandoning all future monitoring until another restart.
	var lease *os.File
	for {
		var err error
		lease, err = m.daemonLease()
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	defer releaseLease(lease)
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.runContext = ctx
	m.mu.Unlock()
	quotaDone := make(chan struct{})
	go func() { defer close(quotaDone); m.runQuota(ctx) }()
	defer func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		cancel()
		<-quotaDone
		m.workers.Wait()
	}()
	tick := time.NewTicker(5 * time.Minute)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		request := filepath.Join(m.Dir, "state", "import-request.json")
		var body struct {
			Profile string `json:"profile"`
		}
		if data, err := os.ReadFile(request); err == nil && json.Unmarshal(data, &body) == nil {
			if m.Enqueue(body.Profile) == nil {
				os.Remove(request)
			}
		}
		// A profile may be removed or disabled while waiting. Discard just that
		// request, never block the remaining queue or future periodic refreshes.
		ps, err := m.Store.List()
		if err == nil {
			enabled := map[string]bool{"": true}
			for _, p := range ps {
				enabled[p.ID] = p.Enabled
			}
			m.queueMu.Lock()
			var pending []string
			ReadJSON(m.DB, "monitor.pending", &pending)
			active := make([]string, 0, len(pending))
			for _, id := range pending {
				if enabled[id] {
					active = append(active, id)
				}
			}
			if len(active) != len(pending) {
				WriteJSON(m.DB, "monitor.pending", active)
			}
			m.queueMu.Unlock()
			if len(active) > 0 {
				m.start(ctx, active[0], true, true)
			} else {
				m.start(ctx, "", false, false)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-m.wake:
		}
	}
}

// Refresh is shared by the server and scheduled CLI. A filesystem lock prevents
// their launchd schedules from indexing the same installation concurrently.
func (m *Manager) Refresh(ctx context.Context, selected string, backfill bool) (int, error) {
	if m.DB == nil {
		return 0, errors.New("database unavailable")
	}
	lock, err := os.OpenFile(filepath.Join(m.Dir, "state", "index.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return 0, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ps, err := m.Store.List()
	if err != nil {
		return 0, err
	}
	var roots []indexer.ProfileRoot
	var issues []string
	for _, p := range ps {
		if p.Enabled && p.Provider == profiles.Claude {
			roots = append(roots, indexer.ProfileRoot{Name: p.ID, ProjectsRoot: filepath.Join(p.Directory, "projects")})
		}
	}
	// Index every enabled Claude root even for a single-profile import: the
	// indexer's missing-file sweep must see the complete configured roster.
	if len(roots) > 0 {
		if _, err := indexer.RunIndex(indexer.IndexOpts{DB: m.DB, Profiles: roots, OutRoot: m.outputRoot(), Now: func() int64 { return time.Now().Unix() }}); err != nil {
			issues = append(issues, err.Error())
		}
	}
	for _, p := range ps {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if !p.Enabled || (selected != "" && p.ID != selected) {
			continue
		}
		if p.Provider == profiles.Claude {
			if backfill {
				if _, err := indexer.RunBackfill(indexer.BackfillOpts{ProjectsRoot: filepath.Join(p.Directory, "projects"), Profile: p.ID, VMURL: m.VMURL}); err != nil {
					issues = append(issues, p.Name+": "+err.Error())
				}
			}
			continue
		}
		if collect, ok := m.Spend[p.Provider]; ok {
			if err := m.refreshSpend(ctx, p, collect); err != nil {
				issues = append(issues, p.Name+": "+err.Error())
			}
			continue
		}
		if p.Provider != profiles.Codex {
			continue
		}
		snap := Snapshot{}
		ReadJSON(m.DB, "codex.snapshot."+p.ID, &snap)
		if snap.UsageGaps == nil {
			snap.UsageGaps = map[string]string{}
		}
		opts := codex.Options{Home: p.Directory, ProfileID: p.ID, Binary: m.CodexBinary}
		collectCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		res, err := m.Collect(collectCtx, opts)
		cancel()
		snap.Warnings = res.Warnings
		if err != nil {
			snap.Error = err.Error()
			issues = append(issues, p.Name+": "+err.Error())
		} else {
			snap.Error = ""
			snap.CollectedAt = time.Now().Unix()
		}
		// Partial reads can still supply valid replacement summaries. Missing
		// usage must never turn previously known history into zero usage.
		for _, row := range res.Sessions {
			if !row.UsageKnown || !row.ToolsKnown || !row.ErrorsKnown || !row.DurationKnown || res.Details[row.ID].RejectedOnly {
				old, ok, readErr := db.GetSession(m.DB, row.ID)
				if readErr != nil {
					issues = append(issues, readErr.Error())
					continue
				}
				// An earlier rejected/zero-usage turn cannot establish zero for
				// a later interruption in the same session.
				hadUsage := ok && (old.Requests > 0 || old.Tokens.Input > 0 || old.Tokens.Output > 0 || old.Tokens.CacheRead > 0 || old.Tokens.CacheCreation > 0)
				if ok && old.UsageKnown && hadUsage && (!row.UsageKnown || res.Details[row.ID].RejectedOnly) {
					row.Tokens = old.Tokens
					row.Requests = old.Requests
					row.ReasoningTokens = old.ReasoningTokens
					row.CostUSD = old.CostUSD
					row.CostKnown = old.CostKnown
					row.UsageKnown = true
					row.Models = old.Models
					row.Trajectory = old.Trajectory
				}
				if ok && old.ToolsKnown && !row.ToolsKnown {
					row.ToolsKnown = true
					row.Tools = old.Tools
					row.ToolCalls = old.ToolCalls
					row.UserTurns = old.UserTurns
				}
				if ok && old.ErrorsKnown && !row.ErrorsKnown {
					row.ErrorsKnown = true
					row.ErrorCount = old.ErrorCount
				}
				if ok && old.DurationKnown && !row.DurationKnown {
					row.DurationKnown = true
					row.DurationMS = old.DurationMS
				}
			}
			if err := db.UpsertSession(m.DB, row); err != nil {
				snap.Error = err.Error()
				issues = append(issues, err.Error())
				break
			}
			if detail := res.Details[row.ID]; detail.TranscriptPath != "" {
				if err := db.PutSessionSource(m.DB, db.SessionSource{ID: row.ID, Path: detail.TranscriptPath, Cwd: row.Project, Format: "codex"}); err != nil {
					issues = append(issues, err.Error())
				}
			}
			if row.UsageKnown {
				delete(snap.UsageGaps, row.ID)
			} else {
				snap.UsageGaps[row.ID] = res.Details[row.ID].UsageUnavailableReason
			}
		}

		// Quota has an independent writer and cadence. Never copy its latest
		// state into this history snapshot: a slow import would overwrite it.
		if err := WriteJSON(m.DB, "codex.snapshot."+p.ID, snap); err != nil {
			issues = append(issues, err.Error())
		}
	}
	if len(issues) == 0 {
		db.MetaSet(m.DB, "sessions_generated_at", strconv.FormatInt(time.Now().Unix(), 10))
	}
	var n int
	if err := m.DB.QueryRow("SELECT count(*) FROM sessions").Scan(&n); err != nil {
		return n, err
	}
	if len(issues) > 0 {
		return n, fmt.Errorf("%s", strings.Join(issues, "; "))
	}
	return n, nil
}

func (m *Manager) outputRoot() string {
	if m.OutRoot != "" {
		return m.OutRoot
	}
	return filepath.Join(m.Dir, "dashboard", "data")
}

// refreshSpend replaces every session row the collection returned. A failed
// or partial collection keeps the previous usage totals rather than writing an
// empty set over them, because missing data cannot prove zero spend.
func (m *Manager) refreshSpend(ctx context.Context, p profiles.Profile, collect SpendCollector) error {
	key := SpendSnapshotKey(p.Provider, p.ID)
	var snap SpendSnapshot
	ReadJSON(m.DB, key, &snap)
	if snap.Directory != "" && snap.Directory != p.Directory {
		snap.Usage = nil
	}
	snap.Directory = p.Directory
	collectCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	res, err := collect(collectCtx, p.Directory, p.ID)
	cancel()
	snap.Warnings = res.Warnings
	if err != nil {
		snap.Error = err.Error()
		if werr := WriteJSON(m.DB, key, snap); werr != nil {
			return errors.Join(err, werr)
		}
		return err
	}
	for _, row := range res.Sessions {
		if err := db.UpsertSession(m.DB, row); err != nil {
			snap.Error = err.Error()
			WriteJSON(m.DB, key, snap)
			return err
		}
	}
	for _, source := range res.Sources {
		if err := db.PutSessionSource(m.DB, source); err != nil {
			return err
		}
	}
	snap.Error = ""
	snap.CollectedAt = time.Now().Unix()
	snap.Usage = payg.Merge(snap.Usage, res.Usage)
	return WriteJSON(m.DB, key, snap)
}
