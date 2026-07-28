package tui

import (
	"testing"

	"github.com/weston6142/watchtower/internal/core"
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
	f := Focus{Floor: 0}
	f = moveFocus(f, m.State, m.stages, "j")
	f = moveFocus(f, m.State, m.stages, "j")
	if f.Issue != "GH-1" {
		t.Fatalf("expected GH-1 focused, got %+v", f)
	}
	f = moveFocus(f, m.State, m.stages, "l")
	if f.Issue != "GH-2" {
		t.Fatalf("l: %+v", f)
	}
	f = moveFocus(f, m.State, m.stages, "l")
	if f.Issue != "GH-2" {
		t.Fatalf("clamp: %+v", f)
	}
	f = moveFocus(f, m.State, m.stages, "tab")
	if f.Issue != "GH-3" {
		t.Fatalf("tab: %+v", f)
	}
	f = moveFocus(f, m.State, m.stages, "1")
	if f.Issue != "GH-1" {
		t.Fatalf("jump: %+v", f)
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
