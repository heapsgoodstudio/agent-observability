package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/payg"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
	"github.com/heapsgoodstudio/agent-observability/internal/transcript"
)

func fixture(t *testing.T) (*Manager, profiles.Profile) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(filepath.Join(dir, "state", "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err = db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	m := New(dir, d)
	m.SendNotification = func(context.Context, Notification) error { return nil }
	p, err := m.Store.Save(profiles.Profile{Provider: profiles.Codex, Name: "Test", Directory: t.TempDir(), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		return codex.Usage{}, errors.New("not signed in")
	}
	return m, p
}

func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("background operation did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRefreshReplacesUsageAndRetainsLastKnownDataOnPartialRead(t *testing.T) {
	m, p := fixture(t)
	row := db.SessionRow{ID: codex.SessionID(p.ID, "thread"), NativeID: "thread", Provider: "codex", Profile: p.ID, UsageKnown: true, CostKnown: true, CostUSD: 2, Tokens: transcript.Tokens{Input: 10, Output: 5}, ReasoningTokens: 3, ErrorCount: 1, DurationMS: 100, ParentID: "parent"}
	m.Collect = func(context.Context, codex.Options) (codex.Result, error) {
		return codex.Result{Sessions: []db.SessionRow{row}, Complete: true}, nil
	}
	fetched := time.Now().Add(-time.Minute)
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		return codex.Usage{FetchedAt: fetched, AccountID: "account", Windows: []codex.Window{{Kind: "primary", UsedPercent: 42}}}, nil
	}
	for range 2 {
		if _, err := m.Refresh(context.Background(), "", false); err != nil {
			t.Fatal(err)
		}
		if err := m.RefreshUsage(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.ListSessions(m.DB, "")
	if err != nil || len(rows) != 1 || rows[0].CostUSD != 2 || rows[0].ReasoningTokens != 3 || rows[0].DurationMS != 100 {
		t.Fatalf("replacement/roundtrip failed: %+v %v", rows, err)
	}
	row.UsageKnown = false
	row.CostKnown = false
	row.Tokens = transcript.Tokens{}
	row.CostUSD = 0
	row.ReasoningTokens = 0
	m.Collect = func(context.Context, codex.Options) (codex.Result, error) {
		return codex.Result{Sessions: []db.SessionRow{row}}, errors.New("partial history")
	}
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		return codex.Usage{}, errors.New("temporarily unavailable")
	}
	if _, err := m.Refresh(context.Background(), "", false); err == nil {
		t.Fatal("partial collection should report its error")
	}
	if err := m.RefreshUsage(context.Background()); err == nil {
		t.Fatal("failed quota collection should report its error")
	}
	stored, _, _ := db.GetSession(m.DB, row.ID)
	if !stored.UsageKnown || stored.CostUSD != 2 || stored.Tokens.Input != 10 {
		t.Fatalf("lost known history: %+v", stored)
	}
	snap, err := ReadSnapshot(m.DB, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Usage.FetchedAt.Equal(fetched) || snap.Usage.Windows[0].UsedPercent != 42 || snap.UsageError == "" || snap.Error == "" {
		t.Fatalf("lost stale snapshot: %+v", snap)
	}
}

func TestUsageGapClearsAfterRecoveryAndCannotEraseKnownUsage(t *testing.T) {
	m, p := fixture(t)
	row := db.SessionRow{ID: codex.SessionID(p.ID, "gap"), Provider: "codex", Profile: p.ID}
	detail := codex.Detail{UsageUnavailableReason: "interrupted"}
	m.Collect = func(context.Context, codex.Options) (codex.Result, error) {
		return codex.Result{Sessions: []db.SessionRow{row}, Details: map[string]codex.Detail{row.ID: detail}}, nil
	}
	check := func(wantGap bool, wantInput int64) {
		t.Helper()
		if _, err := m.Refresh(context.Background(), "", false); err != nil {
			t.Fatal(err)
		}
		snap, err := ReadSnapshot(m.DB, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if reason, exists := snap.UsageGaps[row.ID]; exists != wantGap || (exists && reason != "interrupted") {
			t.Fatalf("gap should reflect persisted usage: %+v", snap.UsageGaps)
		}
		stored, _, err := db.GetSession(m.DB, row.ID)
		if err != nil || stored.Tokens.Input != wantInput {
			t.Fatalf("known usage changed: %+v, %v", stored, err)
		}
	}
	check(true, 0)
	row.UsageKnown, detail.RejectedOnly = true, true
	check(false, 0)
	// The next attempt in that session might be interrupted. Its earlier
	// rejection does not turn the new unknown usage into another zero.
	row.UsageKnown, detail.RejectedOnly = false, false
	check(true, 0)
	row.UsageKnown, row.Requests, row.Tokens.Input = true, 1, 123
	check(false, 123)
	row.UsageKnown, row.Requests, row.Tokens.Input = false, 0, 0
	check(false, 123)
	// A truncated log containing only a rejected request cannot replace an
	// already indexed response with a known zero either.
	row.UsageKnown, detail.RejectedOnly = true, true
	check(false, 123)
}

func TestQueuedImportSurvivesInterruptionAndSkipsRemovedProfile(t *testing.T) {
	m, p := fixture(t)
	if err := m.Enqueue("removed-profile"); err != nil {
		t.Fatal(err)
	}
	if err := m.Enqueue(p.ID); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	m.Collect = func(ctx context.Context, _ codex.Options) (codex.Result, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return codex.Result{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("removed profile blocked queue")
	}
	var pending []string
	if err := ReadJSON(m.DB, "monitor.pending", &pending); err != nil || len(pending) != 1 || pending[0] != p.ID {
		t.Fatalf("in-flight import not durable: %v %v", pending, err)
	}
	// The scheduled fallback must not duplicate work owned by the server.
	fallback := New(m.Dir, m.DB)
	var duplicate atomic.Bool
	fallback.Collect = func(context.Context, codex.Options) (codex.Result, error) {
		duplicate.Store(true)
		return codex.Result{}, nil
	}
	if _, err := fallback.RefreshScheduled(context.Background()); err != nil || duplicate.Load() {
		t.Fatal("scheduled collection duplicated server work", err)
	}
	cancel()
	<-done
	await(t, func() bool { return m.Status().State != "running" })
	restarted := New(m.Dir, m.DB)
	restarted.Collect = func(context.Context, codex.Options) (codex.Result, error) { return codex.Result{Complete: true}, nil }
	restarted.ReadUsage = m.ReadUsage
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { restarted.Run(ctx2); close(done2) }()
	await(t, func() bool {
		var q []string
		return ReadJSON(m.DB, "monitor.pending", &q) == nil && len(q) == 0 && restarted.Status().State == "complete"
	})
	cancel2()
	<-done2
}

func TestUnavailableDatabaseDoesNotPanic(t *testing.T) {
	m := New(t.TempDir(), nil)
	if err := m.Enqueue(""); err == nil {
		t.Fatal("expected unavailable database error")
	}
	m.Run(context.Background())
}

// Parser corrections must replace inflated history, even when the stored
// values are larger. Only genuinely unavailable usage retains old totals.
func TestRefreshCorrectsInflatedKnownUsage(t *testing.T) {
	m, p := fixture(t)
	row := db.SessionRow{ID: codex.SessionID(p.ID, "thread"), NativeID: "thread", Provider: "codex", Profile: p.ID,
		UsageKnown: true, CostKnown: true, CostUSD: 26045, Requests: 733,
		Tokens: transcript.Tokens{Input: 315981446, CacheRead: 8071713792, Output: 48672761}}
	if err := db.UpsertSession(m.DB, row); err != nil {
		t.Fatal(err)
	}
	row.CostUSD = 74.882136
	row.Requests = 405
	row.Tokens = transcript.Tokens{Input: 1812671, CacheRead: 43758976, Output: 259929}
	m.Collect = func(context.Context, codex.Options) (codex.Result, error) {
		return codex.Result{Sessions: []db.SessionRow{row}, Complete: true}, nil
	}
	for range 2 {
		if _, err := m.Refresh(context.Background(), p.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.ListSessions(m.DB, p.ID)
	if err != nil || len(rows) != 1 || rows[0].CostUSD != row.CostUSD || rows[0].Tokens != row.Tokens || rows[0].Requests != 405 {
		t.Fatalf("corrected totals did not replace inflated history: %+v, %v", rows, err)
	}
}

// Each pay-as-you-go harness reaches its own collector and snapshot key, the
// Oh My Pi key keeps the form the first release wrote, and a failed read keeps
// the usage already stored rather than writing an empty set over it.
func TestRefreshRoutesEachSpendHarnessToItsOwnCollectorAndSnapshot(t *testing.T) {
	m, _ := fixture(t)
	ompDir := filepath.Join(t.TempDir(), ".omp", "agent")
	ocDir := filepath.Join(t.TempDir(), ".local", "share", "opencode")
	for _, d := range []string{filepath.Join(ompDir, "sessions"), ocDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pOmp, err := m.Store.Save(profiles.Profile{Provider: profiles.Omp, Directory: ompDir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	pOc, err := m.Store.Save(profiles.Profile{Provider: profiles.Opencode, Directory: ocDir, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	bucket := func(provider string) []payg.Bucket {
		return []payg.Bucket{{Day: "2026-09-23", Provider: provider, Model: "glm-5.3", Requests: 1, CostUSD: 1, CostKnown: true}}
	}
	fail := false
	m.Spend = map[profiles.Provider]SpendCollector{
		profiles.Omp: func(_ context.Context, dir, id string) (SpendResult, error) {
			return SpendResult{Sessions: []db.SessionRow{{ID: "omp:" + id + ":a", Provider: "omp", Profile: id}}, Usage: bucket("baseten")}, nil
		},
		profiles.Opencode: func(_ context.Context, dir, id string) (SpendResult, error) {
			if fail {
				return SpendResult{}, errors.New("database unavailable")
			}
			return SpendResult{Sessions: []db.SessionRow{{ID: "opencode:" + id + ":b", Provider: "opencode", Profile: id}}, Usage: bucket("fireworks")}, nil
		},
	}
	if _, err := m.Refresh(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	if SpendSnapshotKey(profiles.Omp, pOmp.ID) != "omp.snapshot."+pOmp.ID {
		t.Fatal("the omp snapshot key must keep its first-release form")
	}
	read := func(p profiles.Profile) SpendSnapshot {
		var snap SpendSnapshot
		if err := ReadJSON(m.DB, SpendSnapshotKey(p.Provider, p.ID), &snap); err != nil {
			t.Fatal(err)
		}
		return snap
	}
	if s := read(pOmp); len(s.Usage) != 1 || s.Usage[0].Provider != "baseten" {
		t.Fatalf("omp snapshot = %+v", s)
	}
	if s := read(pOc); len(s.Usage) != 1 || s.Usage[0].Provider != "fireworks" || s.CollectedAt == 0 {
		t.Fatalf("opencode snapshot = %+v", s)
	}
	for _, id := range []string{"omp:" + pOmp.ID + ":a", "opencode:" + pOc.ID + ":b"} {
		if _, ok, err := db.GetSession(m.DB, id); err != nil || !ok {
			t.Fatalf("session %s not upserted (%v)", id, err)
		}
	}
	fail = true
	m.Refresh(context.Background(), "", false)
	if s := read(pOc); s.Error == "" || len(s.Usage) != 1 {
		t.Fatalf("after a failed read = %+v; the error is recorded and usage kept", s)
	}
}
