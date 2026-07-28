package claude

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/runner"
)

// Kinds of StreamEvent produced by ParseLine.
const (
	KindInit          = "init"
	KindAssistantText = "assistant_text"
	KindToolUse       = "tool_use"
	KindResult        = "result"
	KindOther         = "other"
)

// StreamEvent is a simplified view of one stream-json line from the claude CLI.
type StreamEvent struct {
	Kind      string
	SessionID string
	Text      string
	// Tools holds one-line summaries of the tool_use blocks in this message,
	// already prefixed. They are what an operator watching a stage sees while
	// the agent is working rather than talking.
	Tools   []string
	Tokens  int
	IsError bool
}

type rawLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
	Usage     *usage `json:"usage"`
	Message   *struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage *usage `json:"usage"`
	} `json:"message"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ParseLine parses one stream-json line. Unrecognized or malformed lines
// yield a KindOther event rather than an error.
func ParseLine(line []byte) StreamEvent {
	var r rawLine
	if err := json.Unmarshal(line, &r); err != nil {
		return StreamEvent{Kind: KindOther}
	}
	switch {
	case r.Type == "system" && r.Subtype == "init":
		return StreamEvent{Kind: KindInit, SessionID: r.SessionID}
	case r.Type == "assistant" && r.Message != nil:
		var parts []string
		var tools []string
		for _, c := range r.Message.Content {
			switch {
			case c.Type == "text" && c.Text != "":
				parts = append(parts, c.Text)
			case c.Type == "tool_use" && c.Name != "":
				tools = append(tools, toolLinePrefix+toolSummary(c.Name, c.Input))
			}
		}
		text := strings.Join(parts, "\n")
		// A message with nothing but tool calls used to fall through as an
		// empty assistant_text, writing a blank line per tool call.
		if text == "" && len(tools) > 0 {
			return StreamEvent{Kind: KindToolUse, Tools: tools}
		}
		return StreamEvent{Kind: KindAssistantText, Text: text, Tools: tools}
	case r.Type == "result":
		u := r.Usage
		if u == nil && r.Message != nil {
			u = r.Message.Usage
		}
		tok := 0
		if u != nil {
			tok = u.InputTokens + u.OutputTokens
		}
		return StreamEvent{Kind: KindResult, Tokens: tok, IsError: r.IsError}
	default:
		return StreamEvent{Kind: KindOther}
	}
}

const (
	// toolLinePrefix marks a tool call apart from prose in the stream door.
	toolLinePrefix = "↳ "
	// maxToolLineRunes bounds the whole emitted line, prefix included.
	maxToolLineRunes = 120
)

// toolSummary renders one tool_use block as a single transcript line: the tool
// name plus its most identifying argument. Unrecognized tools and unparseable
// inputs degrade to the bare name — raw JSON in the stream door is noise.
func toolSummary(name string, input json.RawMessage) string {
	maxRunes := maxToolLineRunes - utf8.RuneCountInString(toolLinePrefix)
	var fields map[string]json.RawMessage
	if len(input) == 0 || json.Unmarshal(input, &fields) != nil {
		return name
	}
	var arg string
	for _, key := range []string{"command", "file_path", "path", "pattern", "description", "query"} {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		if json.Unmarshal(raw, &arg) == nil && arg != "" {
			break
		}
		arg = ""
	}
	if arg == "" {
		return name
	}
	arg = strings.Join(strings.Fields(arg), " ")
	out := name + " " + arg
	if runes := []rune(out); len(runes) > maxRunes {
		out = string(runes[:maxRunes-1]) + "…"
	}
	return out
}

type decisionPayload struct {
	Question     string   `json:"question"`
	Options      []string `json:"options"`
	Recommended  int      `json:"recommended"`
	Importance   float64  `json:"importance"`
	Paths        []string `json:"paths"`
	Why          string   `json:"why"`
	Consequences []string `json:"consequences"`
	Reversible   string   `json:"reversible"`
}

// Legacy accepts the pre-rename guildhall_* marker key. Removable once no
// in-flight agent session predates the rename — sessions carry the marker
// spelling in their system prompt, so a session started before the rebuild
// still emits the old key.
type decisionMarker struct {
	D      decisionPayload `json:"watchtower_decision"`
	Legacy decisionPayload `json:"guildhall_decision"`
}

// ExtractDecision scans assistant text for a watchtower_decision marker line
// and returns the parsed decision if a valid one is found.
func ExtractDecision(text string) (levers.Decision, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"watchtower_decision":`) &&
			!strings.HasPrefix(line, `{"guildhall_decision":`) {
			continue
		}
		var m decisionMarker
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		d := m.D
		if d.Question == "" { // new key absent or empty — fall back to legacy
			d = m.Legacy
		}
		if d.Question == "" || len(d.Options) == 0 {
			continue
		}
		return levers.Decision{
			Question: d.Question, Options: d.Options,
			Recommended: d.Recommended, Importance: d.Importance,
			Paths: d.Paths, Why: d.Why,
			Consequences: d.Consequences, Reversible: d.Reversible,
		}, true
	}
	return levers.Decision{}, false
}

type proposalPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Legacy accepts the pre-rename guildhall_* marker key; see decisionMarker.
type proposalMarker struct {
	P      proposalPayload `json:"watchtower_proposal"`
	Legacy proposalPayload `json:"guildhall_proposal"`
}

// ExtractProposal scans assistant text for the watchtower_proposal marker.
func ExtractProposal(text string) (runner.Proposal, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"watchtower_proposal":`) &&
			!strings.HasPrefix(line, `{"guildhall_proposal":`) {
			continue
		}
		var m proposalMarker
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		p := m.P
		if p.Title == "" { // new key absent or empty — fall back to legacy
			p = m.Legacy
		}
		if p.Title == "" {
			continue
		}
		return runner.Proposal{Title: p.Title, Body: p.Body}, true
	}
	return runner.Proposal{}, false
}

// UserMessage encodes text as a stream-json user message line (newline-terminated),
// ready to write to the claude CLI's stdin.
func UserMessage(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": text}},
		},
	})
	return append(b, '\n')
}
