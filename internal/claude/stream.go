package claude

import (
	"encoding/json"
	"strings"

	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/runner"
)

// Kinds of StreamEvent produced by ParseLine.
const (
	KindInit          = "init"
	KindAssistantText = "assistant_text"
	KindResult        = "result"
	KindOther         = "other"
)

// StreamEvent is a simplified view of one stream-json line from the claude CLI.
type StreamEvent struct {
	Kind      string
	SessionID string
	Text      string
	Tokens    int
	IsError   bool
}

type rawLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
	Usage     *usage `json:"usage"`
	Message   *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
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
		for _, c := range r.Message.Content {
			if c.Type == "text" && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		return StreamEvent{Kind: KindAssistantText, Text: strings.Join(parts, "\n")}
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

type decisionMarker struct {
	D struct {
		Question     string   `json:"question"`
		Options      []string `json:"options"`
		Recommended  int      `json:"recommended"`
		Importance   float64  `json:"importance"`
		Paths        []string `json:"paths"`
		Why          string   `json:"why"`
		Consequences []string `json:"consequences"`
		Reversible   string   `json:"reversible"`
	} `json:"watchtower_decision"`
}

// ExtractDecision scans assistant text for a watchtower_decision marker line
// and returns the parsed decision if a valid one is found.
func ExtractDecision(text string) (levers.Decision, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"watchtower_decision":`) {
			continue
		}
		var m decisionMarker
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m.D.Question == "" || len(m.D.Options) == 0 {
			continue
		}
		return levers.Decision{
			Question: m.D.Question, Options: m.D.Options,
			Recommended: m.D.Recommended, Importance: m.D.Importance,
			Paths: m.D.Paths, Why: m.D.Why,
			Consequences: m.D.Consequences, Reversible: m.D.Reversible,
		}, true
	}
	return levers.Decision{}, false
}

type proposalMarker struct {
	P struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	} `json:"watchtower_proposal"`
}

// ExtractProposal scans assistant text for the watchtower_proposal marker.
func ExtractProposal(text string) (runner.Proposal, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"watchtower_proposal":`) {
			continue
		}
		var m proposalMarker
		if err := json.Unmarshal([]byte(line), &m); err != nil || m.P.Title == "" {
			continue
		}
		return runner.Proposal{Title: m.P.Title, Body: m.P.Body}, true
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
