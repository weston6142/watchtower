package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewEventMarshalsPayloadSnakeCase(t *testing.T) {
	ev, err := NewEvent(EvIssueCreated, "GH-1", map[string]string{"title": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != EvIssueCreated || ev.IssueID != "GH-1" {
		t.Fatalf("bad event: %+v", ev)
	}
	var p map[string]string
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["title"] != "hello" {
		t.Fatalf("payload lost: %v", p)
	}
	if ev.At.IsZero() {
		t.Fatal("At not set")
	}
}

func TestRunnerAttemptEventHasStablePublicPayload(t *testing.T) {
	ev, err := NewEvent(EvRunnerAttempt, "GH-38", map[string]any{
		"operation_id": "operation-1", "attempt_kind": "fallback", "state": "reserved",
		"failure_class": "launch", "redacted_argv": []string{"exec", "[redacted]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != EventType("runner_attempt") || !strings.Contains(string(ev.Payload), `"attempt_kind":"fallback"`) ||
		strings.Contains(string(ev.Payload), "prompt secret") {
		t.Fatalf("runner attempt event = %#v", ev)
	}
}
