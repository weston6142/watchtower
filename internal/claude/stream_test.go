package claude

import (
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/levers"
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

func TestParseToolUse(t *testing.T) {
	ev := ParseLine([]byte(`{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}},` +
		`{"type":"tool_use","name":"Read","input":{"file_path":"internal/engine/engine.go"}}]}}`))
	if ev.Kind != KindToolUse {
		t.Fatalf("kind = %q, want tool_use", ev.Kind)
	}
	if len(ev.Tools) != 2 {
		t.Fatalf("tools = %v", ev.Tools)
	}
	if ev.Tools[0] != "↳ Bash go test ./..." {
		t.Fatalf("tools[0] = %q", ev.Tools[0])
	}
	if ev.Tools[1] != "↳ Read internal/engine/engine.go" {
		t.Fatalf("tools[1] = %q", ev.Tools[1])
	}
}

// Text and tool blocks in one message must both survive, text first.
func TestParseMixedTextAndToolUse(t *testing.T) {
	ev := ParseLine([]byte(`{"type":"assistant","message":{"content":[` +
		`{"type":"text","text":"checking the tests"},` +
		`{"type":"tool_use","name":"Bash","input":{"command":"go vet ./..."}}]}}`))
	if ev.Kind != KindAssistantText {
		t.Fatalf("kind = %q, want assistant_text", ev.Kind)
	}
	if ev.Text != "checking the tests" {
		t.Fatalf("text = %q", ev.Text)
	}
	if len(ev.Tools) != 1 || ev.Tools[0] != "↳ Bash go vet ./..." {
		t.Fatalf("tools = %v", ev.Tools)
	}
}

// An unknown tool, or one whose input has no field we recognize, still names
// itself rather than dumping raw JSON at the operator.
func TestParseToolUseUnknownShape(t *testing.T) {
	ev := ParseLine([]byte(`{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"mcp__thing__do","input":{"weird":{"nested":1}}}]}}`))
	if len(ev.Tools) != 1 || ev.Tools[0] != "↳ mcp__thing__do" {
		t.Fatalf("tools = %v", ev.Tools)
	}
}

// Malformed input must not panic or leak newlines into the transcript.
func TestParseToolUseMalformedInput(t *testing.T) {
	ev := ParseLine([]byte(`{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"Bash","input":"not-an-object"}]}}`))
	if len(ev.Tools) != 1 || ev.Tools[0] != "↳ Bash" {
		t.Fatalf("tools = %v", ev.Tools)
	}
	long := ParseLine([]byte(`{"type":"assistant","message":{"content":[` +
		`{"type":"tool_use","name":"Bash","input":{"command":"echo ` + strings.Repeat("x", 400) + `"}}]}}`))
	if strings.Contains(long.Tools[0], "\n") {
		t.Fatal("summary leaked a newline")
	}
	if len([]rune(long.Tools[0])) > 120 {
		t.Fatalf("summary not truncated: %d runes", len([]rune(long.Tools[0])))
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
	if !ok || d.Kind != levers.DecisionChoice || d.Why != "b is safer" ||
		len(d.Consequences) != 2 || d.Reversible != "until execute" {
		t.Fatalf("v2 fields: %+v ok=%v", d, ok)
	}
}

func TestExtractChoiceDecisionAllowsFreeform(t *testing.T) {
	text := `{"watchtower_decision":{"kind":"choice","question":"Fix it?","options":["Apply fix","Hold"],"recommended":0,"allow_freeform":true,"importance":0.8,"why":"The fix is scoped.","consequences":["Tests rerun.","Branch is preserved."],"reversible":"yes"}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Kind != levers.DecisionChoice || !d.AllowFreeform {
		t.Fatalf("decision = %#v, %v", d, ok)
	}
}

func TestExtractFreeformDecision(t *testing.T) {
	text := `{"watchtower_decision":{"kind":"freeform","question":"Review spec.md","recommended_response":"Approve spec.md as written.","importance":0.8,"why":"It matches the design.","consequences":["Planning begins."],"reversible":"yes"}}`
	d, ok := ExtractDecision(text)
	if !ok || d.Kind != levers.DecisionFreeform ||
		d.RecommendedResponse != "Approve spec.md as written." {
		t.Fatalf("decision = %#v, %v", d, ok)
	}
}

func TestExtractDecisionRejectsInvalidKindPayload(t *testing.T) {
	cases := []string{
		`{"watchtower_decision":{"kind":"choice","question":"Q?","recommended":0}}`,
		`{"watchtower_decision":{"kind":"choice","question":"Q?","options":["a"],"recommended":1}}`,
		`{"watchtower_decision":{"kind":"freeform","question":"Q?"}}`,
		`{"watchtower_decision":{"kind":"unknown","question":"Q?","options":["a"]}}`,
	}
	for _, text := range cases {
		if d, ok := ExtractDecision(text); ok {
			t.Errorf("ExtractDecision(%q) = %#v, true", text, d)
		}
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
