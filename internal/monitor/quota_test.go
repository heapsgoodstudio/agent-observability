package monitor

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

func quota(now time.Time, account string, pct float64, reset int64) codex.Usage {
	return codex.Usage{FetchedAt: now, AccountID: account, Windows: []codex.Window{{
		LimitID: "codex", Name: "Codex", Kind: "primary", UsedPercent: pct,
		WindowMinutes: 300, WindowMinutesKnown: true, ResetsAt: reset, ResetsAtKnown: reset != 0,
	}}}
}

func TestQuotaContinuesWhileHistoryIsBlockedAndStopsOnCancellation(t *testing.T) {
	m, _ := fixture(t)
	m.quotaInterval = 10 * time.Millisecond
	entered := make(chan struct{})
	m.Collect = func(ctx context.Context, _ codex.Options) (codex.Result, error) {
		close(entered)
		<-ctx.Done()
		return codex.Result{}, ctx.Err()
	}
	var polls atomic.Int64
	m.ReadUsage = func(ctx context.Context, _ codex.Options) (codex.Usage, error) {
		polls.Add(1)
		return quota(time.Now(), "", 12, time.Now().Add(time.Hour).Unix()), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(cancel)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("history did not start")
	}
	await(t, func() bool { return polls.Load() >= 3 })
	fallback := New(m.Dir, m.DB)
	var duplicates atomic.Int64
	fallback.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		duplicates.Add(1)
		return codex.Usage{}, nil
	}
	fallback.Collect = func(context.Context, codex.Options) (codex.Result, error) {
		duplicates.Add(1)
		return codex.Result{}, nil
	}
	if _, err := fallback.RefreshScheduled(ctx); err != nil || duplicates.Load() != 0 {
		t.Fatalf("CLI repeated daemon work: %d %v", duplicates.Load(), err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor did not join its quota/history workers")
	}
	after := polls.Load()
	time.Sleep(30 * time.Millisecond)
	if polls.Load() != after || m.Status().State == "running" {
		t.Fatal("work continued after shutdown")
	}
	if _, err := m.Start("", false); err == nil {
		t.Fatal("shut down monitor accepted more work")
	}
}

func TestIndependentSnapshotsNeverOverwriteNewQuota(t *testing.T) {
	m, p := fixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	m.Collect = func(context.Context, codex.Options) (codex.Result, error) {
		close(started)
		<-release
		return codex.Result{Complete: true}, nil
	}
	historyDone := make(chan error, 1)
	go func() { _, err := m.Refresh(context.Background(), "", false); historyDone <- err }()
	<-started
	now := time.Now()
	allowed := false
	balance := "19.25"
	u := quota(now, "acct", 99, now.Add(time.Hour).Unix())
	u.OrdinaryUsageAllowed = &allowed
	u.Buckets = []codex.Bucket{{LimitID: "codex", Credits: &codex.Credits{HasCredits: true, Balance: &balance}}}
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) { return u, nil }
	if err := m.RefreshUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := ReadSnapshot(m.DB, p.ID)
	if err != nil || before.Usage.OrdinaryUsageAllowed == nil || *before.Usage.OrdinaryUsageAllowed || before.CollectedAt != 0 {
		t.Fatalf("quota-only snapshot unavailable: %+v %v", before, err)
	}
	close(release)
	if err := <-historyDone; err != nil {
		t.Fatal(err)
	}
	after, err := ReadSnapshot(m.DB, p.ID)
	if err != nil || !after.Usage.FetchedAt.Equal(now) || after.Usage.Windows[0].UsedPercent != 99 || after.CollectedAt == 0 || *after.Usage.Buckets[0].Credits.Balance != balance || after.UsageDirectory != p.Directory {
		t.Fatalf("history overwrote quota: %+v %v", after, err)
	}
}

func TestQuotaFailureRetainsLegacySnapshotAndNeverAlertsFromStaleData(t *testing.T) {
	m, p := fixture(t)
	now := time.Now()
	u := quota(now.Add(-time.Minute), "account", 100, now.Add(time.Hour).Unix())
	if err := WriteJSON(m.DB, "codex.snapshot."+p.ID, Snapshot{Usage: u}); err != nil {
		t.Fatal(err)
	}
	var alerts int
	m.SendNotification = func(context.Context, Notification) error { alerts++; return nil }
	if err := m.RefreshUsage(context.Background()); err == nil {
		t.Fatal("missing account should be reported")
	}
	snap, err := ReadSnapshot(m.DB, p.ID)
	if err != nil || snap.UsageError == "" || !snap.Usage.FetchedAt.Equal(u.FetchedAt) || alerts != 0 || snap.UsageAttemptedAt == 0 {
		t.Fatalf("failed read became healthy or alerted: %+v alerts=%d err=%v", snap, alerts, err)
	}
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		return quota(now.Add(-3*time.Minute), "account", 100, now.Add(time.Hour).Unix()), nil
	}
	if err := m.RefreshUsage(context.Background()); err == nil {
		t.Fatal("older response replaced fresher persisted quota")
	}
	if alerts != 0 {
		t.Fatal("older quota fired notification")
	}
}

