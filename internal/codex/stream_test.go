package codex

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/weston6142/watchtower/internal/runner"
)

func TestParseLineNormalizesCodexEvents(t *testing.T) {
	tests := []struct {
		name string
		line string
		want Event
	}{
		{"thread", `{"type":"thread.started","thread_id":"thr-123"}`, Event{Kind: KindThread, ThreadID: "thr-123"}},
		{"message", `{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"done"}}`, Event{Kind: KindText, Text: "done"}},
		{"command", `{"type":"item.completed","item":{"id":"i2","type":"command_execution","command":"go test ./...","status":"completed"}}`, Event{Kind: KindTool, Tool: "↳ command go test ./..."}},
		{"file", `{"type":"item.completed","item":{"id":"i3","type":"file_change","changes":[{"path":"a.go"}]}}`, Event{Kind: KindTool, Tool: "↳ files a.go"}},
		{"mcp", `{"type":"item.completed","item":{"id":"i4","type":"mcp_tool_call","server":"jira","tool":"search","status":"completed"}}`, Event{Kind: KindTool, Tool: "↳ mcp jira/search"}},
		{"web", `{"type":"item.completed","item":{"id":"i5","type":"web_search","query":"Codex docs"}}`, Event{Kind: KindTool, Tool: "↳ web Codex docs"}},
		{"complete", `{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":25}}`, Event{Kind: KindComplete, Tokens: 125}},
		{"failed", `{"type":"turn.failed","error":{"message":"model unavailable"}}`, Event{Kind: KindFailed, Error: "model unavailable"}},
		{"error", `{"type":"error","message":"auth failed"}`, Event{Kind: KindFailed, Error: "auth failed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseLine([]byte(tt.line)); got != tt.want {
				t.Fatalf("ParseLine() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseLineIgnoresMalformedUnknownAndProgressEvents(t *testing.T) {
	for _, line := range []string{
		"garbage",
		`{"type":"future.event"}`,
		`{"type":"item.started","item":{"type":"command_execution"}}`,
	} {
		if got := ParseLine([]byte(line)); got.Kind != KindOther {
			t.Errorf("ParseLine(%q) = %#v, want other", line, got)
		}
	}
}

func TestParseLinePreservesStructuredFailureClass(t *testing.T) {
	got := ParseLine([]byte(`{"type":"error","error":{"code":"authentication_failed","category":"auth","message":"credentials rejected"}}`))
	if got.Kind != KindFailed || got.FailureClass != runner.FailureAuthentication {
		t.Fatalf("structured failure = %#v, want authentication class", got)
	}
}

func TestParseLineBoundsToolOutput(t *testing.T) {
	command := "go test " + strings.Repeat("x", 400) + "\nwith a newline"
	line := `{"type":"item.completed","item":{"type":"command_execution","command":` +
		`"` + strings.ReplaceAll(command, "\n", `\n`) + `"}}`
	got := ParseLine([]byte(line))
	if got.Kind != KindTool {
		t.Fatalf("ParseLine() = %#v, want tool", got)
	}
	if strings.Contains(got.Tool, "\n") {
		t.Fatalf("tool output contains newline: %q", got.Tool)
	}
	if n := utf8.RuneCountInString(got.Tool); n > 120 {
		t.Fatalf("tool output is %d runes: %q", n, got.Tool)
	}
}
