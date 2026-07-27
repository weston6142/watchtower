package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/projection"
)

func TestDecisionsDoorSelectable(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	ds := []projection.DecisionView{
		{ID: 2, IssueID: "GH-2", Stage: "spec", Question: "big blocker?"},
		{ID: 1, IssueID: "GH-1", Stage: "merge", Question: "small one?"},
	}
	out := renderDecisionsDoor(ds, map[string]Identity{"GH-1": {Tag: "01"}, "GH-2": {Tag: "02"}}, 1, 80)
	if strings.Index(out, "big blocker?") > strings.Index(out, "small one?") {
		t.Fatal("server order not preserved")
	}
	if !strings.Contains(out, "▸") {
		t.Fatalf("no selection marker:\n%s", out)
	}
}

func TestTimelineHumanizes(t *testing.T) {
	lines := humanizeEvents([]core.Event{
		mkev(t, core.EvIssueMerged, "GH-1", map[string]any{"branch": "issue/GH-1"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "review"}),
	}, "GH-1")
	if len(lines) != 2 || !strings.Contains(lines[0], "merged") || strings.Contains(lines[1], "stage_started") {
		t.Fatalf("humanize: %v", lines)
	}
}
