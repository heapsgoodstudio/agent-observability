package server

import (
	"context"
	"net/http/httptest"
	"testing"

	claudeobsv1 "github.com/heapsgoodstudio/agent-observability/gen/claudeobs/v1"
	"github.com/heapsgoodstudio/agent-observability/internal/usage"
)

func win(name string, pct float64, has bool) *claudeobsv1.UsageWindow {
	return &claudeobsv1.UsageWindow{Window: name, Pct: pct, HasPct: has, Source: "cache"}
}

// needsProbe is what decides whether a caller's max_age_s spends three seconds
// and a process. Each branch is a distinct claim about what is known.
func TestNeedsProbe(t *testing.T) {
	cases := []struct {
		name string
		row  *claudeobsv1.UsageProfile
		want bool
		why  string
	}{
		{
			name: "fresh enough",
			row:  &claudeobsv1.UsageProfile{ConfirmedAgeS: 30, ConfirmedAgeKnown: true},
			want: false, why: "confirmed inside the window: nothing to gain",
		},
		{
			name: "too old",
			row:  &claudeobsv1.UsageProfile{ConfirmedAgeS: 400, ConfirmedAgeKnown: true},
			want: true, why: "confirmed outside the window: that is what max_age_s asked about",
		},
		{
			name: "never confirmed",
			row:  &claudeobsv1.UsageProfile{ConfirmedAgeKnown: false},
			want: true, why: "nothing has ever confirmed this, which is further from fresh than any number",
		},
		{
			name: "signed out",
			row: &claudeobsv1.UsageProfile{
				UsageAvailableKnown: true, UsageAvailable: false, ConfirmedAgeKnown: false,
			},
			want: false, why: "Claude Code already said it cannot fetch usage; probing repeats the question",
		},
		{
			name: "availability unknown",
			row:  &claudeobsv1.UsageProfile{UsageAvailableKnown: false, ConfirmedAgeKnown: false},
			want: true, why: "not knowing whether it can answer is a reason to ask, not to skip",
		},
		{
			name: "exactly at the boundary",
			row:  &claudeobsv1.UsageProfile{ConfirmedAgeS: 60, ConfirmedAgeKnown: true},
			want: false, why: "max_age_s is an upper bound the reading may sit on",
		},
	}
	for _, c := range cases {
		if got := needsProbe(c.row, 60); got != c.want {
			t.Errorf("%s: needsProbe = %v, want %v (%s)", c.name, got, c.want, c.why)
		}
	}
}

// A live reading replaces the stored percentages and marks itself as the
// confirmation. The change age is deliberately untouched: a fetch returning
// the same numbers does not make them newer.
func TestApplyProbeOverlaysAndStampsConfirmation(t *testing.T) {
	row := &claudeobsv1.UsageProfile{
		Profile:       "acme",
		Windows:       []*claudeobsv1.UsageWindow{win("five_hour", 23, true), win("seven_day", 16, true)},
		ConfirmedAgeS: 900, ConfirmedAgeKnown: true,
		ChangedAgeS: 3400, ChangedAgeKnown: true,
	}
	applyProbe(row, usage.Result{
		Available: true,
		Windows: map[string]usage.Window{
			"five_hour": {Pct: 91, ResetTS: 1790000000, HasReset: true},
			"seven_day": {Pct: 26},
		},
	})

	if !row.Probed {
		t.Error("the row does not say it was probed")
	}
	if row.ConfirmedAgeS != 0 || !row.ConfirmedAgeKnown {
		t.Errorf("confirmed age = %v/%v, want 0/true: this IS the confirmation", row.ConfirmedAgeS, row.ConfirmedAgeKnown)
	}
	if row.ChangedAgeS != 3400 {
		t.Errorf("changed age = %v, want 3400 left alone", row.ChangedAgeS)
	}
	byName := map[string]*claudeobsv1.UsageWindow{}
	for _, w := range row.Windows {
		byName[w.Window] = w
	}
	if got := byName["five_hour"]; got.Pct != 91 || got.Source != "probe" {
		t.Errorf("five_hour = %v source %q, want 91 from probe", got.Pct, got.Source)
	}
	if got := byName["five_hour"]; !got.HasReset || got.ResetTs != 1790000000 {
		t.Errorf("five_hour reset = %v/%v, want the probe's", got.ResetTs, got.HasReset)
	}
	// A window the probe gave no reset for keeps has_reset false rather than
	// reporting epoch 0, which would read as a window that reset in 1970.
	if byName["seven_day"].HasReset {
		t.Error("seven_day claimed a reset the probe did not give")
	}
}

