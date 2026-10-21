package db

import (
	"testing"
	"time"
)

func TestAllowanceHistoryPreservesScopeDeduplicatesAndBoundsRetention(t *testing.T) {
	d := openMigrated(t)
	now := int64(1800000000)
	row := AllowanceObservation{WindowID: "codex:primary", Label: "Weekly", UsedPercent: 0, ResetAt: now + 500, Source: "codex"}
	for _, tc := range []struct {
		provider, profile, account string
		ts                         int64
	}{{"codex", "one", "codex:a", now - 60}, {"codex", "two", "codex:a", now - 60}, {"claude", "one", "claude:a", now - 60}, {"codex", "one", "codex:b", now - 60}, {"codex", "old", "codex:a", now - int64(AccountHistoryRetention/time.Second) - 1}} {
		if err := PutAllowanceObservations(d, tc.provider, tc.profile, tc.account, tc.ts, now, []AllowanceObservation{row}); err != nil {
			t.Fatal(err)
		}
	}
	rows, truncated, err := ListAllowanceObservations(d, "codex", "codex:a", 0, now)
	if err != nil || truncated || len(rows) != 1 || rows[0].UsedPercent != 0 {
		t.Fatalf("dedup/isolation/zero/retention failed: %+v %v %v", rows, truncated, err)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM profile_account_observations WHERE provider='codex' AND account_key='codex:a'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("profile provenance lost: %d %v", n, err)
	}
	if _, _, err := ListAllowanceObservations(d, "codex", "", 0, now); err == nil {
		t.Fatal("empty account widened query")
	}
	if err := PutAllowanceObservations(d, "codex", "one", "codex:a", now+61, now, []AllowanceObservation{row}); err == nil {
		t.Fatal("future sample accepted")
	}
}

func TestAccountKeysCannotCollideWithUnresolvedProfiles(t *testing.T) {
	if AccountKey("codex", "profile:test", "") == AccountKey("codex", "", "test") {
		t.Fatal("reported identity collides with unresolved profile")
	}
	if AllowanceWindowID("a:b", "c") == AllowanceWindowID("a", "b:c") {
		t.Fatal("window IDs collide")
	}
}

func TestAllowanceHistoryResetZeroHasPresence(t *testing.T) {
	d := openMigrated(t)
	now := int64(1800000000)
	rows := []AllowanceObservation{{WindowID: "known", ResetKnown: true, ResetAt: 0, UsedPercent: 0}, {WindowID: "unknown", UsedPercent: 0}}
	if err := PutAllowanceObservations(d, "codex", "p", "codex:a", now, now, rows); err != nil {
		t.Fatal(err)
	}
	got, _, err := ListAllowanceObservations(d, "codex", "codex:a", now, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.ResetKnown != (r.WindowID == "known") {
			t.Fatalf("lost reset presence: %+v", r)
		}
	}
}
