package claude

import (
	"encoding/json"
	"strings"

	"github.com/wbushyeager/guildhall/internal/levers"
)

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

func ParseLine(line []byte) StreamEvent {
	var r rawLine
	if err := json.Unmarshal(line, &r); err != nil {
		return StreamEvent{Kind: "other"}
	}
	switch {
	case r.Type == "system" && r.Subtype == "init":
		return StreamEvent{Kind: "init", SessionID: r.SessionID}
	case r.Type == "assistant" && r.Message != nil:
		var parts []string
		for _, c := range r.Message.Content {
			if c.Type == "text" && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		return StreamEvent{Kind: "assistant_text", Text: strings.Join(parts, "\n")}
	case r.Type == "result":
		u := r.Usage
		if u == nil && r.Message != nil {
			u = r.Message.Usage
		}
		tok := 0
		if u != nil {
			tok = u.InputTokens + u.OutputTokens
		}
		return StreamEvent{Kind: "result", Tokens: tok, IsError: r.IsError}
	default:
		return StreamEvent{Kind: "other"}
	}
}

type decisionMarker struct {
	D struct {
		Question    string   `json:"question"`
		Options     []string `json:"options"`
		Recommended int      `json:"recommended"`
		Importance  float64  `json:"importance"`
		Paths       []string `json:"paths"`
	} `json:"guildhall_decision"`
}

func ExtractDecision(text string) (levers.Decision, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `{"guildhall_decision":`) {
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
			Paths: m.D.Paths,
		}, true
	}
	return levers.Decision{}, false
}

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