// A probe against a signed-out profile is a successful probe with a real
// answer. It must record that, and must NOT blank the windows into something
// a router could read as an empty account.
func TestApplyProbeRecordsUnavailableWithoutInventingHeadroom(t *testing.T) {
	row := &claudeobsv1.UsageProfile{
		Profile: "default",
		Windows: []*claudeobsv1.UsageWindow{win("five_hour", 65, true)},
	}
	applyProbe(row, usage.Result{Available: false})

	if row.UsageAvailable || !row.UsageAvailableKnown {
		t.Errorf("availability = %v/%v, want false/true", row.UsageAvailable, row.UsageAvailableKnown)
	}
	if len(row.Windows) != 1 || row.Windows[0].Pct != 65 {
		t.Errorf("windows = %v, want the stored reading left alone", row.Windows)
	}
}

// The order is what a caller sees first, and the point is that whichever
// account is closest to throttling leads. A row nothing has measured sorts
// last rather than as a zero, which would put it first among the quiet ones.
func TestSortUsageProfilesRanksByPeakWindow(t *testing.T) {
	rows := []*claudeobsv1.UsageProfile{
		{Profile: "quiet"},
		{Profile: "acme", Windows: []*claudeobsv1.UsageWindow{win("five_hour", 23, true), win("seven_day", 16, true)}},
		{Profile: "personal", Windows: []*claudeobsv1.UsageWindow{win("five_hour", 0, true), win("seven_day", 95, true)}},
		{Profile: "studio", Windows: []*claudeobsv1.UsageWindow{win("five_hour", 49, true)}},
	}
	sortUsageProfiles(rows)

	var got []string
	for _, r := range rows {
		got = append(got, r.Profile)
	}
	want := []string{"personal", "studio", "acme", "quiet"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	// personal leads on its SEVEN_DAY window while its five_hour is 0, so the
	// rank is the peak across windows rather than any single one.
}

// The name must match what the poller stamps on every metric, or a snapshot
// row would never find its own profile's dir to probe.
func TestProfileNameOfMatchesThePollersRule(t *testing.T) {
	cases := map[string]string{
		"/Users/x/Claude/.claude-acme":   "acme",
		"/Users/x/Claude/.claude-acme/":  "acme",
		"/Users/x/.claude":               "default",
		"/Users/x/.claude/":              "default",
		"/Users/x/Claude/.claude-studio": "studio",
	}
	for dir, want := range cases {
		if got := profileNameOf(dir); got != want {
			t.Errorf("profileNameOf(%q) = %q, want %q", dir, got, want)
		}
	}
}

// A probe that comes back signed in but with no windows is a shape change in
// Claude Code's payload, not a reading. Stamping a confirmation there would
// claim the stored figures had just been checked when nothing checked them,
// which is the exact lie the confirmation age exists to prevent.
func TestApplyProbeDoesNotConfirmWhenNoWindowsParsed(t *testing.T) {
	row := &claudeobsv1.UsageProfile{
		Profile:       "acme",
		Windows:       []*claudeobsv1.UsageWindow{win("five_hour", 23, true)},
		ConfirmedAgeS: 900, ConfirmedAgeKnown: true,
	}
	applyProbe(row, usage.Result{Available: true, Windows: map[string]usage.Window{}})

	if row.ConfirmedAgeS != 900 {
		t.Errorf("confirmed age = %v, want 900 left alone", row.ConfirmedAgeS)
	}
	if row.Error == "" {
		t.Error("no error recorded for a probe that parsed no windows")
	}
	if len(row.Windows) != 1 || row.Windows[0].Pct != 23 {
		t.Errorf("windows = %v, want the stored reading left alone", row.Windows)
	}
}

// The gate has to be visible in the response. A max_age_s that silently did
// nothing would leave a caller believing it had asked for and received a fresh
// reading, which is worse than not offering the field: it would read a stale
// number as a live one.
func TestSnapshotSaysWhenOnDemandProbingIsOff(t *testing.T) {
	srv := httptest.NewServer(fakeVMHandler{instant: map[string]string{
		accountInfoQuery: infoBody([3]string{"acme", "5e6f7a8b", "work.example"}),
	}})
	defer srv.Close()

	// No prober and no profile dirs: the shipped default.
	off := profileOptions(srv, "acme")
	resp, err := buildUsageSnapshot(context.Background(), off, 60, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if resp.OnDemandProbeEnabled {
		t.Error("probing reported as enabled with no prober configured")
	}
	if len(resp.Profiles) != 1 {
		t.Fatalf("got %d profiles, want the roster served from the store regardless", len(resp.Profiles))
	}
	// The row is still a real answer: turning probing off must not turn the
	// endpoint off.
	if resp.Profiles[0].Profile != "acme" {
		t.Errorf("profile = %q, want acme", resp.Profiles[0].Profile)
	}
}
