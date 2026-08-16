package core

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/failure"
)

func TestNewEventAtUsesInjectedClock(t *testing.T) {
	fixed := time.Date(2026, 8, 13, 14, 15, 16, 123456789, time.UTC)
	clock := ClockFunc(func() time.Time { return fixed })

	event, err := NewEventAt(EvIssueCreated, "GH-69", map[string]string{"title": "deterministic"}, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !event.At.Equal(fixed) || event.At.Location() != time.UTC {
		t.Fatalf("event At = %v (%v), want %v (UTC)", event.At, event.At.Location(), fixed)
	}
}

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

func TestPlannerBudgetUpdatedEventTypeIsAdditive(t *testing.T) {
	if EvPlannerBudgetUpdated != EventType("planner_budget_updated") {
		t.Fatalf("planner event type = %q", EvPlannerBudgetUpdated)
	}
}

func TestCapabilityEventsExposeOnlyStableEvidence(t *testing.T) {
	for _, eventType := range []EventType{EvCapabilityCompiled, EvCapabilityPreflighted, EvCapabilityDenied, EvCapabilityValidated, EvCapabilityRejected} {
		event, err := NewEvent(eventType, "GH-68", map[string]any{
			"stage": "execute", "attempt_id": "checkpoint-1", "contract_id": strings.Repeat("a", 64),
			"reason": "capability_runtime_denied",
		})
		if err != nil {
			t.Fatal(err)
		}
		payload := string(event.Payload)
		if !strings.Contains(payload, "contract_id") || strings.Contains(payload, "secret") || strings.Contains(payload, "command") {
			t.Fatalf("unsafe capability event %q: %s", eventType, payload)
		}
	}
}

func TestFailureRecordedEventProjectsCanonicalRecord(t *testing.T) {
	record := failure.FailureRecord{
		RecordID: 17, SchemaVersion: failure.SchemaVersion, IssueID: "GH-63", Stage: "execute", StageAttempt: 3,
		FailureSite: failure.SiteVerification, FailureClass: failure.ClassValidation,
		RetryDisposition: failure.RetryAfterStateChange, RequiredStateChange: failure.StateVerification,
		Fingerprint: "sha256:" + strings.Repeat("a", 64), OccurredAt: time.Date(2026, 8, 9, 18, 0, 0, 123, time.UTC),
	}
	ev, err := NewEvent(EvFailureRecorded, record.IssueID, record)
	if err != nil {
		t.Fatal(err)
	}
	var got failure.FailureRecord
	if err := json.Unmarshal(ev.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, record) || ev.IssueID != record.IssueID {
		t.Fatalf("failure event = %+v, want %+v", got, record)
	}
	for _, secret := range []string{"raw error", "/private/repo", "run command", "configuration body", "normalized input", "artifact bytes", "decision text"} {
		if strings.Contains(string(ev.Payload), secret) {
			t.Fatalf("failure event leaked %q: %s", secret, ev.Payload)
		}
	}
}
