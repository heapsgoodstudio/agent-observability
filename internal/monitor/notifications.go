package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/config"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
)

type Notification struct {
	Title, Body, URL string
}

// NotificationSender is injectable so fixtures never display real banners.
type NotificationSender func(context.Context, Notification) error

type alertMarker struct {
	Reset      int64 `json:"reset"`
	NotifiedAt int64 `json:"notified_at"`
}

type quotaAlert struct {
	reading quotaReading
	window  codex.Window
}

func quotaIdentity(r quotaReading) string {
	directory := r.profile.Directory
	if canonical, err := filepath.EvalSymlinks(directory); err == nil {
		directory = canonical
	}
	// A profile redirected to another home may now represent another account.
	// Without an explicit account ID, its former dedup markers cannot prove
	// that a warning for the newly connected allowance was already delivered.
	owner := fmt.Sprintf("profile:%s:%x", r.profile.ID, sha256.Sum256([]byte(directory)))
	if r.snap.Usage.AccountID != "" {
		owner = "account:" + r.snap.Usage.AccountID
	}
	return owner
}

func windowIdentity(r quotaReading, w codex.Window) string {
	key, _ := json.Marshal([]string{"codex", quotaIdentity(r), w.LimitID, w.Kind})
	return string(key)
}

func windowDuration(minutes int64) string {
	switch {
	case minutes == 10080:
		return "weekly"
	case minutes == 1440:
		return "daily"
	case minutes%10080 == 0:
		return fmt.Sprintf("%d-week", minutes/10080)
	case minutes%1440 == 0:
		return fmt.Sprintf("%d-day", minutes/1440)
	case minutes%60 == 0:
		return fmt.Sprintf("%d-hour", minutes/60)
	default:
		return fmt.Sprintf("%d-minute", minutes)
	}
}

func sameReset(a, b int64) bool {
	// Missing metadata cannot establish that the allowance period changed.
	if a == 0 || b == 0 || a == b {
		return true
	}
	return a > 0 && b > 0 && math.Abs(float64(a)-float64(b)) <= 120
}

