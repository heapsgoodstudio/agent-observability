// Package transcript parses Claude Code transcript JSONL files into the
// per-session summary the indexer stores. Every gating rule, dedup fallback
// and rounding step below is load-bearing and was validated against a
// 2.16 GB corpus of real transcripts; the comments record why each one is
// shaped the way it is, because most of them look arbitrary until a real
// transcript violates the obvious reading.
//
// A session's history lives across one primary transcript plus zero or more
// subagent ("sidechain") transcripts nested under
// <project>/<session-id>/subagents/*.jsonl. Both Scan and Parse take the
// primary path and that nested list, processed as one combined pass:
// totals include both sources; user turns and context observations describe
// only the primary conversation.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/heapsgoodstudio/agent-observability/internal/pricing"
)

// TrajectoryMax is the row cap Parse downsamples a session's trajectory to.
const TrajectoryMax = 300

// Tokens aggregates token counts across all billable requests in a session.
type Tokens struct {
	Input, Output, CacheRead, CacheCreation int64
}

// ModelCost is one model's total cost within a session, already rounded to 6
// decimal places. Models in a Summary are ordered by CostUSD descending,
// ties broken by first-appearance order among the session's requests sorted
// by timestamp.
type ModelCost struct {
	Model   string
	CostUSD float64
}

// Summary is one session's parsed totals and detail rows: the combination of
// parse_transcript's "summary", "tools" and "trajectory" keys (this port
// folds all three into one struct; later tasks split it back out for the
// index row versus the detail file if that split is still needed).
type Summary struct {
	Start, End       int64
	CostUSD          float64
	Requests         int
	Tokens           Tokens
	Models           []ModelCost
	SidechainCostUSD float64
	ToolCalls        int
	UserTurns        int
	Skills           []string
	MCPServers       []string
	Tools            map[string]int
	Trajectory       [][3]float64 // [epoch_seconds, cumulative_cost_usd, main conversation input tokens (including caches); 0 if absent], downsampled to TrajectoryMax
	Unpriced         []string
	// Cwds is every distinct working directory the session's records mention,
	// most-mentioned first. It is ranked rather than reduced to one value
	// because the launch directory is often not where the work happened: a
	// session started in a workspace root that holds several repos spends
	// most of its records inside one of them. internal/workspace turns this
	// ranking into the session's display name.
	Cwds []string
}

// ScannedRequest is one deduplicated, billable API request extracted from a
// transcript, as Scan collects it, before Parse sorts, costs and aggregates
// it. Model is already normalised (pricing.NormModel) at scan time rather
// than at cost time, so the dedup key itself is normalised.
type ScannedRequest struct {
	Timestamp                   string
	Model                       string
	Sidechain                   bool
	InputTokens, OutputTokens   int64
	CacheRead, Cache5m, Cache1h int64
	WebSearchRequests           int64
	Speed, Tier                 string
}

// Scanned is the result of one pass over a session's transcript files.
type Scanned struct {
	// Requests is deduped and in first-seen order of each dedup key; when a
	// key repeats, the VALUE reflects the last occurrence, not the first:
	// a repeat overwrites the value but never moves the key's position.
	Requests []ScannedRequest
	// Tools counts distinct tool_use block ids per tool name, so a block that was
	// streamed as several repeated lines with the same block id still counts
	// once.
	Tools map[string]int
	// Skills holds each unique Skill tool_use input.skill value seen,
	// unsorted (Parse sorts on the way into a Summary).
	Skills    []string
	UserTurns int
	// Cwds ranks the distinct cwd values seen by how many records carried
	// each, most first, ties broken lexicographically so the ranking is
	// deterministic across scans.
	Cwds []string
}

// reqKey is scan_transcript's dedup key: (message.id, requestId) when both
// are present and non-empty, else (message.id,) alone, else a synthetic key
// unique to this scan (uniq > 0 marks that case; mid is only ever empty in
// that branch, so uniq alone disambiguates it from every mid-bearing key).
type reqKey struct {
	mid, rid string
	uniq     int
}

// --- JSON record shapes -----------------------------------------------

