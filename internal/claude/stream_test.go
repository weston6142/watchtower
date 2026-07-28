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
	text := "I need input.\n{\"watchtower_decision\": {\"question\": \"REST or GraphQL?\", \"options\": [\"REST\", \"GraphQL\"], \"recommended\": 0, \"importance\": 0.6, \"paths\": [\"api/routes.go\"]}}\n"
	d, ok := ExtractDecision(text)
	if !ok || d.Question != "REST or GraphQL?" || len(d.Options) != 2 || d.Importance != 0.6 || d.Paths[0] != "api/routes.go" {
		t.Fatalf("decision: %+v ok=%v", d, ok)
	}
	if _, ok := ExtractDecision("no marker here"); ok {
		t.Fatal("false positive")
	}
}

func TestExtractDecisionV2Fields(t *testing.T) {
	text := `{"watchtower_decision": {"question": "Q?", "options": ["a","b"], "recommended": 1, "importance": 0.5, "paths": [], "why": "b is safer", "consequences": ["fast but risky", "slower, safe"], "reversible": "until execute"}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Why != "b is safer" || len(d.Consequences) != 2 || d.Reversible != "until execute" {
		t.Fatalf("v2 fields: %+v ok=%v", d, ok)
	}
}

func TestExtractProposal(t *testing.T) {
	text := "found something\n{\"watchtower_proposal\": {\"title\": \"Refactor refunds\", \"body\": \"3 call sites entangled\"}}"
	p, ok := ExtractProposal(text)
	if !ok || p.Title != "Refactor refunds" || p.Body != "3 call sites entangled" {
		t.Fatalf("proposal: %+v ok=%v", p, ok)
	}
	if _, ok := ExtractProposal("nothing"); ok {
		t.Fatal("false positive")
	}
}

// The three cases below pin the legacy-key transition window: a session
// started before the rename still emits guildhall_* markers.

func TestExtractDecisionLegacyKey(t *testing.T) {
	text := `{"guildhall_decision": {"question": "Q?", "options": ["a","b"], "recommended": 1, "importance": 0.5, "paths": ["api/routes.go"], "why": "b is safer", "consequences": ["fast but risky", "slower, safe"], "reversible": "until execute"}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Question != "Q?" || len(d.Options) != 2 || d.Recommended != 1 ||
		d.Importance != 0.5 || d.Paths[0] != "api/routes.go" || d.Why != "b is safer" ||
		len(d.Consequences) != 2 || d.Reversible != "until execute" {
		t.Fatalf("legacy decision: %+v ok=%v", d, ok)
	}
}

func TestExtractProposalLegacyKey(t *testing.T) {
	text := "found something\n{\"guildhall_proposal\": {\"title\": \"Refactor refunds\", \"body\": \"3 call sites entangled\"}}"
	p, ok := ExtractProposal(text)
	if !ok || p.Title != "Refactor refunds" || p.Body != "3 call sites entangled" {
		t.Fatalf("legacy proposal: %+v ok=%v", p, ok)
	}
}

func TestExtractMarkerNewKeyWinsOverLegacy(t *testing.T) {
	text := `{"watchtower_decision": {"question": "new?", "options": ["a","b"]}, "guildhall_decision": {"question": "old?", "options": ["c","d"]}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Question != "new?" || d.Options[0] != "a" {
		t.Fatalf("precedence: %+v ok=%v", d, ok)
	}

	ptext := `{"watchtower_proposal": {"title": "new", "body": "n"}, "guildhall_proposal": {"title": "old", "body": "o"}}`
	p, ok := ExtractProposal(ptext)
	if !ok || p.Title != "new" || p.Body != "n" {
		t.Fatalf("proposal precedence: %+v ok=%v", p, ok)
	}
}

func TestUserMessage(t *testing.T) {
	line := string(UserMessage("go on"))
	if !strings.Contains(line, `"type":"user"`) || !strings.Contains(line, "go on") || !strings.HasSuffix(line, "\n") {
		t.Fatalf("bad user line: %q", line)
	}
}
