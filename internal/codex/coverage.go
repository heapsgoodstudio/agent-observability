package codex

import (
	"encoding/json"
	"strings"
)

// A missing token record is not a zero. The narrow exception is an intact
// rollout containing only requests explicitly rejected for an unsupported
// model, before any model output or tools. Interruptions never prove zero.
type rejectionEvidence struct {
	started, rejected           map[string]bool
	header, unsafe, interrupted bool
}

func (e *rejectionEvidence) observe(r rawRecord) {
	p := r.Payload
	if e.started == nil {
		e.started, e.rejected = map[string]bool{}, map[string]bool{}
	}
	if !e.header && r.Type != "session_meta" {
		e.unsafe = true
	}
	switch r.Type {
	case "session_meta":
		if e.header || p.ID == "" {
			e.unsafe = true
		}
		e.header = true
	case "turn_context":
		if !e.started[p.TurnID] || e.rejected[p.TurnID] {
			e.unsafe = true
		}
	case "world_state":
	case "response_item":
		if p.Type != "message" || (p.Role != "user" && p.Role != "developer" && p.Role != "system") {
			e.unsafe = true
		}
	case "event_msg":
		switch p.Type {
		case "task_started":
			if p.TurnID == "" || e.started[p.TurnID] {
				e.unsafe = true
			}
			e.started[p.TurnID] = true
		case "task_complete":
			if !e.started[p.TurnID] || e.rejected[p.TurnID] || p.LastAgentMessage != "" || !unsupportedModelRejection(p.Error) {
				e.unsafe = true
			} else {
				e.rejected[p.TurnID] = true
			}
		case "turn_aborted":
			e.interrupted, e.unsafe = true, true
		case "user_message", "thread_settings_applied":
		case "item_completed":
			if p.Item == nil || p.Item.Type != "UserMessage" {
				e.unsafe = true
			}
		case "token_count":
			if p.Info != nil {
				e.unsafe = true
			}
		default:
			e.unsafe = true
		}
	default:
		e.unsafe = true
	}
}

func (e rejectionEvidence) confirmsZero(p parsed) bool {
	return e.header && !e.unsafe && len(e.started) > 0 && len(e.started) == len(e.rejected) &&
		p.ParentID == "" && p.ForkedFromID == "" && len(p.Warnings) == 0
}

type rolloutError struct {
	Message string `json:"message"`
}

func unsupportedModelRejection(err *rolloutError) bool {
	if err == nil {
		return false
	}
	var response struct {
		Type   string `json:"type"`
		Status int    `json:"status"`
		Error  struct {
			Type, Message string
		} `json:"error"`
	}
	if json.Unmarshal([]byte(err.Message), &response) != nil {
		return false
	}
	return response.Type == "error" && response.Status == 400 && response.Error.Type == "invalid_request_error" &&
		strings.HasPrefix(response.Error.Message, "The '") &&
		strings.HasSuffix(response.Error.Message, "' model is not supported when using Codex with a ChatGPT account.")
}
