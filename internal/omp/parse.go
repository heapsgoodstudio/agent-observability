package omp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
)

// maxLineBytes bounds one JSONL record's size; omp session lines can run to
// several MB (a large assistant turn embeds its own content), but a file with
// no newline at all must not grow this without bound.
const maxLineBytes = 64 * 1024 * 1024

// toolNameRe accepts a bare tool name outright; toolNameLeadingRe recovers
// its leading run of allowed characters when it does not.
var (
	toolNameRe        = regexp.MustCompile(`^[\w.:-]+$`)
	toolNameLeadingRe = regexp.MustCompile(`^[\w.:-]+`)
)

// usageRecord is one billable request extracted from a single JSONL line,
// keyed for cross-file dedup (docs/omp-provider.md's "A request is counted
// once, across every file of the profile").
type usageRecord struct {
	Key                                             string
	Provider, Model                                 string
	Timestamp                                       int64 // unix seconds
	Input, Output, CacheRead, CacheWrite, Reasoning int64
	CostUSD                                         float64
	CostKnown                                       bool
}

// parsedFile is one JSONL file's extracted facts, cached by path+size+mtime.
// It holds only what Collect needs to build rows and buckets, never raw
// message content.
type parsedFile struct {
	HasHeader                        bool
	HeaderID                         string
	HeaderTS                         int64
	Cwd, Title, ParentSession        string
	FirstTS, LastTS                  int64
	Usage                            []usageRecord
	Tools                            map[string]int
	ToolCalls, UserTurns, ErrorCount int
	// Malformed is true when at least one complete line failed to parse as
	// JSON. A trailing line with no newline is never counted here: it is an
	// in-flight write, not a malformed record.
	Malformed bool
}

type rawContent struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type rawCost struct {
	Total json.RawMessage `json:"total"`
}

type rawUsage struct {
	Input           json.RawMessage `json:"input"`
	Output          json.RawMessage `json:"output"`
	CacheRead       json.RawMessage `json:"cacheRead"`
	CacheWrite      json.RawMessage `json:"cacheWrite"`
	ReasoningTokens json.RawMessage `json:"reasoningTokens"`
	Cost            *rawCost        `json:"cost"`
}

type rawMessage struct {
	Role       string          `json:"role"`
	Provider   json.RawMessage `json:"provider"`
	Model      json.RawMessage `json:"model"`
	Usage      *rawUsage       `json:"usage"`
	StopReason string          `json:"stopReason"`
	ResponseID string          `json:"responseId"`
	Timestamp  json.RawMessage `json:"timestamp"` // epoch ms, request-level
	Synthetic  *bool           `json:"synthetic"`
	Content    []rawContent    `json:"content"`
}

// rawEntry is one JSONL line. Fields not listed here (prompts, tool
// arguments/output, thinking text, credentials) are discarded by
// encoding/json rather than read.
type rawEntry struct {
	Type          string          `json:"type"`
	ID            string          `json:"id"`
	Timestamp     string          `json:"timestamp"` // ISO, entry-level
	Cwd           string          `json:"cwd"`
	Title         string          `json:"title"`
	ParentSession string          `json:"parentSession"`
	Message       *rawMessage     `json:"message"`
	Provider      json.RawMessage `json:"provider"` // model_usage only
	Model         json.RawMessage `json:"model"`    // model_usage only
	Usage         *rawUsage       `json:"usage"`    // model_usage only
}

// parseFile streams path's already-opened content line by line. It never
// returns an error for a malformed or oversized-but-abandoned line; those are
// recorded on the result instead so one damaged record cannot lose an entire
// session's usage. path is embedded in a record's dedup key when it has no
// responseId, so it must be the same absolute path used to discover the file.
func parseFile(path string, r io.Reader) (parsedFile, error) {
	pf := parsedFile{Tools: map[string]int{}}
	b := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := readLine(b)
		if err == io.EOF {
			// Whatever is left, complete JSON or not, is an in-flight write:
			// it is retried whole once the writer finishes it.
			break
		}
		if err != nil {
			return pf, fmt.Errorf("omp: read session file: %w", err)
		}
		line = bytes.TrimRight(line, "\n")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e rawEntry
		if json.Unmarshal(line, &e) != nil {
			pf.Malformed = true
			continue
		}
		if ts, ok := parseISO(e.Timestamp); ok {
			if pf.FirstTS == 0 {
				pf.FirstTS = ts
			}
			if ts > pf.LastTS {
				pf.LastTS = ts
			}
		}
		switch e.Type {
		case "session":
			if pf.HasHeader {
				continue
			}
			pf.HasHeader = true
			pf.HeaderID = e.ID
			if ts, ok := parseISO(e.Timestamp); ok {
				pf.HeaderTS = ts
			}
			pf.Cwd = e.Cwd
			pf.Title = e.Title
			pf.ParentSession = e.ParentSession
		case "message":
			if e.Message == nil {
				continue
			}
			pf.consumeMessage(path, e)
		case "model_usage":
			key := path + "#" + e.ID
			ts := requestTimestamp(e.Timestamp, nil, pf.HeaderTS)
			pf.addUsage(e.Provider, e.Model, e.Usage, key, ts)
		}
	}
	return pf, nil
}

