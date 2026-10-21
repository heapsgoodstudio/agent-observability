package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"
)

// This is intentionally isolated from App Server's public types: rollout is a
// versioned local format, not a public API. Fields not listed here (including
// prompts, responses, arguments, outputs and credentials) are discarded.
type counts struct {
	Input     int64 `json:"input_tokens"`
	Cached    int64 `json:"cached_input_tokens"`
	Write     int64 `json:"cache_write_input_tokens"`
	Output    int64 `json:"output_tokens"`
	Reasoning int64 `json:"reasoning_output_tokens"`
}

func (c counts) empty() bool {
	return c.Input == 0 && c.Output == 0 && c.Cached == 0 && c.Write == 0 && c.Reasoning == 0
}
func (c counts) valid() bool {
	return c.Input >= 0 && c.Output >= 0 && c.Cached >= 0 && c.Write >= 0 && c.Reasoning >= 0 && c.Cached <= c.Input && c.Write <= c.Input-c.Cached && c.Reasoning <= c.Output
}
func (c counts) sub(p counts) counts {
	return counts{c.Input - p.Input, c.Cached - p.Cached, c.Write - p.Write, c.Output - p.Output, c.Reasoning - p.Reasoning}
}
func (c counts) key() string {
	// Older token_count mirrors omit cache-write counts. Their encompassing
	// input/output checkpoint still identifies the same newer usage record.
	return fmt.Sprintf("%d/%d/%d/%d", c.Input, c.Cached, c.Output, c.Reasoning)
}

type request struct {
	Key, ThreadID, TurnID, Model string
	Timestamp                    int64
	Usage                        counts
	Modern                       bool
}
type parsed struct {
	ID, ParentID, ForkedFromID, Cwd, Model, HistoryMode, Version string
	Start, End                                                   int64
	Requests                                                     []request
	Tools                                                        map[string]int
	UserTurns, Errors                                            int
	DurationMS                                                   int64
	UsageKnown, ToolsKnown, ErrorsKnown, DurationKnown           bool
	UsageUnavailableReason                                       string
	RejectedOnly                                                 bool
	Warnings                                                     []string
}
type rawPayload struct {
	Type, ID, Name, Role, Model, Cwd string
	ThreadID                         string  `json:"thread_id"`
	ParentID                         string  `json:"parent_thread_id"`
	ForkedFromID                     string  `json:"forked_from_id"`
	TurnID                           string  `json:"turn_id"`
	ResponseID                       string  `json:"response_id"`
	CallID                           string  `json:"call_id"`
	HistoryMode                      string  `json:"history_mode"`
	Version                          string  `json:"cli_version"`
	Timestamp                        string  `json:"timestamp"`
	Usage                            *counts `json:"usage"`
	Total                            *counts `json:"thread_token_usage"`
	Info                             *struct {
		Total *counts `json:"total_token_usage"`
		Last  *counts `json:"last_token_usage"`
	} `json:"info"`
	DurationMS       *int64          `json:"duration_ms"`
	Item             *historyItem    `json:"item"`
	HistoryStart     *int64          `json:"subagent_history_start_ordinal"`
	Source           json.RawMessage `json:"source"`
	Error            *rolloutError   `json:"error"`
	LastAgentMessage string          `json:"last_agent_message"`
}
type rawRecord struct {
	Type      string     `json:"type"`
	Timestamp string     `json:"timestamp"`
	Payload   rawPayload `json:"payload"`
}

