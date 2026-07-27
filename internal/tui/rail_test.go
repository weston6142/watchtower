package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/projection"
)

func TestRenderToastMarksRecommended(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	d := projection.DecisionView{ID: 4, IssueID: "GH-1", Stage: "spec",
		Question: "Approve spec artifacts?", Options: []string{"approve", "reject"}, Recommended: 0}
	out := renderToast(d, Identity{Color: "#e06c75", Tag: "PA"}, 60)
	if !strings.Contains(out, "Approve spec artifacts?") || !strings.Contains(out, "★ approve") {
		t.Fatalf("toast:\n%s", out)
	}
	if !strings.Contains(out, "y accept") {
		t.Fatalf("keys missing:\n%s", out)
	}
}

func TestRenderRailShowsQueueOrder(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	m := navModel(t)
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvDecisionRequired, "GH-1", map[string]any{
			"decision_id": float64(1), "stage": "spec", "question": "first?",
			"options": []any{"a"}, "recommended": float64(0)}),
		mkev(t, core.EvDecisionRequired, "GH-2", map[string]any{
			"decision_id": float64(2), "stage": "spec", "question": "second?",
			"options": []any{"a"}, "recommended": float64(0)}),
	})
	out := renderRail(m.State, m.Ids, nil, 40)
	if strings.Index(out, "first?") > strings.Index(out, "second?") {
		t.Fatalf("queue order wrong:\n%s", out)
	}
}
