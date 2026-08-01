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

func TestUserMessage(t *testing.T) {
	line := string(UserMessage("go on"))
	if !strings.Contains(line, `"type":"user"`) || !strings.Contains(line, "go on") || !strings.HasSuffix(line, "\n") {
		t.Fatalf("bad user line: %q", line)
	}
}
