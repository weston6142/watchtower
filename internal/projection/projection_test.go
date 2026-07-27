package projection

import (
	"testing"

	"github.com/wbushyeager/guildhall/internal/core"
)

func ev(t *testing.T, typ core.EventType, issue string, payload any) core.Event {
	t.Helper()
	e, err := core.NewEvent(typ, issue, payload)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestReplayBuildsIssueView(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "hello"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}))
	s.Apply(ev(t, core.EvDecisionRequired, "GH-1", map[string]any{
		"decision_id": float64(1), "stage": "spec", "question": "Approve spec artifacts?",
		"options": []any{"approve", "reject"}, "recommended": float64(0)}))

	iv := s.Issues["GH-1"]
	if iv == nil || iv.Title != "hello" || iv.State != "waiting_decision" || iv.CurrentStage != "spec" {
		t.Fatalf("bad view: %+v", iv)
	}
	if len(s.Decisions) != 1 || s.Decisions[1].Question != "Approve spec artifacts?" {
		t.Fatalf("bad decisions: %+v", s.Decisions)
	}

	s.Apply(ev(t, core.EvDecisionAnswered, "GH-1", map[string]any{"decision_id": float64(1), "option": float64(0)}))
	s.Apply(ev(t, core.EvStageCompleted, "GH-1", map[string]any{"stage": "spec"}))
	if len(s.Decisions) != 0 || iv.State != "running" || len(iv.Completed) != 1 {
		t.Fatalf("after answer: %+v decisions=%v", iv, s.Decisions)
	}
}

func TestIssueCompletedSetsDone(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "x"}))
	s.Apply(ev(t, core.EvStageCompleted, "GH-2", map[string]any{"stage": "review"}))
	if s.Issues["GH-2"].State == "done" {
		t.Fatal("stage name must no longer imply done")
	}
	s.Apply(ev(t, core.EvIssueCompleted, "GH-2", nil))
	if s.Issues["GH-2"].State != "done" {
		t.Fatalf("want done, got %q", s.Issues["GH-2"].State)
	}
}

func TestMergeSequencingProjection(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "a", "flow": "default"}))
	s.Apply(ev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "b", "flow": "default"}))
	s.Apply(ev(t, core.EvMergeSequenced, "GH-2", map[string]any{"behind": "GH-1"}))
	if s.Issues["GH-2"].Behind != "GH-1" {
		t.Fatalf("behind: %+v", s.Issues["GH-2"])
	}
	if len(s.Order) != 2 || s.Order[0] != "GH-1" {
		t.Fatalf("order: %v", s.Order)
	}
	s.Apply(ev(t, core.EvIssueMerged, "GH-1", nil))
	if !s.Issues["GH-1"].Merged || s.Issues["GH-2"].Behind != "" {
		t.Fatalf("release: %+v %+v", s.Issues["GH-1"], s.Issues["GH-2"])
	}
	s.Apply(ev(t, core.EvProposalFiled, "GH-2", map[string]any{"title": "x"}))
	if s.ProposalCount != 1 {
		t.Fatalf("proposals: %d", s.ProposalCount)
	}
}