func TestDisabledProfileDuringPollDiscardsResponse(t *testing.T) {
	m, p := fixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		close(started)
		<-release
		return quota(time.Now(), "account", 100, time.Now().Add(time.Hour).Unix()), nil
	}
	var alerts int
	m.SendNotification = func(context.Context, Notification) error { alerts++; return nil }
	done := make(chan error, 1)
	go func() { done <- m.RefreshUsage(context.Background()) }()
	<-started
	p.Enabled = false
	if _, err := m.Store.Save(p); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(m.DB, p.ID); err == nil || alerts != 0 {
		t.Fatal("disabled profile's pending response was published")
	}
}

func TestQuotaPassLeaseSkipsConcurrentReader(t *testing.T) {
	m, _ := fixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		close(started)
		<-release
		return quota(time.Now(), "", 10, 0), nil
	}
	done := make(chan error, 1)
	go func() { done <- m.RefreshUsage(context.Background()) }()
	<-started
	other := New(m.Dir, m.DB)
	other.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		t.Error("concurrent quota read")
		return codex.Usage{}, nil
	}
	if err := other.RefreshUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestQuotaAlertsThresholdsResetJitterRestartAndDeliveryRetry(t *testing.T) {
	m, profile := fixture(t)
	now := time.Now()
	m.now = func() time.Time { return now }
	reset := now.Add(time.Hour).Unix()
	u := quota(now, "account", 74, reset)
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) { return u, nil }
	var sent []Notification
	fail := false
	m.SendNotification = func(_ context.Context, n Notification) error {
		if fail {
			return errors.New("not authorized")
		}
		sent = append(sent, n)
		return nil
	}
	poll := func(pct float64) {
		t.Helper()
		u.Windows[0].UsedPercent = pct
		if err := m.RefreshUsage(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	poll(74)
	for _, pct := range []float64{75, 90, 95, 100} {
		poll(pct)
		poll(pct)
	}
	if len(sent) != 4 {
		t.Fatalf("threshold delivery count=%d: %+v", len(sent), sent)
	}
	for _, n := range sent {
		if strings.Contains(n.Title+n.Body, "$") || strings.Contains(n.Title+n.Body, "billing") || !strings.Contains(n.URL, "provider=codex") {
			t.Fatalf("allowance warning made monetary claim or lost provider: %+v", n)
		}
		if !strings.Contains(n.Title, profile.Name) {
			t.Fatalf("account/profile missing from notification title: %+v", n)
		}
		link, err := url.Parse(n.URL)
		if err != nil || link.Query().Get("accountKey") != db.AccountKey("codex", "account", "") || link.Query().Has("profile") {
			t.Fatalf("known account notification did not link directly to its account: %+v", n)
		}
	}
	// A restart and small reset-time wobble must not reissue every warning.
	restarted := New(m.Dir, m.DB)
	restarted.now, restarted.ReadUsage, restarted.SendNotification = m.now, m.ReadUsage, m.SendNotification
	m = restarted
	u.Windows[0].ResetsAt += 1
	poll(100)
	if len(sent) != 4 {
		t.Fatal("restart/jitter repeated alert")
	}
	u.Windows[0].ResetsAt = reset + 5*3600
	poll(96)
	if len(sent) != 5 || !strings.Contains(sent[4].Body, "95%") {
		t.Fatal("new period did not emit only highest threshold")
	}
	fail = true
	u.Windows[0].UsedPercent = 100
	if err := m.RefreshUsage(context.Background()); err == nil {
		t.Fatal("failed notification not visible")
	}
	fail = false
	poll(100)
	if len(sent) != 6 {
		t.Fatal("failed delivery incorrectly consumed marker")
	}
}

func TestQuotaAlertsPickFreshestSharedAccountAndSeparateUnknownAccounts(t *testing.T) {
	for _, knownAccount := range []bool{true, false} {
		t.Run(map[bool]string{true: "shared account", false: "unknown accounts"}[knownAccount], func(t *testing.T) {
			m, first := fixture(t)
			second, err := m.Store.Save(profiles.Profile{Provider: profiles.Codex, Name: "Second", Directory: t.TempDir(), Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			account := ""
			if knownAccount {
				account = "shared"
			}
			m.ReadUsage = func(_ context.Context, opts codex.Options) (codex.Usage, error) {
				if opts.ProfileID == first.ID {
					return quota(now.Add(-time.Second), account, 95, now.Add(time.Hour).Unix()), nil
				}
				return quota(now, account, 90, now.Add(time.Hour).Unix()), nil
			}
			var alerts []Notification
			m.SendNotification = func(_ context.Context, n Notification) error { alerts = append(alerts, n); return nil }
			if err := m.RefreshUsage(context.Background()); err != nil {
				t.Fatal(err)
			}
			if knownAccount {
				if len(alerts) != 1 || !strings.Contains(alerts[0].Title, second.Name) || !strings.Contains(alerts[0].Body, second.Name) || !strings.Contains(alerts[0].Body, "90%") {
					t.Fatalf("wrong shared allowance: %+v", alerts)
				}
			} else if len(alerts) != 2 {
				t.Fatalf("unknown identities collapsed: %+v", alerts)
			} else {
				want := map[string]string{first.ID: first.Name, second.ID: second.Name}
				for _, alert := range alerts {
					link, err := url.Parse(alert.URL)
					if err != nil {
						t.Fatal(err)
					}
					name, ok := want[link.Query().Get("profile")]
					if !ok || link.Query().Get("provider") != "codex" || link.Query().Has("accountKey") || !strings.Contains(alert.Title, name) {
						t.Fatalf("unknown account notification lost its exact profile/name: %+v", alert)
					}
					delete(want, link.Query().Get("profile"))
				}
			}
		})
	}
}

func TestQuotaNotificationAccountLinkEscapesProviderIdentity(t *testing.T) {
	m, profile := fixture(t)
	profile.Name = "Work & research"
	if _, err := m.Store.Save(profile); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	account := "workspace/team + id"
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		return quota(now, account, 90, now.Add(time.Hour).Unix()), nil
	}
	var sent []Notification
	m.SendNotification = func(_ context.Context, n Notification) error { sent = append(sent, n); return nil }
	if err := m.RefreshUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || !strings.Contains(sent[0].Title, profile.Name) {
		t.Fatalf("missing notification account label: %+v", sent)
	}
	link, err := url.Parse(sent[0].URL)
	if err != nil || link.Query().Get("accountKey") != db.AccountKey("codex", account, "") || link.Query().Get("provider") != "codex" || link.Query().Has("profile") {
		t.Fatalf("account identity was corrupted in notification link: %+v", sent[0])
	}
}

func TestQuotaStaleAndExpiredResetDoNotRearmAllowance(t *testing.T) {
	for _, tc := range []struct {
		name        string
		age         time.Duration
		resetOffset time.Duration
	}{
		{"stale success", -(QuotaFreshFor + time.Minute), time.Hour},
		{"expired reset", 0, -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := fixture(t)
			now := time.Now()
			m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
				return quota(now.Add(tc.age), "", 100, now.Add(tc.resetOffset).Unix()), nil
			}
			var alerts int
			m.SendNotification = func(context.Context, Notification) error { alerts++; return nil }
			if err := m.RefreshUsage(context.Background()); err != nil {
				t.Fatal(err)
			}
			if alerts != 0 {
				t.Fatal("stale reading triggered allowance notification")
			}
		})
	}
}

