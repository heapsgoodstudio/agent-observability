// Package report is the shape of a generated report: the JSON object one
// headless `claude -p` run prints, parsed and normalized into something the
// rest of the stack can render. Pure functions only, no I/O, in the style of
// internal/rules, so a report can be validated without a runner, a store or a
// filesystem.
//
// Parsing is deliberately tolerant and normalizing rather than strict. A run
// has already spent real money by the time its output arrives, so a stray
// sentence or a fence must not throw the report away, and a band value outside
// its set is blanked (the UI then omits a chip) rather than failing the whole
// document. Only three things are fatal: no JSON object at all, the wrong
// schema number, and a missing headline or summary. Those two fields are what
// the session Overview card is made of, so a report without them has nothing
// to be short about.
// CLI hook rejections are execution failures and are rejected before trying
// to parse any echoed prompt content as a report.
//
// This schema is spelled in three places that must agree: the prompt contract
// at internal/analysis/prompts/report-contract.md, the structs below, and the
// proto Report message in proto/claudeobs/v1/obs.proto.
package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// Schema is the usage-focused report version. Schema 1 remains parseable for
// compatibility, but is not eligible for display as an allowance-aware report.
const Schema = 2

// List caps. The prompt asks for these; they are enforced here so a model that
// ignores one cannot turn a report into a page of twenty cards.
const (
	MaxStory    = 4
	MaxDrivers  = 6
	MaxProblems = 5
	MaxEvidence = 4
	MaxGood     = 4
	MaxActions  = 5
	MaxConfig   = 5
	MaxFine     = 4
)

// The band vocabularies. Keeping the sets this small is deliberate: every
// extra word is one more tone the UI has to have an answer for.
var (
	verdicts  = map[string]bool{"efficient": true, "mixed": true, "wasteful": true}
	avoidable = map[string]bool{"none": true, "small": true, "moderate": true, "large": true}
	sizes     = map[string]bool{"small": true, "moderate": true, "large": true}
	efforts   = map[string]bool{"low": true, "medium": true, "high": true}
)

// Sentinel errors. A caller distinguishes them only for logging: every one of
// them means the same thing operationally, which is to keep the raw output as
// a markdown fallback.
var (
	ErrNotJSON    = errors.New("report: no JSON object in the output")
	ErrSchema     = errors.New("report: unsupported schema version")
	ErrIncomplete = errors.New("report: headline and summary are both required")
)

// Driver is one row of "where the money went": a real measured amount, so it
// carries figures rather than a band.
type Driver struct {
	Label     string  `json:"label"`
	AmountUSD float64 `json:"amount_usd"`
	SharePct  float64 `json:"share_pct"`
	Plain     string  `json:"plain"`
}

// Problem is one card: what happened, why that costs money, and what to do
// instead. Evidence is the receipts, collapsed in the UI.
type Problem struct {
	Confidence   string   `json:"confidence,omitempty"`
	Category     string   `json:"category,omitempty"`
	Title        string   `json:"title"`
	WhatHappened string   `json:"what_happened"`
	WhyItCosts   string   `json:"why_it_costs"`
	Fix          string   `json:"fix"`
	Cost         string   `json:"cost"`
	Evidence     []string `json:"evidence,omitempty"`
}

// Good is something worth keeping, with the reason it is worth keeping. A
// report that only lists faults teaches nothing.
type Good struct {
	Title   string `json:"title"`
	What    string `json:"what"`
	WhyGood string `json:"why_good"`
}

// Action is one ranked next step. Effort and saving are bands, never dollar
// figures: the model can rank changes against each other, but it cannot know
// what a change would have saved.
type Action struct {
	Verification string `json:"verification,omitempty"`
	Do           string `json:"do"`
	Why          string `json:"why"`
	Effort       string `json:"effort"`
	Saving       string `json:"saving"`
}

