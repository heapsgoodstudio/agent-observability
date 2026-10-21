package monitor

import (
	"fmt"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
)

func (m *Manager) recordQuotaHistory(reading quotaReading) error {
	s := reading.snap
	// A retained reading after a failed attempt is not a new observation.
	if s.UsageError != "" || s.Usage.FetchedAt.IsZero() {
		return nil
	}
	key := db.AccountKey("codex", s.Usage.AccountID, reading.profile.ID)
	var rows []db.AllowanceObservation
	for _, w := range s.Usage.Windows {
		reset := int64(0)
		if w.ResetsAtKnown {
			reset = w.ResetsAt
		}
		label := w.Name
		if w.WindowMinutesKnown {
			label = fmt.Sprintf("%s %d-minute allowance", label, w.WindowMinutes)
		}
		rows = append(rows, db.AllowanceObservation{WindowID: db.AllowanceWindowID(w.LimitID, w.Kind), Label: label, Source: "codex", ResetAt: reset, ResetKnown: w.ResetsAtKnown, UsedPercent: w.UsedPercent})
	}
	return db.PutAllowanceObservations(m.DB, "codex", reading.profile.ID, key, s.Usage.FetchedAt.Unix(), m.currentTime().Unix(), rows)
}
