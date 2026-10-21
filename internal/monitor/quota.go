package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

// QuotaFreshFor allows brief delays in one-minute quota polling. Failed polls
// still retain the original timestamp and are marked stale immediately. Keep
// this aligned with CODEX_FRESH_FOR_S in frontend/src/lib/allowance.ts.
const QuotaFreshFor = 5 * time.Minute

// QuotaSnapshot has a separate storage key from history so the two independent
// writers cannot lose each other's updates. A failed read retains the last
// successful Usage with its original FetchedAt and records the failed attempt.
type QuotaSnapshot struct {
	Directory         string      `json:"directory,omitempty"`
	Usage             codex.Usage `json:"usage"`
	UsageError        string      `json:"usage_error,omitempty"`
	AttemptedAt       int64       `json:"attempted_at"`
	NotificationError string      `json:"notification_error,omitempty"`
}

// ReadSnapshot combines independently persisted history and quota snapshots.
// Old combined snapshots remain readable until the first independent poll.
// A quota-only snapshot is valid even before history has been indexed.
func ReadSnapshot(d *sql.DB, profileID string) (Snapshot, error) {
	var history Snapshot
	historyErr := ReadJSON(d, "codex.snapshot."+profileID, &history)
	if historyErr != nil && !errors.Is(historyErr, os.ErrNotExist) {
		return Snapshot{}, historyErr
	}
	var quota QuotaSnapshot
	quotaErr := ReadJSON(d, "codex.quota."+profileID, &quota)
	if quotaErr != nil && !errors.Is(quotaErr, os.ErrNotExist) {
		return Snapshot{}, quotaErr
	}
	if historyErr != nil && quotaErr != nil {
		return Snapshot{}, os.ErrNotExist
	}
	if quotaErr == nil {
		history.Usage = quota.Usage
		history.UsageError = quota.UsageError
		history.UsageAttemptedAt = quota.AttemptedAt
		history.UsageDirectory = quota.Directory
		history.NotificationError = quota.NotificationError
	}
	return history, nil
}

func (m *Manager) currentTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *Manager) runQuota(ctx context.Context) {
	interval := m.quotaInterval
	if interval <= 0 {
		interval = time.Minute
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		// Per-profile failures are persisted and shown by the dashboard; a
		// failed poll must not stop subsequent scheduled attempts.
		_ = m.RefreshUsage(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

type quotaReading struct {
	profile profiles.Profile
	snap    QuotaSnapshot
	raw     string
	err     error
}

// Settings invalidates quota when a profile moves or is re-enabled. Match the
// value captured before the network request so that such an invalidation wins
// without waiting for the poller's process or network timeouts.
func (m *Manager) persistQuota(profile string, snap QuotaSnapshot, expected string) (string, bool, error) {
	data, err := json.Marshal(snap)
	if err != nil {
		return "", false, err
	}
	result, err := m.DB.Exec(`INSERT INTO meta(key,value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value WHERE meta.value=?`, "codex.quota."+profile, string(data), expected)
	if err != nil {
		return "", false, err
	}
	n, err := result.RowsAffected()
	return string(data), n > 0, err
}

// RefreshUsage performs only account quota reads. It never waits for history
// collection. A filesystem lease also serializes explicit callers in another
// process, including alert-state updates; concurrent passes simply skip.
func (m *Manager) RefreshUsage(ctx context.Context) error {
	if m.DB == nil {
		return errors.New("database unavailable")
	}
	if m.ReadUsage == nil {
		return errors.New("Codex quota reader unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(m.Dir, "state", "quota.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil
		}
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ps, err := m.Store.List()
	if err != nil {
		return err
	}
	var readings []quotaReading
	for _, p := range ps {
		if p.Enabled && p.Provider == profiles.Codex {
			readings = append(readings, quotaReading{profile: p})
		}
	}
	// Bound process count, while one slow or signed-out profile does not hold
	// up every other account's quota. Workers finish before the pass returns.
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(len(readings), 4) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range jobs {
				p := readings[i].profile
				var previous QuotaSnapshot
				raw, found, err := db.MetaGet(m.DB, "codex.quota."+p.ID)
				if err != nil {
					readings[i].err = err
					continue
				}
				readings[i].raw = raw
				if found {
					_ = json.Unmarshal([]byte(raw), &previous)
				} else {
					// Retain legacy successful quota on the first failed poll.
					if old, err := ReadSnapshot(m.DB, p.ID); err == nil {
						previous.Usage = old.Usage
					}
				}
				if previous.Directory != "" && previous.Directory != p.Directory {
					previous = QuotaSnapshot{}
				}
				previous.Directory = p.Directory
				readCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				usage, err := m.ReadUsage(readCtx, codex.Options{Home: p.Directory, ProfileID: p.ID, Binary: m.CodexBinary})
				cancel()
				previous.AttemptedAt = m.currentTime().Unix()
				if err != nil {
					previous.UsageError = err.Error()
				} else if usage.FetchedAt.IsZero() {
					previous.UsageError = "Codex quota response has no observation time"
				} else if usage.FetchedAt.Before(previous.Usage.FetchedAt) {
					previous.UsageError = "Codex quota response predates the last successful observation"
				} else {
					previous.Usage = usage
					previous.UsageError = ""
				}
				readings[i].snap = previous
			}
		}()
	}
	for i := range readings {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	// A settings edit can race a network read. Never publish its result after
	// the profile was disconnected, disabled, or pointed at a different home.
	current, err := m.Store.List()
	if err != nil {
		return err
	}
	active := map[string]profiles.Profile{}
	for _, p := range current {
		if p.Enabled && p.Provider == profiles.Codex {
			active[p.ID] = p
		}
	}
	var valid []quotaReading
	var issues []error
	for _, reading := range readings {
		if reading.err != nil {
			issues = append(issues, reading.err)
			continue
		}
		p, ok := active[reading.profile.ID]
		if !ok || p.Directory != reading.profile.Directory {
			continue
		}
		reading.profile = p
		raw, saved, err := m.persistQuota(p.ID, reading.snap, reading.raw)
		if err != nil {
			issues = append(issues, err)
			continue
		}
		if !saved {
			continue
		}
		reading.raw = raw
		if err := m.recordQuotaHistory(reading); err != nil {
			issues = append(issues, fmt.Errorf("quota history: %w", err))
		}
		valid = append(valid, reading)
		if reading.snap.UsageError != "" {
			issues = append(issues, fmt.Errorf("%s: %s", p.Name, reading.snap.UsageError))
		}
	}
	if err := m.alertQuota(ctx, valid); err != nil {
		issues = append(issues, err)
	}
	return errors.Join(issues...)
}