// ConfigChange is an advice-only edit to make, quoting the setting it means.
type ConfigChange struct {
	File    string `json:"file"`
	Setting string `json:"setting"`
	Change  string `json:"change"`
	Why     string `json:"why"`
}

// Fine is something that looks alarming and is not a problem here. Naming
// these is what stops a report inventing problems to fill a section.
type Fine struct {
	Title   string `json:"title"`
	WhyFine string `json:"why_fine"`
}

// Report is one whole generated report, for either kind. Story reads as "what
// this session was doing" for an analysis and "what your usage looks like" for
// advice; ConfigChanges only ever appears on advice. Absent sections are
// absent, and the UI draws what is there.
type Report struct {
	Schema        int            `json:"schema"`
	Verdict       string         `json:"verdict,omitempty"`
	Avoidable     string         `json:"avoidable,omitempty"`
	Headline      string         `json:"headline"`
	Summary       string         `json:"summary"`
	Coverage      string         `json:"coverage,omitempty"`
	Story         []string       `json:"story,omitempty"`
	CostDrivers   []Driver       `json:"cost_drivers,omitempty"`
	Problems      []Problem      `json:"problems,omitempty"`
	Good          []Good         `json:"good,omitempty"`
	Actions       []Action       `json:"actions,omitempty"`
	ConfigChanges []ConfigChange `json:"config_changes,omitempty"`
	Fine          []Fine         `json:"fine,omitempty"`
}

// Parse turns one run's stdout into a normalized report.
func Parse(raw []byte) (*Report, error) {
	if err := ExecutionError(raw); err != nil {
		return nil, err
	}
	body, ok := jsonObject(raw)
	if !ok {
		return nil, ErrNotJSON
	}
	var r Report
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotJSON, err)
	}
	if r.Schema != 1 && r.Schema != Schema {
		return nil, fmt.Errorf("%w: %d", ErrSchema, r.Schema)
	}
	r.normalize()
	if r.Headline == "" || r.Summary == "" {
		return nil, ErrIncomplete
	}
	return &r, nil
}

// AllowanceAware distinguishes the usage-focused contract from legacy reports
// that valued every token as dollars. Legacy reports remain readable on disk,
// but must not reintroduce included-usage valuations into the dashboard.
func (r *Report) AllowanceAware() bool {
	if r == nil || r.Schema != Schema || len(r.CostDrivers) != 0 {
		return false
	}
	data, err := json.Marshal(r)
	return err == nil && !monetaryFigure.Match(data)
}

// The prompt and schema establish the contract; this check additionally keeps
// an obvious generated monetary figure from silently violating it. These
// reports have no request-level billing evidence and should contain none.
var monetaryFigure = regexp.MustCompile(`(?i)([$€£]\s*[0-9]|[0-9][0-9,. ]*\s*(USD|EUR|GBP|dollars?)\b)`)

// Marshal renders a report the way it lands on disk: one-space indentation, so
// the file is readable by whoever opens it rather than one long line.
func Marshal(r *Report) ([]byte, error) {
	return json.MarshalIndent(r, "", " ")
}

// jsonObject pulls the JSON object out of stdout: a fenced block loses its
// fences, and prose either side of the outermost braces is dropped. The prompt
// asks for bare JSON; this is what forgives a model that framed it anyway.
func jsonObject(raw []byte) ([]byte, bool) {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		if j := strings.LastIndex(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return nil, false
	}
	return []byte(s[start : end+1]), true
}