func TestQuotaMissingResetDoesNotInventANewPeriod(t *testing.T) {
	m, _ := fixture(t)
	now := time.Now()
	u := quota(now, "account", 90, now.Add(time.Hour).Unix())
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) { return u, nil }
	var alerts int
	m.SendNotification = func(context.Context, Notification) error { alerts++; return nil }
	poll := func() {
		t.Helper()
		if err := m.RefreshUsage(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	u.Windows[0].ResetsAtKnown = false
	u.Windows[0].ResetsAt = 0
	poll()
	poll()
	if alerts != 1 {
		t.Fatal("missing reset rearmed an already delivered crossing")
	}
	u.Windows[0].UsedPercent = 40
	poll()
	u.Windows[0].UsedPercent = 90
	poll()
	if alerts != 2 {
		t.Fatal("observed lower usage did not rearm an unknown-reset window")
	}
}

func TestCreditOnlyQuotaPersistsWithoutInventedWindowOrCharge(t *testing.T) {
	m, p := fixture(t)
	zero := "0"
	u := codex.Usage{FetchedAt: time.Now(), Buckets: []codex.Bucket{{LimitID: "credits", Credits: &codex.Credits{Balance: &zero}}}}
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) { return u, nil }
	var alerts int
	m.SendNotification = func(context.Context, Notification) error { alerts++; return nil }
	if err := m.RefreshUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap, err := ReadSnapshot(m.DB, p.ID)
	if err != nil || snap.UsageError != "" || snap.Usage.OrdinaryUsageAllowed != nil || len(snap.Usage.Windows) != 0 || len(snap.Usage.Buckets) != 1 || alerts != 0 {
		t.Fatalf("credit-only response invented allowance/charge: %+v %v", snap, err)
	}
}

