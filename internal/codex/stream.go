package codex

import (
	"encoding/json"
	"strings"

	"github.com/weston6142/watchtower/internal/runner"
)

const (
	KindThread      = "thread"
	KindText        = "text"
	KindTool        = "tool"
	KindToolRequest = "tool_request"
	KindComplete    = "complete"
	KindFailed      = "failed"
	KindOther       = "other"
	maxToolRunes    = 120
)

// Event is the provider-neutral subset of one Codex JSONL event needed by the
// runner and operator transcript.
type Event struct {
	Kind         string
	ThreadID     string
	Text         string
	Tool         string
	ToolCall     runner.ToolCall
	Tokens       int
	TokensKnown  bool
	Error        string
	FailureClass runner.FailureClass
}

type rawEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message  string `json:"message"`
		Code     string `json:"code"`
		Category string `json:"category"`
	} `json:"error"`
	Usage *struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	} `json:"usage"`
	Item *struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Command string `json:"command"`
		Server  string `json:"server"`
		Tool    string `json:"tool"`
		Query   string `json:"query"`
		Changes []struct {
			Path string `json:"path"`
		} `json:"changes"`
	} `json:"item"`
}

// ParseLine normalizes one line from `codex exec --json`. Malformed,
// unrecognized, and progress events are intentionally ignored.
func ParseLine(line []byte) Event {
	var raw rawEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{Kind: KindOther}
	}
	switch raw.Type {
	case "thread.started":
		return Event{Kind: KindThread, ThreadID: raw.ThreadID}
	case "item.completed":
		if raw.Item == nil {
			return Event{Kind: KindOther}
		}
		switch raw.Item.Type {
		case "agent_message":
			if raw.Item.Text != "" {
				return Event{Kind: KindText, Text: raw.Item.Text}
			}
		case "command_execution":
			return toolEvent("command " + raw.Item.Command)
		case "file_change":
			paths := make([]string, 0, len(raw.Item.Changes))
			for _, change := range raw.Item.Changes {
				if path := normalize(change.Path); path != "" {
					paths = append(paths, path)
				}
			}
			return toolEvent("files " + strings.Join(paths, ", "))
		case "mcp_tool_call":
			return toolEvent("mcp " + raw.Item.Server + "/" + raw.Item.Tool)
		case "web_search":
			return toolEvent("web " + raw.Item.Query)
		}
	case "item.started", "item.created":
		if raw.Item == nil {
			return Event{Kind: KindOther}
		}
		paths := make([]string, 0, len(raw.Item.Changes))
		for _, change := range raw.Item.Changes {
			paths = append(paths, change.Path)
		}
		if call, ok := requestToolCall(raw.Item.Type, raw.Item.Command, raw.Item.Server, raw.Item.Tool, raw.Item.Query, paths); ok {
			return Event{Kind: KindToolRequest, ToolCall: call}
		}
	case "turn.completed":
		tokens := 0
		known := raw.Usage != nil
		if raw.Usage != nil {
			tokens = raw.Usage.Input + raw.Usage.Output
		}
		return Event{Kind: KindComplete, Tokens: tokens, TokensKnown: known}
	case "turn.failed", "error":
		message := ""
		classification := ""
		if raw.Error != nil {
			message = raw.Error.Message
			classification = raw.Error.Code + " " + raw.Error.Category
		}
		if message == "" {
			message = raw.Message
		}
		if message == "" {
			message = "codex turn failed"
		}
		return Event{Kind: KindFailed, Error: message, FailureClass: classifyFailureMessage(classification + " " + message)}
	}
	return Event{Kind: KindOther}
}

func requestToolCall(kind, command, server, tool, query string, changes []string) (runner.ToolCall, bool) {
	source := ""
	fingerprint := ""
	switch kind {
	case "command_execution":
		fields := strings.Fields(command)
		if len(fields) > 0 {
			source = fields[len(fields)-1]
		}
		fingerprint = normalize(command)
	case "file_change":
		if len(changes) > 0 {
			source = normalize(changes[0])
		}
		fingerprint = strings.Join([]string{kind, source}, ":")
	case "mcp_tool_call":
		source = normalize(server + "/" + tool)
		fingerprint = source
	case "web_search":
		source = normalize(query)
		fingerprint = source
	default:
		return runner.ToolCall{}, false
	}
	if source == "" {
		return runner.ToolCall{}, false
	}
	return runner.ToolCall{Name: kind, SourceID: source, Fingerprint: fingerprint, Reservation: 1}, true
}

func toolEvent(summary string) Event {
	line := "↳ " + normalize(summary)
	if runes := []rune(line); len(runes) > maxToolRunes {
		line = string(runes[:maxToolRunes-1]) + "…"
	}
	return Event{Kind: KindTool, Tool: line}
}

func normalize(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
