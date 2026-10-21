package plan

import (
	"context"
	"testing"

	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

// fakeQ answers each query from a table, so a test can describe a whole
// multi-account store without standing one up.
type fakeQ struct{ byQuery map[string][]vm.Series }

func (f fakeQ) Q(_ context.Context, query string) ([]vm.Series, error) {
	return f.byQuery[query], nil
}

func ser(v string, labels map[string]string) vm.Series {
	return vm.Series{Metric: labels, Value: []any{float64(1), v}}
}

// TestLoadAccountWindowsKeepsAccountsApart is the guard for the mistake the
// flat loader would make if it were reused here.
//
// The flat queries reduce with max by (window,source). Across two accounts
// that answers "which account is highest", so one account at 95% would be
// reported as every account's figure, and an account with headroom would look
// like it was about to throttle.
func TestLoadAccountWindowsKeepsAccountsApart(t *testing.T) {
	f := fakeQ{byQuery: map[string][]vm.Series{
		pctByAccountQuery: {
			ser("95", map[string]string{"window": "seven_day", "source": "cache", "account": "aaaa1111"}),
			ser("5", map[string]string{"window": "seven_day", "source": "cache", "account": "bbbb2222"}),
		},
	}}

	got, err := LoadAccountWindows(context.Background(), f)
	if err != nil {
		t.Fatalf("LoadAccountWindows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 accounts, got %d", len(got))
	}
	// Sorted by highest percentage, so the account closest to throttling is
	// first however the caller renders the list.
	if got[0].Account != "aaaa1111" || got[1].Account != "bbbb2222" {
		t.Fatalf("wrong order: %s then %s", got[0].Account, got[1].Account)
	}
	if p := MaxPct(got[0].Windows); p != 95 {
		t.Errorf("account aaaa1111 max pct = %v, want 95", p)
	}
	if p := MaxPct(got[1].Windows); p != 5 {
		t.Errorf("account bbbb2222 max pct = %v, want 5; a collapsed query would say 95", p)
	}
}

// TestLoadAccountWindowsPicksFreshestPerAccount: the freshness rule applies
// inside one account, using that account's own source ages. One account's
// statusline tap being cold says nothing about another's.
func TestLoadAccountWindowsPicksFreshestPerAccount(t *testing.T) {
	f := fakeQ{byQuery: map[string][]vm.Series{
		pctByAccountQuery: {
			ser("14", map[string]string{"window": "five_hour", "source": "statusline", "account": "aaaa1111"}),
			ser("0", map[string]string{"window": "five_hour", "source": "cache", "account": "aaaa1111"}),
			ser("60", map[string]string{"window": "five_hour", "source": "statusline", "account": "bbbb2222"}),
			ser("90", map[string]string{"window": "five_hour", "source": "cache", "account": "bbbb2222"}),
		},
		ageByAccountQuery: {
			// Account a's tap is nine hours cold and its cache fresh; b is
			// the other way round. max() would take 14 and 90; freshness
			// takes 0 and 60.
			ser("32400", map[string]string{"source": "statusline", "account": "aaaa1111"}),
			ser("30", map[string]string{"source": "cache", "account": "aaaa1111"}),
			ser("30", map[string]string{"source": "statusline", "account": "bbbb2222"}),
			ser("32400", map[string]string{"source": "cache", "account": "bbbb2222"}),
		},
	}}

	got, err := LoadAccountWindows(context.Background(), f)
	if err != nil {
		t.Fatalf("LoadAccountWindows: %v", err)
	}
	byAcct := map[string]float64{}
	for _, a := range got {
		byAcct[a.Account] = MaxPct(a.Windows)
	}
	if byAcct["aaaa1111"] != 0 {
		t.Errorf("account aaaa1111 = %v, want 0 from the fresh cache, not 14 from the cold tap", byAcct["aaaa1111"])
	}
	if byAcct["bbbb2222"] != 60 {
		t.Errorf("account bbbb2222 = %v, want 60 from the fresh tap, not 90 from the cold cache", byAcct["bbbb2222"])
	}
}

// TestMaxPctReportsMinusOneWithNoPercentages, so a caller can tell "no data"
// from "zero percent used".
func TestMaxPctReportsMinusOneWithNoPercentages(t *testing.T) {
	if got := MaxPct([]Window{{Name: "five_hour", HasReset: true}}); got != -1 {
		t.Errorf("MaxPct with no pct = %v, want -1", got)
	}
}

// TestLoadAccountWindowsDropsUnlabelledSeries.
//
// Observed on a live upgrade: the previous poller's unlabelled samples were
// still inside the next query's lookbehind, so grouping by account produced a
// nameless fifth "account" on a four account machine, carrying a copy of the
// primary account's 22% and its $878.93 billed figure. The same dollars were
// reported twice, once under a row with no name to explain itself.
//
// It ages out within the lookbehind, which is precisely why it is worth
// dropping in code rather than waiting: a bug that disappears before anyone
// can investigate it is one that gets rediscovered on every single upgrade.
func TestLoadAccountWindowsDropsUnlabelledSeries(t *testing.T) {
	f := fakeQ{byQuery: map[string][]vm.Series{
		pctByAccountQuery: {
			ser("22", map[string]string{"window": "five_hour", "source": "cache", "account": "aaaa1111"}),
			// No account label: a leftover from before the label existed.
			ser("22", map[string]string{"window": "five_hour", "source": "cache"}),
		},
	}}

	got, err := LoadAccountWindows(context.Background(), f)
	if err != nil {
		t.Fatalf("LoadAccountWindows: %v", err)
	}
	if len(got) != 1 {
		var names []string
		for _, a := range got {
			names = append(names, "\""+a.Account+"\"")
		}
		t.Fatalf("want 1 account, got %d (%v); an unlabelled series is not an account", len(got), names)
	}
	if got[0].Account != "aaaa1111" {
		t.Errorf("kept the wrong one: %q", got[0].Account)
	}
}

// The confirmation age is a different question from Ages, and the pair has to
// stay separable: an account whose percentage has genuinely not moved in an
// hour has an old cache Age and a young ConfirmedAgeS, and both readings are
// correct. Collapsing them would either call a working poller stale (what
// happened before this existed) or call a dead one fresh.
func TestLoadAccountWindowsKeepsConfirmationApartFromChange(t *testing.T) {
	q := fakeQ{byQuery: map[string][]vm.Series{
		pctByAccountQuery: {
			ser("70", map[string]string{"window": "five_hour", "source": "cache", "account": "aaaa"}),
			ser("23", map[string]string{"window": "five_hour", "source": "cache", "account": "bbbb"}),
		},
		ageByAccountQuery: {
			ser("2640", map[string]string{"source": "cache", "account": "aaaa"}),
			ser("120", map[string]string{"source": "cache", "account": "bbbb"}),
		},
		confirmedByAccountQuery: {
			ser("120", map[string]string{"account": "aaaa"}),
		},
	}}

	got, err := LoadAccountWindows(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	byAcct := map[string]AccountWindows{}
	for _, a := range got {
		byAcct[a.Account] = a
	}

	// aaaa: unchanged for 44 minutes, confirmed 2 minutes ago. Both survive.
	a := byAcct["aaaa"]
	if !a.HasConfirmedAge || a.ConfirmedAgeS != 120 {
		t.Errorf("aaaa confirmed = %v (known %v), want 120", a.ConfirmedAgeS, a.HasConfirmedAge)
	}
	if a.Ages["cache"] != 2640 {
		t.Errorf("aaaa cache age = %v, want 2640 kept alongside the confirmation", a.Ages["cache"])
	}

	// bbbb: nothing ever confirmed it. Absent, not zero. A zero would read as
	// "confirmed just now", which is the opposite of what is known.
	b := byAcct["bbbb"]
	if b.HasConfirmedAge {
		t.Errorf("bbbb reported a confirmation nothing recorded: %v", b.ConfirmedAgeS)
	}
	if b.Ages["cache"] != 120 {
		t.Errorf("bbbb cache age = %v, want 120", b.Ages["cache"])
	}
}