func TestRunWaitsForScheduledLeaseThenStartsQuota(t *testing.T) {
	m, _ := fixture(t)
	lease, err := m.daemonLease()
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			releaseLease(lease)
		}
	})
	var polls atomic.Int64
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		polls.Add(1)
		return quota(time.Now(), "", 0, 0), nil
	}
	m.Collect = func(context.Context, codex.Options) (codex.Result, error) { return codex.Result{}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	select {
	case <-done:
		t.Fatal("server abandoned scheduling while CLI held lease")
	case <-time.After(30 * time.Millisecond):
	}
	if polls.Load() != 0 {
		t.Fatal("quota read bypassed daemon lease")
	}
	releaseLease(lease)
	released = true
	await(t, func() bool { return polls.Load() > 0 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("monitor did not stop")
	}
}

func TestMovingUnknownAccountProfileRearmsItsAllowance(t *testing.T) {
	m, p := fixture(t)
	now := time.Now()
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		return quota(now, "", 90, now.Add(time.Hour).Unix()), nil
	}
	var alerts []Notification
	m.SendNotification = func(_ context.Context, n Notification) error { alerts = append(alerts, n); return nil }
	if err := m.RefreshUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.Directory = t.TempDir()
	if _, err := m.Store.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := m.RefreshUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 || !strings.Contains(alerts[1].Body, "5-hour") {
		t.Fatalf("new unknown account inherited old warning marker or technical duration: %+v", alerts)
	}
}

func TestSettingsInvalidationWinsOverInFlightQuota(t *testing.T) {
	m, p := fixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		close(started)
		<-release
		return quota(time.Now(), "old-account", 100, time.Now().Add(time.Hour).Unix()), nil
	}
	var alerts int
	m.SendNotification = func(context.Context, Notification) error { alerts++; return nil }
	done := make(chan error, 1)
	go func() { done <- m.RefreshUsage(context.Background()) }()
	<-started
	// A settings mutation can invalidate an observation even when the same
	// profile/home is re-enabled before the network request finishes.
	invalid := QuotaSnapshot{Directory: p.Directory, UsageError: "Waiting for a fresh account read"}
	if err := WriteJSON(m.DB, "codex.quota."+p.ID, invalid); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	snap, err := ReadSnapshot(m.DB, p.ID)
	if err != nil || snap.UsageError != invalid.UsageError || !snap.Usage.FetchedAt.IsZero() || alerts != 0 {
		t.Fatalf("old account response replaced Settings invalidation: %+v %v", snap, err)
	}
}

func TestNewestWholeAccountSnapshotDropsMissingWindows(t *testing.T) {
	for _, secondary := range []bool{false, true} {
		t.Run(map[bool]string{false: "credit only", true: "secondary only"}[secondary], func(t *testing.T) {
			m, first := fixture(t)
			if _, err := m.Store.Save(profiles.Profile{Provider: profiles.Codex, Name: "Newer", Directory: t.TempDir(), Enabled: true}); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			m.ReadUsage = func(_ context.Context, opts codex.Options) (codex.Usage, error) {
				if opts.ProfileID == first.ID {
					return quota(now.Add(-time.Second), "same-account", 100, now.Add(time.Hour).Unix()), nil
				}
				newest := codex.Usage{AccountID: "same-account", FetchedAt: now, Buckets: []codex.Bucket{{LimitID: "codex", Credits: &codex.Credits{HasCredits: true}}}}
				if secondary {
					newest.Windows = []codex.Window{{LimitID: "codex", Kind: "secondary", UsedPercent: 20}}
				}
				return newest, nil
			}
			var alerts int
			m.SendNotification = func(context.Context, Notification) error { alerts++; return nil }
			if err := m.RefreshUsage(context.Background()); err != nil {
				t.Fatal(err)
			}
			if alerts != 0 {
				t.Fatal("older account response retained a missing window")
			}
		})
	}
}