// parseRollout consumes complete lines only. A final partial write is retried on
// the next scan; malformed complete records mark coverage partial. Checkpoints
// deduplicate legacy token_count repeats. Modern and legacy cumulative ledgers
// remain separate: compaction makes their totals diverge. Only explicit
// per-response usage is summed; adjacent legacy/modern mirrors are reconciled
// by that usage and turn identity, regardless of which appears first.
func parseRollout(ctx context.Context, r io.Reader) (parsed, error) {
	out := parsed{Tools: map[string]int{}, HistoryMode: "legacy"}
	b := bufio.NewReaderSize(r, 64*1024)
	var legacyPrevious counts
	haveLegacy := false
	legacyCheckpoints := map[string]bool{}
	pendingModern, pendingLegacy := -1, -1
	var pendingForeign *counts
	var foreignTurn string
	responseIndex := map[string]int{}
	toolIDs := map[string]bool{}
	turns := map[string]bool{}
	durations := map[string]bool{}
	var activeTurn string
	userEvents, userResponses := 0, 0
	var historyStart int64
	var rejection rejectionEvidence
	for lineNumber := 1; ; lineNumber++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		line, err := readCompleteLine(ctx, b)
		if err == io.EOF {
			if len(line) > 0 {
				rejection.unsafe = true
			}
			break
		} // even valid JSON without newline may be in flight
		if err != nil {
			return out, fmt.Errorf("codex: read rollout: %w", err)
		}
		var rec rawRecord
		if json.Unmarshal(line, &rec) != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("Malformed rollout record at line %d", lineNumber))
			continue
		}
		p := rec.Payload
		rejection.observe(rec)
		if int64(lineNumber-1) < historyStart && rec.Type != "session_meta" {
			// Inherited records establish only the legacy checkpoint. Modern
			// thread totals have a different scope after compaction.
			if p.Info != nil && p.Info.Total != nil && p.Info.Total.valid() {
				legacyPrevious = *p.Info.Total
				haveLegacy = true
			}
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, rec.Timestamp)
		stamp := int64(0)
		if !ts.IsZero() {
			stamp = ts.Unix()
			if out.Start == 0 {
				out.Start = stamp
			}
			if stamp > out.End {
				out.End = stamp
			}
		}
		switch rec.Type {
		case "session_meta":
			// Forks/subagents embed parent metadata after their own header.
			// Only the first session_meta owns this file's identity.
			if out.ID != "" {
				continue
			}
			if p.ID != "" {
				out.ID = p.ID
			}
			if p.Cwd != "" {
				out.Cwd = p.Cwd
			}
			out.ParentID = p.ParentID
			if out.ParentID == "" {
				var source struct {
					Subagent struct {
						ThreadSpawn struct {
							ParentID string `json:"parent_thread_id"`
						} `json:"thread_spawn"`
					} `json:"subagent"`
				}
				if json.Unmarshal(p.Source, &source) == nil {
					out.ParentID = source.Subagent.ThreadSpawn.ParentID
				}
			}
			out.ForkedFromID = p.ForkedFromID
			if p.HistoryMode != "" {
				out.HistoryMode = p.HistoryMode
			}
			out.Version = p.Version
			if p.HistoryStart != nil {
				historyStart = *p.HistoryStart
			}
		case "turn_context":
			if p.Model != "" {
				out.Model = p.Model
			}
			if p.Cwd != "" {
				out.Cwd = p.Cwd
			}
			activeTurn = p.TurnID
		case "response_item":
			if p.Type == "message" && p.Role == "user" {
				userResponses++
			}
			if p.Type == "function_call" || p.Type == "custom_tool_call" || p.Type == "tool_search_call" || p.Type == "web_search_call" {
				name := p.Name
				if name == "" {
					name = p.Type
				}
				id := p.CallID
				if id == "" {
					id = p.ID
				}
				if id == "" {
					id = "line:" + strconv.Itoa(lineNumber)
				}
				if !toolIDs[id] {
					out.Tools[name]++
					toolIDs[id] = true
				}
				out.ToolsKnown = true
			}
		case "event_msg":
			switch p.Type {
			case "user_message":
				userEvents++
			case "task_started":
				activeTurn = p.TurnID
				turns[p.TurnID] = true
			case "task_complete":
				if p.DurationMS != nil && *p.DurationMS >= 0 && !durations[p.TurnID] {
					out.DurationMS += *p.DurationMS
					out.DurationKnown = true
					durations[p.TurnID] = true
				}
				out.ErrorsKnown = true
			case "error":
				out.Errors++
				out.ErrorsKnown = true
			}
		}
		modern := rec.Type == "token_usage_record"
		legacy := rec.Type == "event_msg" && p.Type == "token_count" && p.Info != nil
		if !modern && !legacy {
			continue
		}
		turnID := activeTurn
		if p.TurnID != "" {
			turnID = p.TurnID
		}
		model := out.Model
		if p.Model != "" {
			model = p.Model
		}
		if modern {
			// usage is per-response. thread_token_usage is a lifetime ledger;
			// it must NEVER share a baseline with legacy total_token_usage,
			// whose compaction accounting differs in Codex 0.155.1.
			if p.Usage == nil || !p.Usage.valid() {
				out.Warnings = append(out.Warnings, "Modern response usage is missing or invalid")
				continue
			}
			if p.ThreadID != "" && out.ID != "" && p.ThreadID != out.ID {
				u := *p.Usage
				pendingForeign = &u
				foreignTurn = turnID
				pendingModern, pendingLegacy = -1, -1
				continue
			}
			out.UsageKnown = true
			if p.Usage.empty() {
				continue
			}
			req := request{Key: "response:" + p.ResponseID, ThreadID: out.ID, TurnID: turnID, Model: model, Timestamp: stamp, Usage: *p.Usage, Modern: true}
			if p.ResponseID == "" {
				req.Key = "modern:" + rec.Timestamp + ":" + turnID + ":" + p.Usage.key()
				out.Warnings = append(out.Warnings, "A modern usage record lacks a response ID")
			}
			index, duplicate := responseIndex[req.Key]
			if duplicate {
				out.Requests[index] = req
				if pendingLegacy >= 0 && sameResponse(out.Requests[pendingLegacy], req) {
					out.Requests[pendingLegacy] = request{}
				}
			} else if pendingLegacy >= 0 && sameResponse(out.Requests[pendingLegacy], req) {
				index = pendingLegacy
				out.Requests[index] = req
			} else {
				index = len(out.Requests)
				out.Requests = append(out.Requests, req)
			}
			responseIndex[req.Key] = index
			pendingModern, pendingLegacy = index, -1
			pendingForeign = nil
			continue
		}

		last, total := p.Info.Last, p.Info.Total
		if last == nil || !last.valid() {
			out.Warnings = append(out.Warnings, "Legacy response usage is missing or invalid; cumulative totals were not imported")
			continue
		}
		out.UsageKnown = true
		if last.empty() {
			continue
		}
		req := request{Key: rec.Timestamp + ":" + last.key(), ThreadID: out.ID, TurnID: turnID, Model: model, Timestamp: stamp, Usage: *last}
		var difference counts
		hadPrevious := haveLegacy
		if total != nil && total.valid() {
			req.Key = rec.Timestamp + ":" + total.key()
			checkpoint := turnID + ":" + total.key()
			if haveLegacy && total.key() == legacyPrevious.key() {
				continue
			} // quota-only repeat
			if legacyCheckpoints[checkpoint] {
				out.Warnings = append(out.Warnings, "A legacy cumulative checkpoint was replayed; ambiguous duplicate skipped")
				continue
			}
			legacyCheckpoints[checkpoint] = true
			difference = total.sub(legacyPrevious)
			legacyPrevious = *total
			haveLegacy = true
		} else {
			out.Warnings = append(out.Warnings, "Legacy usage has no valid cumulative checkpoint; response deduplication is partial")
		}
		if pendingForeign != nil && usageMatches(*pendingForeign, *last) && compatibleTurn(foreignTurn, turnID) {
			pendingForeign = nil
			pendingLegacy = -1
			continue
		}
		if pendingModern >= 0 && sameResponse(out.Requests[pendingModern], req) {
			pendingModern, pendingLegacy = -1, -1
			continue
		}
		if total != nil && total.valid() && hadPrevious {
			// A subset correction is not evidence of another response. More
			// generally, disagreement never turns a lifetime total into usage.
			if difference.Input == 0 && difference.Output == 0 {
				out.Warnings = append(out.Warnings, "Legacy token subsets changed without new input/output; correction skipped")
				continue
			}
			if difference.Input >= 0 && difference.Output >= 0 && !usageMatches(difference, *last) {
				out.Warnings = append(out.Warnings, "Legacy cumulative and per-response usage disagree; only explicit response usage imported")
			}
		} else if total != nil && total.valid() && !hadPrevious && !usageMatches(*total, *last) {
			out.Warnings = append(out.Warnings, "Legacy history starts with a cumulative snapshot; only explicit response usage imported")
		}
		pendingLegacy = len(out.Requests)
		pendingModern = -1
		pendingForeign = nil
		out.Requests = append(out.Requests, req)

	}
	if userEvents > 0 {
		out.UserTurns = userEvents
	} else {
		out.UserTurns = userResponses
	}
	if out.UserTurns == 0 {
		out.UserTurns = len(turns)
	}
	kept := out.Requests[:0]
	for _, req := range out.Requests {
		if req.Key != "" {
			kept = append(kept, req)
		}
	}
	out.Requests = kept
	if !out.UsageKnown {
		if rejection.confirmsZero(out) {
			out.RejectedOnly = true
			out.UsageKnown = true
			out.ToolsKnown = true
			out.Errors = len(rejection.rejected)
		} else if rejection.interrupted {
			out.UsageUnavailableReason = "interrupted"
		} else {
			out.UsageUnavailableReason = "missing_records"
		}
	}
	return out, nil
}

// Different releases omit cache-write/reasoning subsets from legacy mirrors.
// Encompassing input/output and cached input identify the adjacent response;
// the modern record preserves its richer subset detail.
func usageMatches(a, b counts) bool {
	return a.Input == b.Input && a.Cached == b.Cached && a.Output == b.Output
}
func compatibleTurn(a, b string) bool { return a == "" || b == "" || a == b }
func sameResponse(a, b request) bool {
	return a.Key != "" && usageMatches(a.Usage, b.Usage) && compatibleTurn(a.TurnID, b.TurnID)
}

// ReadSlice bounds allocation even when a damaged file contains no newline.
func readCompleteLine(ctx context.Context, b *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk, err := b.ReadSlice('\n')
		if len(line)+len(chunk) > 64*1024*1024 {
			return nil, fmt.Errorf("codex: rollout record exceeds size limit")
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}
