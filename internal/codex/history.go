package codex

import (
	"context"
	"fmt"
)

// Deliberately omit thread previews, titles, message bodies and tool arguments.
type thread struct {
	ID, Cwd, Path, Model string
	ParentID             string `json:"parentThreadId"`
	ForkedFromID         string `json:"forkedFromId"`
	HistoryMode          string `json:"historyMode"`
	Version              string `json:"cliVersion"`
	CreatedAt            int64  `json:"createdAt"`
	UpdatedAt            int64  `json:"updatedAt"`
	Turns                []turn `json:"turns"`
}
type turn struct {
	ID, Status string
	DurationMS *int64        `json:"durationMs"`
	Items      []historyItem `json:"items"`
}
type historyItem struct {
	ID, Type, Status, Tool, Server, Namespace string
}
type historySummary struct {
	Tools             map[string]int
	UserTurns, Errors int
	DurationMS        int64
	DurationKnown     bool
	Turns             int
	ModelActivity     bool
}

var allSources = []string{"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown"}

func listThreads(ctx context.Context, c RPC, limit int) ([]thread, error) {
	var out []thread
	seenIDs := map[string]bool{}
	for _, archived := range []bool{false, true} {
		cursor := ""
		seenCursors := map[string]bool{}
		for {
			var page struct {
				Data []thread `json:"data"`
				Next string   `json:"nextCursor"`
			}
			params := map[string]any{"limit": 100, "sourceKinds": allSources, "modelProviders": []string{}, "archived": archived, "useStateDbOnly": true, "sortKey": "updated_at"}
			if cursor != "" {
				params["cursor"] = cursor
			}
			if err := c.Call(ctx, "thread/list", params, &page); err != nil {
				return out, err
			}
			for _, t := range page.Data {
				if t.ID == "" || seenIDs[t.ID] {
					continue
				}
				seenIDs[t.ID] = true
				if len(out) >= limit {
					return out, fmt.Errorf("codex: thread collection limit reached")
				}
				out = append(out, t)
			}
			if page.Next == "" {
				break
			}
			if seenCursors[page.Next] {
				return out, fmt.Errorf("codex: repeated thread pagination cursor")
			}
			seenCursors[page.Next] = true
			cursor = page.Next
		}
	}
	return out, nil
}

func readHistory(ctx context.Context, c RPC, t thread, caps *Capabilities) (historySummary, error) {
	h := historySummary{Tools: map[string]int{}}
	seenItems, seenTurns := map[string]bool{}, map[string]bool{}
	consume := func(turns []turn) {
		for _, t := range turns {
			if seenTurns[t.ID] {
				continue
			}
			seenTurns[t.ID] = true
			h.Turns++
			if t.Status == "failed" {
				h.Errors++
			}
			if t.DurationMS != nil && *t.DurationMS >= 0 {
				h.DurationMS += *t.DurationMS
				h.DurationKnown = true
			}
			for _, i := range t.Items {
				key := t.ID + ":" + i.ID
				if seenItems[key] {
					continue
				}
				seenItems[key] = true
				if i.Type == "userMessage" {
					h.UserTurns++
					continue
				}
				h.ModelActivity = true
				name := ""
				switch i.Type {
				case "commandExecution":
					name = "shell"
				case "fileChange":
					name = "apply_patch"
				case "mcpToolCall":
					name = "mcp__" + i.Server + "__" + i.Tool
				case "dynamicToolCall":
					name = i.Tool
					if i.Namespace != "" {
						name = i.Namespace + "." + name
					}
				case "collabAgentToolCall":
					name = i.Tool
				case "webSearch", "imageView", "imageGeneration":
					name = i.Type
				}
				if name != "" {
					h.Tools[name]++
					if i.Status == "failed" {
						h.Errors++
					}
				}
			}
		}
	}
	// Probe the storage-specific item capability; full turn pages remain the
	// authority for tool totals so returned item content is never counted twice.
	if t.HistoryMode == "paginated" && !caps.ItemsList {
		var probe struct {
			Data []struct{} `json:"data"`
		}
		if err := c.Call(ctx, "thread/items/list", map[string]any{"threadId": t.ID, "limit": 1}, &probe); err == nil {
			caps.ItemsList = true
		} else if !unsupported(err) {
			return h, err
		}
	}
	cursor := ""
	seenCursors := map[string]bool{}
	for pageCount := 0; pageCount < 10000; pageCount++ {
		var page struct {
			Data []turn `json:"data"`
			Next string `json:"nextCursor"`
		}
		params := map[string]any{"threadId": t.ID, "limit": 100, "sortDirection": "asc", "itemsView": "full"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		err := c.Call(ctx, "thread/turns/list", params, &page)
		if err != nil {
			if cursor != "" || !unsupported(err) {
				return h, err
			}
			var full struct {
				Thread thread `json:"thread"`
			}
			if err := c.Call(ctx, "thread/read", map[string]any{"threadId": t.ID, "includeTurns": true}, &full); err != nil {
				return h, err
			}
			caps.ThreadRead = true
			consume(full.Thread.Turns)
			return h, nil
		}
		caps.TurnsList = true
		consume(page.Data)
		if page.Next == "" {
			return h, nil
		}
		if seenCursors[page.Next] {
			return h, fmt.Errorf("codex: repeated turn pagination cursor")
		}
		seenCursors[page.Next] = true
		cursor = page.Next
	}
	return h, fmt.Errorf("codex: turn collection limit reached")
}
