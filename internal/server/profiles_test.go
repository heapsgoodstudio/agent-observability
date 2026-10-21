package server

import (
	"context"
	"net/http/httptest"
	"testing"

	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/store"
	"github.com/heapsgoodstudio/agent-observability/internal/store/vm"
)

func profileOptions(vmSrv *httptest.Server, primary string) Options {
	return Options{
		VM:             &vm.Client{BaseURL: vmSrv.URL, HTTP: vmSrv.Client()},
		Memo:           store.New(store.TTL),
		PrimaryProfile: primary,
	}
}

// oneInfoSeries renders the info gauge's answer for a set of profiles.
func infoBody(rows ...[3]string) string {
	out := `{"status":"success","data":{"result":[`
	for i, r := range rows {
		if i > 0 {
			out += ","
		}
		out += `{"metric":{"__name__":"claude_profile_account_info","profile":"` + r[0] +
			`","account":"` + r[1] + `","domain":"` + r[2] + `"},"value":[1,"1"]}`
	}
	return out + `]}}`
}

func findProfile(rows []*claudeobsv1.ProfileRow, name string) *claudeobsv1.ProfileRow {
	for _, r := range rows {
		if r.Profile == name {
			return r
		}
	}
	return nil
}

// Two profiles signed in to one account must carry the SAME percentage and the
// SAME account id, because they share one allowance. A roster that gave each
// its own figure would let a client draw one allowance as two, which is the
// mistake the account label exists to prevent.
func TestListProfilesGivesSharedAccountsOneFigure(t *testing.T) {
	srv := httptest.NewServer(fakeVMHandler{instant: map[string]string{
		accountInfoQuery: infoBody(
			[3]string{"studio", "1a2b3c4d", "studio.example"},
			[3]string{"default", "1a2b3c4d", "studio.example"},
			[3]string{"acme", "5e6f7a8b", "work.example"},
		),
		`max by (window,source,account)(claude_plan_window_pct)`: `{"status":"success","data":{"result":[` +
			`{"metric":{"window":"seven_day","source":"cache","account":"1a2b3c4d"},"value":[1,"49"]},` +
			`{"metric":{"window":"seven_day","source":"cache","account":"5e6f7a8b"},"value":[1,"23"]}]}}`,
	}})
	defer srv.Close()

	resp, err := buildListProfiles(context.Background(), profileOptions(srv, "acme"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Profiles) != 3 {
		t.Fatalf("got %d profiles, want 3", len(resp.Profiles))
	}

	hg, def := findProfile(resp.Profiles, "studio"), findProfile(resp.Profiles, "default")
	if hg == nil || def == nil {
		t.Fatal("studio or default missing from the roster")
	}
	if hg.Account != def.Account {
		t.Errorf("shared account split: studio=%q default=%q", hg.Account, def.Account)
	}
	if hg.MaxPct != def.MaxPct {
		t.Errorf("shared allowance shown as two figures: studio=%v default=%v", hg.MaxPct, def.MaxPct)
	}
	// The control: a genuinely separate account must NOT share that figure,
	// or the test above would pass against a loader that gave everyone the
	// same number.
	if acme := findProfile(resp.Profiles, "acme"); acme == nil || acme.MaxPct == hg.MaxPct {
		t.Errorf("separate accounts collapsed onto one figure")
	}
}

// The picker opens on whatever is first, so first has to be the account
// closest to throttling. This is the complaint the selector exists to answer:
// personal at 95% was off screen while the page opened on acme at 23%.
func TestListProfilesPutsTheHighestAccountFirst(t *testing.T) {
	srv := httptest.NewServer(fakeVMHandler{instant: map[string]string{
		accountInfoQuery: infoBody(
			[3]string{"acme", "5e6f7a8b", "work.example"},
			[3]string{"personal", "9c0d1e2f", "personal.example"},
			[3]string{"quiet", "aaaaaaaa", "example.com"},
		),
		`max by (window,source,account)(claude_plan_window_pct)`: `{"status":"success","data":{"result":[` +
			`{"metric":{"window":"seven_day","source":"cache","account":"5e6f7a8b"},"value":[1,"23"]},` +
			`{"metric":{"window":"seven_day","source":"cache","account":"9c0d1e2f"},"value":[1,"95"]}]}}`,
	}})
	defer srv.Close()

	resp, err := buildListProfiles(context.Background(), profileOptions(srv, "acme"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range resp.Profiles {
		order = append(order, r.Profile)
	}
	want := []string{"personal", "acme", "quiet"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("order = %v, want %v (highest first, unknown last)", order, want)
		}
	}
	// A profile whose account reports no window at all must say so rather
	// than sort as a zero, which would read as an idle account.
	if q := findProfile(resp.Profiles, "quiet"); q == nil || q.HasMaxPct {
		t.Error("an account with no window reported a percentage")
	}
}

// A profile the poller cannot fetch usage for is the reason this field
// exists: its plan-window metrics will never appear, and a permanent zero
// reads as a broken poller rather than a profile nobody is signed in to.
// Unknown has to stay distinct from false for the same reason.
func TestListProfilesReportsWhetherUsageCanBeFetched(t *testing.T) {
	srv := httptest.NewServer(fakeVMHandler{instant: map[string]string{
		accountInfoQuery: infoBody(
			[3]string{"default", "1a2b3c4d", "studio.example"},
			[3]string{"studio", "1a2b3c4d", "studio.example"},
			[3]string{"acme", "5e6f7a8b", "work.example"},
		),
		usageAvailableQuery: `{"status":"success","data":{"result":[` +
			`{"metric":{"profile":"default"},"value":[1,"0"]},` +
			`{"metric":{"profile":"studio"},"value":[1,"1"]}]}}`,
	}})
	defer srv.Close()

	resp, err := buildListProfiles(context.Background(), profileOptions(srv, "acme"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		profile     string
		wantKnown   bool
		wantAvail   bool
		whatItMeans string
	}{
		{"default", true, false, "signed out: says so"},
		{"studio", true, true, "signed in: says so"},
		{"acme", false, false, "no probe read: says nothing"},
	}
	for _, c := range cases {
		r := findProfile(resp.Profiles, c.profile)
		if r == nil {
			t.Fatalf("%s missing", c.profile)
		}
		if r.UsageAvailableKnown != c.wantKnown || r.UsageAvailable != c.wantAvail {
			t.Errorf("%s (%s): known=%v available=%v, want known=%v available=%v",
				c.profile, c.whatItMeans, r.UsageAvailableKnown, r.UsageAvailable, c.wantKnown, c.wantAvail)
		}
	}
}

// The roster comes from the info gauge rather than from the account windows
// precisely so a profile whose account reports nothing still appears. A
// selector that silently omits a profile is worse than one that shows it with
// nothing to say: the reader cannot select what is not listed.
func TestListProfilesListsAProfileWhoseAccountHasNoWindows(t *testing.T) {
	srv := httptest.NewServer(fakeVMHandler{instant: map[string]string{
		accountInfoQuery: infoBody([3]string{"lapsed", "deadbeef", "example.com"}),
	}})
	defer srv.Close()

	resp, err := buildListProfiles(context.Background(), profileOptions(srv, "lapsed"), testNow)
	if err != nil {
		t.Fatal(err)
	}
	r := findProfile(resp.Profiles, "lapsed")
	if r == nil {
		t.Fatal("a profile with no account windows was dropped from the roster")
	}
	if r.HasMaxPct {
		t.Error("a profile with no windows reported a percentage")
	}
	if !r.IsPrimary {
		t.Error("the primary profile is not marked")
	}
}

/* profileScope -------------------------------------------------------------- */

// The two halves of a scope narrow different things, and swapping them is the
// mistake that would report one profile's spend as billed against another
// account's overage windows.
func TestProfileScopeSeparatesProfileFromAccount(t *testing.T) {
	sc := profileScope{Profile: "studio", Account: "1a2b3c4d"}

	if got := sc.match()["profile"]; got != "studio" {
		t.Errorf("match() profile = %q, want studio", got)
	}
	if _, ok := sc.match()["account"]; ok {
		t.Error("match() carries an account label; per-profile metrics have none")
	}
	if got := sc.accountMatch()["account"]; got != "1a2b3c4d" {
		t.Errorf("accountMatch() account = %q, want 1a2b3c4d", got)
	}
	if _, ok := sc.accountMatch()["profile"]; ok {
		t.Error("accountMatch() carries a profile label; an allowance is not per profile")
	}

	// An unselected scope must leave every query byte-identical to what it
	// was before a selector existed, or the whole unfiltered page changes
	// behaviour and its cached query strings all miss.
	empty := profileScope{}
	if empty.match() != nil || empty.accountMatch() != nil {
		t.Error("an empty scope narrows something")
	}
	if got := empty.acct("claude_in_overage"); got != "claude_in_overage" {
		t.Errorf("empty scope rewrote a metric name: %q", got)
	}
}

// A selected profile whose account is unknown must not fall back to another
// account's bands. Leaving the bands unnarrowed overstates which spend was
// billed, which a reader can see and argue with; a wrong account's bands
// cannot be argued with because nothing on screen says which account they are.
func TestProfileScopeWithNoAccountDoesNotGuessOne(t *testing.T) {
	sc := profileScope{Profile: "orphan"}
	if got := sc.bandSelector(); got != "claude_in_overage" {
		t.Errorf("bandSelector() = %q, want the unnarrowed metric", got)
	}
	if sc.accountMatch() != nil {
		t.Error("a scope with no account produced an account filter")
	}
	// The profile half still narrows: the profile IS known.
	if got := sc.match()["profile"]; got != "orphan" {
		t.Errorf("match() profile = %q, want orphan", got)
	}
}
