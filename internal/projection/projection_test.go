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

func TestAttemptAndErrorSurfacing(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "x", "flow": "default"}))
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "execute", "attempt": float64(2), "of": float64(3)}))
	iv := s.Issues["GH-1"]
	if iv.Attempt != 2 || iv.AttemptOf != 3 {
		t.Fatalf("attempts: %+v", iv)
	}
	s.Apply(ev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "execute", "error": "2 tests failing", "attempt": float64(3), "of": float64(3), "final": true}))
	if iv.LastError != "2 tests failing" || iv.State != "failed" {
		t.Fatalf("error: %+v", iv)
	}
	s.Apply(ev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "execute", "attempt": float64(1), "of": float64(3)}))
	if iv.LastError != "" {
		t.Fatalf("new attempt did not clear error: %+v", iv)
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

func TestProjectionV2Fields(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "x", "flow": "default"}))
	s.Apply(ev(t, core.EvIssuePaused, "GH-1", nil))
	if !s.Issues["GH-1"].Paused {
		t.Fatal("paused not tracked")
	}
	s.Apply(ev(t, core.EvIssueResumed, "GH-1", nil))
	s.Apply(ev(t, core.EvArtifactProduced, "GH-1", map[string]any{
		"stage": "execute", "artifact": "evidence.json", "path": "/x",
		"area_weight": map[string]any{"payments": float64(300), "api": float64(40)}}))
	if s.Issues["GH-1"].AreaWeights["payments"] != 300 {
		t.Fatalf("weights: %v", s.Issues["GH-1"].AreaWeights)
	}
	s.Apply(ev(t, core.EvDecisionRequired, "GH-1", map[string]any{
		"decision_id": float64(1), "stage": "spec", "question": "Q?",
		"options": []any{"a", "b"}, "recommended": float64(0),
		"why": "a is safe", "consequences": []any{"c1", "c2"}, "reversible": "anytime"}))
	d := s.Decisions[1]
	if d.Why != "a is safe" || len(d.Consequences) != 2 || d.Reversible != "anytime" {
		t.Fatalf("decision v2: %+v", d)
	}
	s.Apply(ev(t, core.EvProposalFiled, "GH-1", map[string]any{"title": "new idea"}))
	if len(s.Notices) != 1 || s.Notices[0].Text != "✉ new idea from GH-1: new idea" {
		t.Fatalf("notices: %v", s.Notices)
	}
}

func TestProjectionTracksShippedAndParked(t *testing.T) {
	s := NewState()
	s.Apply(ev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "x"}))
	s.Apply(ev(t, core.EvIssueMerged, "GH-1", nil))
	if len(s.ShippedToday) != 1 || s.ShippedToday[0] != "GH-1" || s.Issues["GH-1"].MergedAt.IsZero() {
		t.Fatalf("shipped: %+v", s)
	}
	s.Apply(ev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "y"}))
	s.Apply(ev(t, core.EvStageFailed, "GH-2", map[string]any{"stage": "execute", "final": true}))
	if len(s.Parked) != 1 || s.Parked[0] != "GH-2" {
		t.Fatalf("parked failure: %v", s.Parked)
	}
	s.Apply(ev(t, core.EvIssueCreated, "GH-3", map[string]any{"title": "z"}))
	s.Apply(ev(t, core.EvStageKilled, "GH-3", map[string]any{"stage": "execute"}))
	if !s.Issues["GH-3"].Killed || s.Issues["GH-3"].State != "paused" {
		t.Fatalf("killed: %+v", s.Issues["GH-3"])
	}
	if len(s.Parked) != 2 || s.Parked[1] != "GH-3" {
		t.Fatalf("parked kill: %v", s.Parked)
	}
}
