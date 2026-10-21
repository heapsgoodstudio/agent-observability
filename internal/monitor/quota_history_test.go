package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/codex"
	"github.com/heapsgoodstudio/agent-observability/internal/db"
	"github.com/heapsgoodstudio/agent-observability/internal/profiles"
)

func TestQuotaHistoryIsProspectiveAndNeverRefreshesFailure(t *testing.T) {
	m, p := fixture(t)
	now := time.Unix(1800000000, 0)
	m.now = func() time.Time { return now }
	fetched := now.Add(-time.Minute)
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) {
		return quota(fetched, "shared", 0, now.Add(time.Hour).Unix()), nil
	}
	second, err := m.Store.Save(profiles.Profile{Provider: profiles.Codex, Name: "Second", Directory: t.TempDir(), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := m.RefreshUsage(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	read := func() []db.AllowanceObservation {
		rows, _, err := db.ListAllowanceObservations(m.DB, "codex", "codex:shared", now.Add(-time.Hour).Unix(), now.Unix())
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	if rows := read(); len(rows) != 1 || rows[0].UsedPercent != 0 || rows[0].ObservedAt != fetched.Unix() {
		t.Fatalf("shared/repeated read duplicated or lost zero: %+v", rows)
	}
	now = now.Add(time.Minute)
	m.ReadUsage = func(context.Context, codex.Options) (codex.Usage, error) { return codex.Usage{}, errors.New("offline") }
	if err := m.RefreshUsage(context.Background()); err == nil {
		t.Fatal("failed refresh did not report error")
	}
	if rows := read(); len(rows) != 1 || rows[0].ObservedAt != fetched.Unix() {
		t.Fatalf("failed poll stamped old value fresh: %+v", rows)
	}
	// Changing account produces new provenance, never migrates historical rows.
	m.ReadUsage = func(_ context.Context, o codex.Options) (codex.Usage, error) {
		account := "shared"
		if o.ProfileID == p.ID {
			account = "changed"
		}
		return quota(now, account, 25, now.Add(time.Hour).Unix()), nil
	}
	if err := m.RefreshUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	var oldCount, newCount int
	if err := m.DB.QueryRow(`SELECT count(*) FROM profile_account_observations WHERE profile=? AND account_key='codex:shared'`, p.ID).Scan(&oldCount); err != nil {
		t.Fatal(err)
	}
	if err := m.DB.QueryRow(`SELECT count(*) FROM profile_account_observations WHERE profile=? AND account_key='codex:changed'`, p.ID).Scan(&newCount); err != nil {
		t.Fatal(err)
	}
	if oldCount != 1 || newCount != 1 {
		t.Fatalf("identity history rewritten: old=%d new=%d (other %s)", oldCount, newCount, second.ID)
	}
}