func (pf *parsedFile) consumeMessage(path string, e rawEntry) {
	m := e.Message
	switch m.Role {
	case "user":
		if m.Synthetic == nil || !*m.Synthetic {
			pf.UserTurns++
		}
	case "assistant":
		if m.StopReason == "error" {
			pf.ErrorCount++
		}
		for _, block := range m.Content {
			if block.Type != "toolCall" {
				continue
			}
			name := sanitizeToolName(block.Name)
			if name == "" {
				continue
			}
			pf.Tools[name]++
			pf.ToolCalls++
		}
		if m.Usage != nil {
			key := m.ResponseID
			if key == "" {
				key = path + "#" + e.ID
			}
			ts := requestTimestamp(e.Timestamp, m.Timestamp, pf.HeaderTS)
			pf.addUsage(m.Provider, m.Model, m.Usage, key, ts)
		}
	}
}

// addUsage skips the record entirely, per docs/omp-provider.md, when either
// identifier is missing or not a JSON string.
func (pf *parsedFile) addUsage(providerRaw, modelRaw json.RawMessage, usage *rawUsage, key string, ts int64) {
	if usage == nil {
		return
	}
	provider, ok := rawString(providerRaw)
	if !ok {
		return
	}
	model, ok := rawString(modelRaw)
	if !ok {
		return
	}
	rec := usageRecord{
		Key:        key,
		Provider:   provider,
		Model:      model,
		Timestamp:  ts,
		Input:      numOrZero(usage.Input),
		Output:     numOrZero(usage.Output),
		CacheRead:  numOrZero(usage.CacheRead),
		CacheWrite: numOrZero(usage.CacheWrite),
		Reasoning:  numOrZero(usage.ReasoningTokens),
	}
	if usage.Cost != nil {
		if v, known := finiteNumber(usage.Cost.Total); known {
			rec.CostUSD, rec.CostKnown = v, true
		}
	}
	pf.Usage = append(pf.Usage, rec)
}

// sanitizeToolName trims a tool name and, when it holds characters outside
// [\w.:-], keeps only its leading run of allowed characters; a name with no
// such run at all reduces to "".
func sanitizeToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || toolNameRe.MatchString(name) {
		return name
	}
	return toolNameLeadingRe.FindString(name)
}

// requestTimestamp resolves one usage record's timestamp: the message's own
// epoch-ms stamp when present and positive, else the entry's ISO timestamp,
// else the file's header timestamp. msgTimestamp is nil for a model_usage
// record, which has no message object to carry one.
func requestTimestamp(entryISO string, msgTimestamp json.RawMessage, headerTS int64) int64 {
	if ms, ok := epochMillis(msgTimestamp); ok {
		return ms
	}
	if ts, ok := parseISO(entryISO); ok {
		return ts
	}
	return headerTS
}

func parseISO(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

func epochMillis(raw json.RawMessage) (int64, bool) {
	f, ok := finiteNumber(raw)
	if !ok || f <= 0 {
		return 0, false
	}
	return int64(math.Round(f / 1000.0)), true
}

// rawString reports s, true only when raw is present and decodes as a JSON
// string; any other shape (absent, null, number, object) is not a string.
func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// finiteNumber reports v, true only when raw is present, decodes as a JSON
// number, and is neither infinite nor NaN.
func finiteNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return f, true
}

// numOrZero reads a token field as a number, per docs/omp-provider.md
// treating anything non-numeric or non-finite as 0 rather than skipping the
// whole record.
func numOrZero(raw json.RawMessage) int64 {
	f, ok := finiteNumber(raw)
	if !ok {
		return 0
	}
	return int64(math.Round(f))
}

// readLine returns one complete line including its trailing "\n", or
// whatever remains with io.EOF when the file ends without one. It grows
// across ReadSlice's internal buffer boundary so a multi-MB record is never
// mistaken for one that overflowed a fixed buffer.
func readLine(b *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := b.ReadSlice('\n')
		if len(line)+len(chunk) > maxLineBytes {
			return nil, fmt.Errorf("omp: session record exceeds size limit")
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}