func (m *Manager) alertQuota(ctx context.Context, readings []quotaReading) error {
	now := m.currentTime()
	// Choose a whole account response before its windows. A newer credit-only
	// response or a removed bucket must not retain an older window's warning.
	// Unknown accounts stay separate rather than inventing shared allowance.
	accounts := map[string]quotaReading{}
	freshest := map[string]quotaAlert{}
	currentRaw := map[string]string{}
	for _, r := range readings {
		currentRaw[r.profile.ID] = r.raw
		key := quotaIdentity(r)
		old, ok := accounts[key]
		if !ok || r.snap.Usage.FetchedAt.After(old.snap.Usage.FetchedAt) ||
			(r.snap.Usage.FetchedAt.Equal(old.snap.Usage.FetchedAt) && old.snap.UsageError != "" && r.snap.UsageError == "") {
			accounts[key] = r
		}
	}
	for _, r := range accounts {
		for _, w := range r.snap.Usage.Windows {
			key := windowIdentity(r, w)
			freshest[key] = quotaAlert{r, w}
		}
	}
	state := map[string]alertMarker{}
	if err := ReadJSON(m.DB, "codex.alerts", &state); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read Codex notification state: %w", err)
	}
	if state == nil {
		state = map[string]alertMarker{}
	}
	keys := make([]string, 0, len(freshest))
	for key := range freshest {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var issues []error
	failedProfiles := map[string]bool{}
	for _, key := range keys {
		candidate := freshest[key]
		r, w := candidate.reading, candidate.window
		age := now.Sub(r.snap.Usage.FetchedAt)
		if r.snap.UsageError != "" || r.snap.Usage.FetchedAt.IsZero() || age > QuotaFreshFor || age < -5*time.Second || math.IsNaN(w.UsedPercent) || math.IsInf(w.UsedPercent, 0) || w.UsedPercent < 0 {
			continue
		}
		reset := int64(0)
		if w.ResetsAtKnown {
			reset = w.ResetsAt
			if reset <= now.Unix() {
				// Crossing the clock boundary does not establish a new allowance.
				// Wait for another account read before alerting about this window.
				continue
			}
		}
		threshold := 0
		var crossed []string
		for _, pct := range []int{75, 90, 95, 100} {
			stateKey := key + ":" + strconv.Itoa(pct)
			old, notified := state[stateKey]
			if w.UsedPercent < float64(pct) {
				// Without a reset timestamp, only an observed drop below a
				// threshold can prove it should be armed for a later crossing.
				if (reset == 0 || old.Reset == 0) && notified {
					delete(state, stateKey)
				}
				continue
			}
			if !notified || !sameReset(old.Reset, reset) {
				crossed = append(crossed, stateKey)
				threshold = pct
			}
		}
		if threshold == 0 {
			continue
		}
		if raw, found, err := db.MetaGet(m.DB, "codex.quota."+r.profile.ID); err != nil {
			issues = append(issues, err)
			continue
		} else if !found || raw != currentRaw[r.profile.ID] {
			// Settings invalidated this observation after it was persisted.
			continue
		}
		remaining := math.Max(0, 100-w.UsedPercent)
		owner := strings.TrimSpace(r.profile.Name)
		if owner == "" {
			owner = r.snap.Usage.AccountID
		}
		if owner == "" {
			owner = r.profile.ID
		}
		title := fmt.Sprintf("Codex · %s: %.0f%% remaining", owner, math.Floor(remaining))
		label := strings.TrimSpace(w.Name)
		if label == "" {
			label = w.LimitID
		}
		if label == "" {
			label = "Included usage"
		}
		if w.WindowMinutesKnown && w.WindowMinutes > 0 {
			label += " " + windowDuration(w.WindowMinutes)
		} else if w.Kind != "" {
			label += " " + w.Kind
		}
		body := fmt.Sprintf("%s: %s crossed %d%% used.", owner, label, threshold)
		if reset > 0 {
			body += " Resets " + time.Unix(reset, 0).Local().Format("Mon 15:04 MST") + "."
		} else {
			body += " Reset time is unavailable."
		}
		if threshold == 100 {
			title = fmt.Sprintf("Codex · %s: included window exhausted", owner)
			body += " Check paid-use status on the dashboard before continuing."
		}
		n := Notification{Title: title, Body: body, URL: m.dashboardURL(r.profile.ID, r.snap.Usage.AccountID)}
		sender := m.SendNotification
		if sender == nil {
			sender = m.sendNotification
		}
		notifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := sender(notifyCtx, n)
		cancel()
		if err != nil {
			failedProfiles[r.profile.ID] = true
			issues = append(issues, fmt.Errorf("%s: Codex allowance notification failed: %w", r.profile.Name, err))
			r.snap.NotificationError = "Codex allowance notification could not be delivered"
			if raw, saved, writeErr := m.persistQuota(r.profile.ID, r.snap, currentRaw[r.profile.ID]); writeErr != nil {
				issues = append(issues, writeErr)
			} else if saved {
				currentRaw[r.profile.ID] = raw
			}
			continue
		}
		// A first observation may cross several thresholds. Deliver the most
		// urgent banner once, and record the lower crossings at the same time.
		for _, stateKey := range crossed {
			state[stateKey] = alertMarker{Reset: reset, NotifiedAt: now.Unix()}
		}
		if err := WriteJSON(m.DB, "codex.alerts", state); err != nil {
			return fmt.Errorf("persist Codex notification state: %w", err)
		}
		if r.snap.NotificationError != "" && !failedProfiles[r.profile.ID] {
			r.snap.NotificationError = ""
			if raw, saved, err := m.persistQuota(r.profile.ID, r.snap, currentRaw[r.profile.ID]); err != nil {
				issues = append(issues, err)
			} else if saved {
				currentRaw[r.profile.ID] = raw
			}
		}
	}
	// Keep disconnected identity markers long enough to avoid repeated alerts
	// after reconnects, without growing state indefinitely with retired IDs.
	for key, marker := range state {
		if marker.NotifiedAt < now.Add(-90*24*time.Hour).Unix() {
			delete(state, key)
		}
	}
	if err := WriteJSON(m.DB, "codex.alerts", state); err != nil {
		issues = append(issues, err)
	}
	return errors.Join(issues...)
}

func (m *Manager) dashboardURL(profile, account string) string {
	port := "8427"
	if cfg, err := config.Load(filepath.Join(m.Dir, "config.env")); err == nil {
		if value := cfg.Get("DASH_PORT", port); value != "" {
			if n, err := strconv.Atoi(value); err == nil && n > 0 && n <= 65535 {
				port = value
			}
		}
	}
	q := url.Values{"provider": {"codex"}}
	if account != "" {
		q.Set("accountKey", db.AccountKey("codex", account, ""))
	} else {
		q.Set("profile", profile)
	}
	return "http://localhost:" + port + "/?" + q.Encode()
}

// sendNotification uses the already installed notifier and its existing
// fallback. Profile names are arguments, never executable AppleScript text.
func (m *Manager) sendNotification(ctx context.Context, n Notification) error {
	home, _ := os.UserHomeDir()
	app := filepath.Join(home, "Applications", "Agent Observability.app")
	dryRun := os.Getenv("DRY_RUN") == "1"
	if cfg, err := config.Load(filepath.Join(m.Dir, "config.env")); err == nil {
		app = cfg.Get("NOTIFIER_APP", app)
		dryRun = dryRun || cfg.Get("DRY_RUN", "0") == "1"
	}
	if dryRun {
		fmt.Printf("ALERT [%s] %s -> %s\n", n.Title, n.Body, n.URL)
		return nil
	}
	if app != "" {
		bin := filepath.Join(app, "Contents", "MacOS", "AgentObsNotifier")
		if st, err := os.Stat(bin); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0 {
			cmd := exec.CommandContext(ctx, bin, n.Title, n.Body, n.URL)
			cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
			if err := cmd.Run(); err == nil {
				return nil
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv) sound name \"Sosumi\"", "-e", "end run", "--", n.Title, n.Body)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}
