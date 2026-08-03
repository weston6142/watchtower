package tui

import (
	"slices"
	"testing"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/projection"
)

func navModel(t *testing.T) Model {
	t.Helper()
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	return m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "aa bb", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}),
		mkev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "cc dd", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-2", map[string]any{"stage": "spec"}),
		mkev(t, core.EvIssueCreated, "GH-3", map[string]any{"title": "ee ff", "flow": "default"}),
		mkev(t, core.EvStageFailed, "GH-3", map[string]any{"stage": "execute"}),
	})
}

func TestMoveFocusAndAttention(t *testing.T) {
	m := navModel(t)
	f := normalizeFocus(Focus{}, m.State, m.stages, nil)
	if f.Issue != "GH-1" {
		t.Fatalf("normalized focus = %+v, want GH-1", f)
	}
	f = moveFocus(f, m.State, m.stages, "l", nil)
	if f.Issue != "GH-2" {
		t.Fatalf("l: %+v", f)
	}
	f = moveFocus(f, m.State, m.stages, "l", nil)
	if f.Issue != "GH-2" {
		t.Fatalf("clamp: %+v", f)
	}
	f = moveFocus(f, m.State, m.stages, "tab", nil)
	if f.Issue != "GH-3" {
		t.Fatalf("tab: %+v", f)
	}
	f = moveFocus(f, m.State, m.stages, "1", nil)
	if f.Issue != "GH-1" {
		t.Fatalf("jump: %+v", f)
	}
}

func TestNormalizeFocusSelectsFirstEligibleInStableOrder(t *testing.T) {
	st := projection.NewState()
	st.Order = []string{"GH-1", "GH-2"}
	st.Issues["GH-1"] = &projection.IssueView{ID: "GH-1", CurrentStage: "execute", State: "running"}
	st.Issues["GH-2"] = &projection.IssueView{ID: "GH-2", CurrentStage: "spec", State: "running"}

	got := normalizeFocus(Focus{}, st, []string{"spec", "execute"}, nil)
	if got.Issue != "GH-1" || got.Floor != 2 || got.Card != 0 {
		t.Fatalf("focus = %+v, want GH-1 at floor 2 card 0", got)
	}
}

func TestNormalizeFocusRepairsStaleFocusAndPreservesValidFocus(t *testing.T) {
	st := projection.NewState()
	st.Order = []string{"GH-1", "GH-2"}
	st.Issues["GH-1"] = &projection.IssueView{ID: "GH-1", CurrentStage: "spec", State: "running"}
	st.Issues["GH-2"] = &projection.IssueView{ID: "GH-2", CurrentStage: "execute", State: "running"}

	if got := normalizeFocus(Focus{Issue: "missing"}, st, []string{"spec", "execute"}, nil); got.Issue != "GH-1" {
		t.Fatalf("stale focus = %+v, want GH-1", got)
	}
	got := normalizeFocus(Focus{Issue: "GH-2", Floor: 1, Card: 99}, st, []string{"spec", "execute"}, nil)
	if got.Issue != "GH-2" || got.Floor != 2 || got.Card != 0 {
		t.Fatalf("valid focus = %+v, want GH-2 at floor 2 card 0", got)
	}
}

func TestNormalizeFocusAllowsNoFocusOnlyWithoutEligibleLanes(t *testing.T) {
	st := projection.NewState()
	st.Order = []string{"GH-1"}
	st.Issues["GH-1"] = &projection.IssueView{ID: "GH-1", CurrentStage: "not-configured", State: "running"}
	if got := normalizeFocus(Focus{}, st, []string{"spec"}, nil); got.Issue != "" {
		t.Fatalf("unrenderable focus = %+v, want empty", got)
	}
	if got := normalizeFocus(Focus{}, nil, []string{"spec"}, nil); got.Issue != "" {
		t.Fatalf("nil-state focus = %+v, want empty", got)
	}
}

func TestClaimedLaneCanReceiveNumericFocusOnItsRenderedFloor(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "running", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}),
		mkev(t, core.EvIssueDrafted, "GH-2", map[string]any{"title": "claimed", "flow": "default"}),
		mkev(t, core.EvIssueClaimed, "GH-2", nil),
	})

	focused := moveFocus(Focus{Issue: "GH-1"}, m.State, m.stages, "2", nil)
	if focused.Issue != "GH-2" || focused.Floor != 1 {
		t.Fatalf("numeric focus = %+v, want claimed GH-2 on floor 1", focused)
	}
	cards := floorCards(m.State, m.stages, 1, nil)
	if !slices.Contains(cards, "GH-2") {
		t.Fatalf("claimed lane missing from rendered floor: %v", cards)
	}
}

func TestPausedLaneFocusMatchesFirstUnfinishedRenderedFloor(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "paused", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
		mkev(t, core.EvStageCompleted, "GH-1", map[string]any{"stage": "brainstorm"}),
		mkev(t, core.EvIssuePaused, "GH-1", nil),
	})

	focused := normalizeFocus(Focus{}, m.State, m.stages, nil)
	if focused.Issue != "GH-1" || focused.Floor != 2 {
		t.Fatalf("paused focus = %+v, want GH-1 on first unfinished floor 2", focused)
	}
	cards := floorCards(m.State, m.stages, 2, nil)
	if !slices.Contains(cards, "GH-1") {
		t.Fatalf("paused lane missing from rendered unfinished floor: %v", cards)
	}
}

func TestAttentionOrder(t *testing.T) {
	m := navModel(t)
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvDecisionRequired, "GH-2", map[string]any{
			"decision_id": float64(1), "stage": "spec", "question": "q",
			"options": []any{"a"}, "recommended": float64(0)}),
	})
	al := attentionList(m.State)
	if len(al) != 2 || al[0] != "GH-3" || al[1] != "GH-2" {
		t.Fatalf("attention: %v", al)
	}
}
