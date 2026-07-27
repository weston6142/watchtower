package claude

import (
	"strings"
	"testing"
)

func TestParseInitAssistantResult(t *testing.T) {
	init := ParseLine([]byte(`{"type":"system","subtype":"init","session_id":"s-123"}`))
	if init.Kind != "init" || init.SessionID != "s-123" {
		t.Fatalf("init: %+v", init)
	}
	at := ParseLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"},{"type":"text","text":"world"}]}}`))
	if at.Kind != "assistant_text" || at.Text != "hello\nworld" {
		t.Fatalf("assistant: %+v", at)
	}
	res := ParseLine([]byte(`{"type":"result","is_error":false,"usage":{"input_tokens":100,"output_tokens":50}}`))
	if res.Kind != "result" || res.Tokens != 150 || res.IsError {
		t.Fatalf("result: %+v", res)
	}
	if ParseLine([]byte(`garbage`)).Kind != "other" {
		t.Fatal("garbage should be other")
	}
}

func TestExtractDecision(t *testing.T) {
	text := "I need input.\n{\"guildhall_decision\": {\"question\": \"REST or GraphQL?\", \"options\": [\"REST\", \"GraphQL\"], \"recommended\": 0, \"importance\": 0.6, \"paths\": [\"api/routes.go\"]}}\n"
	d, ok := ExtractDecision(text)
	if !ok || d.Question != "REST or GraphQL?" || len(d.Options) != 2 || d.Importance != 0.6 || d.Paths[0] != "api/routes.go" {
		t.Fatalf("decision: %+v ok=%v", d, ok)
	}
	if _, ok := ExtractDecision("no marker here"); ok {
		t.Fatal("false positive")
	}
}

func TestExtractProposal(t *testing.T) {
	text := "found something\n{\"guildhall_proposal\": {\"title\": \"Refactor refunds\", \"body\": \"3 call sites entangled\"}}"
	p, ok := ExtractProposal(text)
	if !ok || p.Title != "Refactor refunds" || p.Body != "3 call sites entangled" {
		t.Fatalf("proposal: %+v ok=%v", p, ok)
	}
	if _, ok := ExtractProposal("nothing"); ok {
		t.Fatal("false positive")
	}
}

func TestUserMessage(t *testing.T) {
	line := string(UserMessage("go on"))
	if !strings.Contains(line, `"type":"user"`) || !strings.Contains(line, "go on") || !strings.HasSuffix(line, "\n") {
		t.Fatalf("bad user line: %q", line)
	}
}
