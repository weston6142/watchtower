package core

import (
	"encoding/json"
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
