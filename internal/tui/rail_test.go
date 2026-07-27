package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/evidence"
	"github.com/wbushyeager/guildhall/internal/projection"
)

func TestRenderToastMarksRecommended(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	d := projection.DecisionView{ID: 4, IssueID: "GH-1", Stage: "spec",
		Question: "Approve spec artifacts?", Options: []string{"approve", "reject"}, Recommended: 0}
	out := renderToast(d, Identity{Color: "#61afef", Tag: "PA"}, 0, 60)
	if !strings.Contains(out, "Approve spec artifacts?") || !strings.Contains(out, "★ approve") {
		t.Fatalf("toast:\n%s", out)
	}
	if !strings.Contains(out, "y accept") {
		t.Fatalf("keys missing:\n%s", out)
	}
}

func TestToastV2RendersRationaleAndConsequences(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	d := projection.DecisionView{ID: 4, IssueID: "GH-1", Stage: "spec",
		Question: "Approve the spec?", Options: []string{"approve", "reject"}, Recommended: 0,
		Why: "scope is settled", Consequences: []string{"planning starts now", "agent revises (~10 min)"},
		Reversible: "changeable until build"}
	out := renderToast(d, Identity{Color: "#61afef", Tag: "PA"}, 4, 70)
	for _, want := range []string{"scope is settled", "planning starts now", "agent revises", "changeable until build", "4 recommendations in a row"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if out2 := renderToast(d, Identity{Color: "#61afef", Tag: "PA"}, 0, 70); strings.Contains(out2, "in a row") {
		t.Fatal("friction line shown with zero streak")
	}
}

func TestEvidencePanelFromBundle(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	b := evidence.Bundle{Added: 412, Removed: 88, Biggest: "payments/gateway/client.go",
		Files:      make([]evidence.FileStat, 14),
		AreaWeight: map[string]int{"payments": 300, "api": 40}}
	out := renderEvidence(b, "GH-1 payment adapter", 76)
	for _, want := range []string{"14 files", "+412", "−88", "payments/gateway/client.go", "payments"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
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
