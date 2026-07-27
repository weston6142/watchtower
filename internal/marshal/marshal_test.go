package marshal

import (
	"context"
	"testing"
	"time"

	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/touchset"
)

func TestSequencingAndRelease(t *testing.T) {
	var events []string
	m := New(func(typ core.EventType, issue string, payload any) {
		events = append(events, string(typ)+":"+issue)
	})
	m.PlanApproved("GH-1", touchset.Set{Globs: []string{"internal/pay/**"}})
	m.PlanApproved("GH-2", touchset.Set{Globs: []string{"internal/pay/refund.go"}})
	m.PlanApproved("GH-3", touchset.Set{Globs: []string{"docs/**"}})

	if m.BlockedBehind("GH-1") != 1 || m.BlockedBehind("GH-3") != 0 {
		t.Fatalf("blocked: GH-1=%d GH-3=%d", m.BlockedBehind("GH-1"), m.BlockedBehind("GH-3"))
	}
	found := false
	for _, e := range events {
		if e == "merge_sequenced:GH-2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no merge_sequenced event: %v", events)
	}

	if err := m.ReadyToMerge(context.Background(), "GH-3"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- m.ReadyToMerge(context.Background(), "GH-2") }()
	select {
	case <-done:
		t.Fatal("GH-2 should wait for GH-1")
	case <-time.After(50 * time.Millisecond):
	}
	m.Merged("GH-1")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("GH-2 never released")
	}
}

func TestAbortReleasesWaiters(t *testing.T) {
	m := New(func(core.EventType, string, any) {})
	m.PlanApproved("GH-1", touchset.Set{Globs: []string{"a/**"}})
	m.PlanApproved("GH-2", touchset.Set{Globs: []string{"a/b.go"}})
	done := make(chan error, 1)
	go func() { done <- m.ReadyToMerge(context.Background(), "GH-2") }()
	m.Aborted("GH-1")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("abort did not release waiter")
	}
}

func TestPlanApprovedIgnoresDuplicateRegistration(t *testing.T) {
	var events []string
	m := New(func(typ core.EventType, issueID string, payload any) {
		if typ == core.EvMergeSequenced {
			events = append(events, issueID)
		}
	})
	m.PlanApproved("GH-1", touchset.Set{Globs: []string{"src/**"}})
	m.PlanApproved("GH-2", touchset.Set{Globs: []string{"src/**"}})
	m.PlanApproved("GH-2", touchset.Set{Globs: []string{"src/**"}})
	if len(events) != 1 || events[0] != "GH-2" {
		t.Fatalf("duplicate registration emitted events: %v", events)
	}
}