type rawRecord struct {
	Type              string          `json:"type"`
	Cwd               string          `json:"cwd"`
	IsSidechain       bool            `json:"isSidechain"`
	Origin            json.RawMessage `json:"origin"`
	Message           json.RawMessage `json:"message"`
	Timestamp         string          `json:"timestamp"`
	RequestID         string          `json:"requestId"`
	IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
}

type rawMessage struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   json.RawMessage `json:"usage"`
}

type rawBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type rawUsage struct {
	InputTokens              int64             `json:"input_tokens"`
	OutputTokens             int64             `json:"output_tokens"`
	CacheReadInputTokens     int64             `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64             `json:"cache_creation_input_tokens"`
	CacheCreation            *rawCacheCreation `json:"cache_creation"`
	Speed                    string            `json:"speed"`
	ServiceTier              string            `json:"service_tier"`
	ServerToolUse            *rawServerToolUse `json:"server_tool_use"`
}

// rawCacheCreation's Ephemeral5m is a pointer because its presence (as
// opposed to its value) is load-bearing: the cost fallback treats "no
// ephemeral_5m_input_tokens key at all" differently from "the key is present
// and 0". Ephemeral1h needs no such distinction: absent and present-as-zero
// both mean zero for it.
type rawCacheCreation struct {
	Ephemeral5m *int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int64  `json:"ephemeral_1h_input_tokens"`
}

type rawServerToolUse struct {
	WebSearchRequests int64 `json:"web_search_requests"`
}

// objIsEmpty reports whether raw is JSON's "falsy" shape for a dict: absent,
// null, or an object with zero keys. The gates below key off this exact
// shape, which is not the same thing as "is this key present":
// {"cache_creation": {}} is still
// falsy, but {"cache_creation": {"ephemeral_5m_input_tokens": 0}} is not,
// even though every numeric field inside is zero.
func objIsEmpty(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return true
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(t, &m); err != nil {
		return false
	}
	return len(m) == 0
}

// originKind reads origin.kind, defaulting to "" when origin is absent or
// malformed (mirrors `(rec.get("origin") or {}).get("kind")`).
func originKind(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var o struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.Kind
}

// isJSONString reports whether raw is a JSON string literal, since a message
// content field is occasionally a bare string rather than a block array.
func isJSONString(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '"'
}

// --- scanning -----------------------------------------------------------

type scanState struct {
	dedup        map[reqKey]ScannedRequest
	order        []reqKey
	uniq         int
	toolsByBlock map[string]string // tool_use block id -> tool name
	skills       map[string]bool
	cwds         map[string]int // cwd -> records carrying it
	userTurns    int
}

// Scan makes one pass over primary and every nested transcript file (in the
// order given, primary first), replicating scan_transcript. nested is
// typically a session's subagent transcripts
// (<project>/<session-id>/subagents/*.jsonl), already sorted by the caller.
func Scan(primary string, nested []string) (*Scanned, error) {
	st := &scanState{
		dedup:        map[reqKey]ScannedRequest{},
		toolsByBlock: map[string]string{},
		skills:       map[string]bool{},
		cwds:         map[string]int{},
	}

	if err := st.scanFile(primary, true); err != nil {
		return nil, err
	}
	for _, p := range nested {
		if err := st.scanFile(p, false); err != nil {
			return nil, err
		}
	}

	requests := make([]ScannedRequest, len(st.order))
	for i, k := range st.order {
		requests[i] = st.dedup[k]
	}

	tools := make(map[string]int, len(st.toolsByBlock))
	for _, name := range st.toolsByBlock {
		tools[name]++
	}

	skills := make([]string, 0, len(st.skills))
	for s := range st.skills {
		skills = append(skills, s)
	}

	return &Scanned{
		Requests:  requests,
		Tools:     tools,
		Skills:    skills,
		Cwds:      rankCwds(st.cwds),
		UserTurns: st.userTurns,
	}, nil
}

// rankCwds orders counts' keys by count descending, ties broken by path
// ascending. The tiebreak is what makes two scans of an untouched transcript
// agree: Go's map iteration order is randomised, so counts alone would let a
// session's name flip between two equally-used directories from pass to pass.
func rankCwds(counts map[string]int) []string {
	out := make([]string, 0, len(counts))
	for c := range counts {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if counts[out[i]] != counts[out[j]] {
			return counts[out[i]] > counts[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// scanFile reads path line by line, feeding each line to processLine. It
// uses bufio.Reader.ReadBytes rather than bufio.Scanner because Scanner's
// default token buffer would truncate the unusually long lines real
// transcripts can contain (large tool outputs); ReadBytes grows without a
// fixed limit, so no line is ever silently truncated.
func (st *scanState) scanFile(path string, primary bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			st.processLine(line, primary)
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// processLine handles one transcript record. Its order of operations is
// load-bearing: a malformed line is skipped silently, tool_use
// and Skill extraction happen for every assistant record regardless of
// whether it turns out billable, and the billability gate is checked last.
func (st *scanState) processLine(line []byte, primary bool) {
	var rec rawRecord
	if err := json.Unmarshal(line, &rec); err != nil {
		return
	}

	// Counted before every type gate below, because cwd rides record types
	// this function otherwise ignores entirely (summaries, meta records, tool
	// results). Gating it on "assistant" would throw away most of the signal.
	if rec.Cwd != "" {
		st.cwds[rec.Cwd]++
	}

	if rec.Type == "user" {
		if primary && !rec.IsSidechain {
			var msg rawMessage
			_ = json.Unmarshal(rec.Message, &msg) // absent/malformed message -> zero value, matching `message or {}`
			kind := originKind(rec.Origin)
			if kind == "human" || (objIsEmpty(rec.Origin) && isJSONString(msg.Content)) {
				st.userTurns++
			}
		}
		return
	}
	if rec.Type != "assistant" {
		return
	}

	var msg rawMessage
	if err := json.Unmarshal(rec.Message, &msg); err != nil {
		msg = rawMessage{}
	}

	var blocks []json.RawMessage
	// Content absent, null, or (rarely) a bare string rather than an array
	// all leave blocks empty here: there are zero block-shaped entries to
	// iterate in any of those cases.
	_ = json.Unmarshal(msg.Content, &blocks)
	for _, rb := range blocks {
		var blk rawBlock
		if json.Unmarshal(rb, &blk) != nil {
			continue // not an object: mirrors `isinstance(block, dict)`
		}
		if blk.Type != "tool_use" {
			continue
		}
		name := blk.Name
		if name == "" {
			name = "unknown"
		}
		bid := blk.ID
		if bid == "" {
			bid = fmt.Sprintf("__noid__%d", len(st.toolsByBlock))
		}
		st.toolsByBlock[bid] = name
		if name == "Skill" {
			var in struct {
				Skill string `json:"skill"`
			}
			if json.Unmarshal(blk.Input, &in) == nil && in.Skill != "" {
				st.skills[in.Skill] = true
			}
		}
	}

	if objIsEmpty(msg.Usage) || msg.Model == "" || msg.Model == "<synthetic>" || rec.IsAPIErrorMessage {
		return
	}
	var usage rawUsage
	if err := json.Unmarshal(msg.Usage, &usage); err != nil {
		return
	}

	var key reqKey
	switch {
	case msg.ID != "" && rec.RequestID != "":
		key = reqKey{mid: msg.ID, rid: rec.RequestID}
	case msg.ID != "":
		key = reqKey{mid: msg.ID}
	default:
		st.uniq++
		key = reqKey{uniq: st.uniq}
	}

	// Cache-creation fallback: prefer the ephemeral 5m/1h breakdown; if there
	// is no breakdown at all (no ephemeral_5m key and no ephemeral_1h),
	// attribute the whole legacy cache_creation_input_tokens total to the 5m
	// bucket. Otherwise, if the breakdown's own sum falls short of that
	// legacy total, top up the shortfall into the 5m bucket.
	var c1 int64
	var c5 int64
	hasC5 := false
	if usage.CacheCreation != nil {
		c1 = usage.CacheCreation.Ephemeral1h
		if usage.CacheCreation.Ephemeral5m != nil {
			c5 = *usage.CacheCreation.Ephemeral5m
			hasC5 = true
		}
	}
	totalCW := usage.CacheCreationInputTokens
	if !hasC5 && c1 == 0 {
		c5, c1 = totalCW, 0
	} else if c5+c1 < totalCW {
		c5 += totalCW - (c5 + c1)
	}

	var ws int64
	if usage.ServerToolUse != nil {
		ws = usage.ServerToolUse.WebSearchRequests
	}

	sidechain := !primary || rec.IsSidechain
	if previous, exists := st.dedup[key]; !exists {
		st.order = append(st.order, key)
	} else if !previous.Sidechain {
		// A main request copied into a nested transcript stays a main request.
		sidechain = false
	}
	st.dedup[key] = ScannedRequest{
		Timestamp:         rec.Timestamp,
		Model:             pricing.NormModel(msg.Model),
		Sidechain:         sidechain,
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		CacheRead:         usage.CacheReadInputTokens,
		Cache5m:           c5,
		Cache1h:           c1,
		WebSearchRequests: ws,
		Speed:             usage.Speed,
		Tier:              usage.ServiceTier,
	}
}

// --- parsing --------------------------------------------------------------

// Parse turns one session's transcript files into a Summary. It returns
// (nil, nil) when the session has no billable API requests at all.
func Parse(primary string, nested []string) (*Summary, error) {
	sc, err := Scan(primary, nested)
	if err != nil {
		return nil, err
	}
	if len(sc.Requests) == 0 {
		return nil, nil
	}

	reqs := append([]ScannedRequest(nil), sc.Requests...)
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].Timestamp < reqs[j].Timestamp })

	var tokens Tokens
	modelTotals := map[string]float64{}
	var modelOrder []string
	seenModel := map[string]bool{}
	unpriced := map[string]bool{}
	var total, side float64
	fullTrajectory := make([][3]float64, 0, len(reqs))

	for _, r := range reqs {
		cost, priced := pricing.RequestCost(
			r.Model, r.InputTokens, r.OutputTokens, r.CacheRead, r.Cache5m, r.Cache1h,
			r.WebSearchRequests, r.Speed, r.Tier,
		)
		if !priced {
			unpriced[r.Model] = true
		}
		total += cost
		if r.Sidechain {
			side += cost
		}
		if !seenModel[r.Model] {
			seenModel[r.Model] = true
			modelOrder = append(modelOrder, r.Model)
		}
		modelTotals[r.Model] += cost

		tokens.Input += r.InputTokens
		tokens.Output += r.OutputTokens
		tokens.CacheRead += r.CacheRead
		tokens.CacheCreation += r.Cache5m + r.Cache1h

		var epoch int64
		if r.Timestamp != "" {
			epoch, err = iso2epoch(r.Timestamp)
			if err != nil {
				return nil, err
			}
		}
		// Cache writes are part of the prompt too. Omitting them makes a cache
		// refresh look like compaction. Subagents have independent contexts;
		// retain their cost point but do not plot it as the main conversation.
		var contextTokens int64
		if !r.Sidechain {
			contextTokens = r.InputTokens + r.CacheRead + r.Cache5m + r.Cache1h
		}
		fullTrajectory = append(fullTrajectory, [3]float64{
			float64(epoch), round6(total), float64(contextTokens),
		})
	}

	// Models must sort on the raw (unrounded) running totals, before each
	// value is rounded for output: sorting after rounding could reorder two
	// models whose rounded costs tie but whose raw totals do not.
	models := make([]ModelCost, len(modelOrder))
	for i, m := range modelOrder {
		models[i] = ModelCost{Model: m, CostUSD: modelTotals[m]}
	}
	sort.SliceStable(models, func(i, j int) bool { return models[i].CostUSD > models[j].CostUSD })
	for i := range models {
		models[i].CostUSD = round6(models[i].CostUSD)
	}

	mcpSet := map[string]bool{}
	for name := range sc.Tools {
		if server, ok := MCPServerOf(name); ok {
			mcpSet[server] = true
		}
	}
	mcpServers := make([]string, 0, len(mcpSet))
	for s := range mcpSet {
		mcpServers = append(mcpServers, s)
	}
	sort.Strings(mcpServers)

	skills := append([]string(nil), sc.Skills...)
	sort.Strings(skills)

	unpricedList := make([]string, 0, len(unpriced))
	for m := range unpriced {
		unpricedList = append(unpricedList, m)
	}
	sort.Strings(unpricedList)

	toolCalls := 0
	for _, c := range sc.Tools {
		toolCalls += c
	}

	return &Summary{
		Start:            int64(fullTrajectory[0][0]),
		End:              int64(fullTrajectory[len(fullTrajectory)-1][0]),
		CostUSD:          round6(total),
		Requests:         len(reqs),
		Tokens:           tokens,
		Models:           models,
		SidechainCostUSD: round6(side),
		ToolCalls:        toolCalls,
		UserTurns:        sc.UserTurns,
		Skills:           skills,
		MCPServers:       mcpServers,
		Tools:            sc.Tools,
		Trajectory:       Downsample(fullTrajectory, TrajectoryMax),
		Unpriced:         unpricedList,
		Cwds:             sc.Cwds,
	}, nil
}

// round6 rounds to 6 decimal places, round-half-to-even. A naive
// `math.Round(f*1e6)/1e6` was tried first and rejected: multiplying by 1e6
// and dividing back are each their own lossy binary floating point step, so
// the result is wrong on a meaningful fraction of realistic cost values, not
// just on exact ties. strconv.FormatFloat's 'f' mode with a fixed precision
// performs a correctly-rounded decimal conversion, so formatting to 6
// decimal places and parsing the result back is the accurate way to do this,
// not a workaround.
func round6(f float64) float64 {
	s := strconv.FormatFloat(f, 'f', 6, 64)
	out, _ := strconv.ParseFloat(s, 64)
	return out
}

// MCPServerOf derives the MCP server name from an mcp__<server>__<tool>
// transcript tool name. Telemetry redacts MCP tool names to a bare
// "mcp_tool", so the transcript is the only place server attribution
// survives; the result is normalised through pricing.NormServer, the same
// fold applied to VictoriaLogs spellings like "plugin:linear:linear", so the
// two compare equal.
func MCPServerOf(tool string) (string, bool) {
	if !strings.HasPrefix(tool, "mcp__") {
		return "", false
	}
	parts := strings.Split(tool, "__")
	if len(parts) < 3 || parts[1] == "" {
		return "", false
	}
	return pricing.NormServer(parts[1]), true
}

// iso2epoch parses a transcript timestamp (Claude Code writes JS's
// Date.toISOString(), e.g. "2026-08-01T10:00:05.000Z") into Unix seconds.
// time.Time.Unix() truncates a positive time's fractional second toward
// zero, since its nanosecond field is always normalised into [0, 1e9).
func iso2epoch(ts string) (int64, error) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0, fmt.Errorf("transcript: bad timestamp %q: %w", ts, err)
	}
	return t.Unix(), nil
}

// Downsample thins points to at most n rows, replicating downsample's exact
// index formula: n evenly spaced positions across [0, len(points)-1], each
// rounded to the nearest integer index, deduplicated through a set and
// sorted. Collisions near either end of the range mean the result can come
// back shorter than n. This codebase's one caller passes n=300, making n-1
// (299) odd; an odd denominator can never produce a fractional part of
// exactly 0.5, so round(i*(len-1)/(n-1)) never hits an exact tie, and
// math.Round's away-from-zero tie-break never comes into play. A caller
// passing an even n-1 would be relying on that tie-break.
func Downsample(points [][3]float64, n int) [][3]float64 {
	if len(points) <= n {
		return points
	}
	last := float64(len(points) - 1)
	seen := make(map[int]bool, n)
	idx := make([]int, 0, n)
	for i := 0; i < n; i++ {
		v := int(math.Round(float64(i) * last / float64(n-1)))
		if !seen[v] {
			seen[v] = true
			idx = append(idx, v)
		}
	}
	sort.Ints(idx)
	out := make([][3]float64, len(idx))
	for k, i := range idx {
		out[k] = points[i]
	}
	return out
}
