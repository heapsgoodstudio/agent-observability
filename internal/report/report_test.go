package report

import (
	"errors"
	"strings"
	"testing"
)

// minimal is the smallest report Parse accepts: schema, headline, summary.
const minimal = `{"schema":1,"headline":"H","summary":"S"}`

func TestAllowanceAwareReportsRequireNewContractWithoutMoney(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{minimal, false},
		{`{"schema":2,"headline":"Repeated reads","summary":"Twenty requests reread the same context."}`, true},
		{`{"schema":2,"headline":"Repeated reads","summary":"This cost $15.00."}`, false},
		{`{"schema":2,"headline":"Repeated reads","summary":"This cost 15 USD."}`, false},
		{`{"schema":2,"headline":"Repeated reads","summary":"Activity","cost_drivers":[{"label":"included","amount_usd":1}]}`, false},
	} {
		r, err := Parse([]byte(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		if r.AllowanceAware() != tc.want {
			t.Errorf("AllowanceAware(%s) = %v, want %v", tc.raw, r.AllowanceAware(), tc.want)
		}
	}
}

func TestParseMinimal(t *testing.T) {
	r, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.Headline != "H" || r.Summary != "S" {
		t.Errorf("got headline %q summary %q, want H/S", r.Headline, r.Summary)
	}
}

func TestParseTolerance(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"fenced", "```json\n" + minimal + "\n```"},
		{"fenced without language", "```\n" + minimal + "\n```"},
		{"leading prose", "Here is the report:\n\n" + minimal},
		{"trailing prose", minimal + "\n\nLet me know if you want more."},
		{"leading whitespace", "\n\n  " + minimal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Parse([]byte(c.in))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if r.Headline != "H" {
				t.Errorf("headline = %q, want H", r.Headline)
			}
		})
	}
}

func TestParseRejections(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"prose only", "# Report\n\nAll fine.", ErrNotJSON},
		{"empty", "", ErrNotJSON},
		{"truncated json", `{"schema":1,"headline":`, ErrNotJSON},
		{"wrong schema", `{"schema":3,"headline":"H","summary":"S"}`, ErrSchema},
		{"no schema", `{"headline":"H","summary":"S"}`, ErrSchema},
		{"no headline", `{"schema":1,"summary":"S"}`, ErrIncomplete},
		{"whitespace headline", `{"schema":1,"headline":"   ","summary":"S"}`, ErrIncomplete},
		{"no summary", `{"schema":1,"headline":"H"}`, ErrIncomplete},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.in)); !errors.Is(err, c.want) {
				t.Errorf("Parse error = %v, want %v", err, c.want)
			}
		})
	}
}

func TestParseIgnoresUnknownFields(t *testing.T) {
	if _, err := Parse([]byte(`{"schema":1,"headline":"H","summary":"S","mood":"chipper"}`)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestNormalizeBands(t *testing.T) {
	r, err := Parse([]byte(`{"schema":1,"headline":"H","summary":"S",
	  "verdict":"MIXED","avoidable":"enormous",
	  "problems":[{"title":"T","cost":" Large "}],
	  "actions":[{"do":"D","effort":"sometimes","saving":"moderate"}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.Verdict != "mixed" {
		t.Errorf("verdict = %q, want mixed (lowercased)", r.Verdict)
	}
	if r.Avoidable != "" {
		t.Errorf("avoidable = %q, want blank for a value outside the set", r.Avoidable)
	}
	if r.Problems[0].Cost != "large" {
		t.Errorf("problem cost = %q, want large (trimmed and lowercased)", r.Problems[0].Cost)
	}
	if r.Actions[0].Effort != "" {
		t.Errorf("effort = %q, want blank for a value outside the set", r.Actions[0].Effort)
	}
	if r.Actions[0].Saving != "moderate" {
		t.Errorf("saving = %q, want moderate", r.Actions[0].Saving)
	}
}

func TestNormalizeFigures(t *testing.T) {
	r, err := Parse([]byte(`{"schema":1,"headline":"H","summary":"S","cost_drivers":[
	  {"label":"over","amount_usd":5,"share_pct":140},
	  {"label":"under","amount_usd":-3,"share_pct":-10}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CostDrivers[0].SharePct != 100 {
		t.Errorf("share = %v, want clamped to 100", r.CostDrivers[0].SharePct)
	}
	if r.CostDrivers[1].SharePct != 0 {
		t.Errorf("share = %v, want clamped to 0", r.CostDrivers[1].SharePct)
	}
	if r.CostDrivers[1].AmountUSD != 0 {
		t.Errorf("amount = %v, want a negative amount zeroed", r.CostDrivers[1].AmountUSD)
	}
}

func TestNormalizeDropsEmptyEntries(t *testing.T) {
	r, err := Parse([]byte(`{"schema":1,"headline":"H","summary":"S",
	  "story":["one","  ",""],
	  "cost_drivers":[{"label":"","amount_usd":1},{"label":"keep"}],
	  "problems":[{"title":""},{"title":"keep","evidence":["e","  "]}],
	  "good":[{"title":""},{"title":"keep"}],
	  "actions":[{"do":""},{"do":"keep"}],
	  "config_changes":[{"setting":""},{"setting":"keep"}],
	  "fine":[{"title":""},{"title":"keep"}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(r.Story) != 1 || r.Story[0] != "one" {
		t.Errorf("story = %v, want the one non-blank bullet", r.Story)
	}
	if len(r.CostDrivers) != 1 || r.CostDrivers[0].Label != "keep" {
		t.Errorf("cost_drivers = %v, want the labelled one only", r.CostDrivers)
	}
	if len(r.Problems) != 1 || len(r.Problems[0].Evidence) != 1 {
		t.Errorf("problems = %v, want one problem with one evidence line", r.Problems)
	}
	if len(r.Good) != 1 || len(r.Actions) != 1 || len(r.ConfigChanges) != 1 || len(r.Fine) != 1 {
		t.Errorf("untitled entries survived: %+v", r)
	}
}

func TestNormalizeCaps(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"schema":1,"headline":"H","summary":"S","problems":[`)
	for i := 0; i < 9; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"title":"p"}`)
	}
	b.WriteString(`]}`)
	r, err := Parse([]byte(b.String()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(r.Problems) != MaxProblems {
		t.Errorf("problems = %d, want capped at %d", len(r.Problems), MaxProblems)
	}
}

func TestMarshalRoundTrips(t *testing.T) {
	r, err := Parse([]byte(`{"schema":1,"headline":"H","summary":"S","verdict":"mixed"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	data, err := Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatalf("re-Parse: %v", err)
	}
	if back.Verdict != "mixed" || back.Headline != "H" {
		t.Errorf("round trip lost fields: %+v", back)
	}
}