// normalize trims, bands, clamps, drops and caps. It never rejects and never
// truncates prose mid-sentence: the word caps are the prompt's job, and an
// over-long field renders long, which is a visible prompt problem rather than
// a silently mangled sentence.
func (r *Report) normalize() {
	r.Verdict = oneOf(r.Verdict, verdicts)
	r.Avoidable = oneOf(r.Avoidable, avoidable)
	r.Headline = strings.TrimSpace(r.Headline)
	r.Summary = strings.TrimSpace(r.Summary)
	r.Coverage = strings.TrimSpace(r.Coverage)
	r.Story = limit(nonBlank(r.Story), MaxStory)

	drivers := make([]Driver, 0, len(r.CostDrivers))
	for _, d := range r.CostDrivers {
		d.Label = strings.TrimSpace(d.Label)
		d.Plain = strings.TrimSpace(d.Plain)
		if d.Label == "" {
			continue
		}
		d.AmountUSD = nonNegative(d.AmountUSD)
		d.SharePct = clampPct(d.SharePct)
		drivers = append(drivers, d)
	}
	r.CostDrivers = limit(drivers, MaxDrivers)

	problems := make([]Problem, 0, len(r.Problems))
	for _, p := range r.Problems {
		p.Title = strings.TrimSpace(p.Title)
		p.WhatHappened = strings.TrimSpace(p.WhatHappened)
		p.WhyItCosts = strings.TrimSpace(p.WhyItCosts)
		p.Fix = strings.TrimSpace(p.Fix)
		p.Cost = oneOf(p.Cost, sizes)
		p.Confidence = oneOf(p.Confidence, map[string]bool{"high": true, "medium": true, "low": true})
		p.Category = oneOf(p.Category, map[string]bool{"outcome": true, "verification": true, "instructions": true, "context": true, "tools": true, "delegation": true, "recovery": true})
		if p.Title == "" {
			continue
		}
		p.Evidence = limit(nonBlank(p.Evidence), MaxEvidence)
		problems = append(problems, p)
	}
	r.Problems = limit(problems, MaxProblems)

	good := make([]Good, 0, len(r.Good))
	for _, g := range r.Good {
		g.Title = strings.TrimSpace(g.Title)
		g.What = strings.TrimSpace(g.What)
		g.WhyGood = strings.TrimSpace(g.WhyGood)
		if g.Title == "" {
			continue
		}
		good = append(good, g)
	}
	r.Good = limit(good, MaxGood)

	actions := make([]Action, 0, len(r.Actions))
	for _, a := range r.Actions {
		a.Do = strings.TrimSpace(a.Do)
		a.Why = strings.TrimSpace(a.Why)
		a.Effort = oneOf(a.Effort, efforts)
		a.Saving = oneOf(a.Saving, sizes)
		if a.Do == "" {
			continue
		}
		actions = append(actions, a)
	}
	r.Actions = limit(actions, MaxActions)

	changes := make([]ConfigChange, 0, len(r.ConfigChanges))
	for _, c := range r.ConfigChanges {
		c.File = strings.TrimSpace(c.File)
		c.Setting = strings.TrimSpace(c.Setting)
		c.Change = strings.TrimSpace(c.Change)
		c.Why = strings.TrimSpace(c.Why)
		if c.Setting == "" {
			continue
		}
		changes = append(changes, c)
	}
	r.ConfigChanges = limit(changes, MaxConfig)

	fine := make([]Fine, 0, len(r.Fine))
	for _, f := range r.Fine {
		f.Title = strings.TrimSpace(f.Title)
		f.WhyFine = strings.TrimSpace(f.WhyFine)
		if f.Title == "" {
			continue
		}
		fine = append(fine, f)
	}
	r.Fine = limit(fine, MaxFine)
}

// oneOf lowercases a band value and blanks anything outside its set, so the UI
// omits a chip rather than printing a word it has no tone for.
func oneOf(v string, allowed map[string]bool) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if !allowed[v] {
		return ""
	}
	return v
}

// limit truncates a list to n entries.
func limit[T any](in []T, n int) []T {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

// nonBlank trims each string and drops the ones left empty. A blank bullet
// renders as a blank bullet, which reads as a rendering bug.
func nonBlank(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// nonNegative zeroes a figure that cannot be money: negative, NaN or Inf.
func nonNegative(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

// clampPct holds a share inside 0 to 100. A share computed against the wrong
// denominator should fill the track, not overflow it.
func clampPct(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
