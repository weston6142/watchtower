package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/projection"
	"github.com/wbushyeager/guildhall/internal/proto"
)

func TestRenderHeaderSeverityOrder(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	h := renderHeader(&proto.Overview{Failing: 1, NeedYou: 2, Building: 3}, 100)
	if !strings.Contains(h, "1 build failing") || !strings.Contains(h, "2 questions for you") {
		t.Fatalf("header: %q", h)
	}
	h = renderHeader(&proto.Overview{Building: 2, ShippedToday: 1, TokensTotal: 41000, DollarsTotal: 0.35}, 100)
	if !strings.Contains(h, "all clear") || !strings.Contains(h, "~$0.35") {
		t.Fatalf("calm header: %q", h)
	}
	h = renderHeader(&proto.Overview{Building: 2}, 100)
	if strings.Contains(h, "$") {
		t.Fatalf("dollars shown when price unset: %q", h)
	}
}

func TestNoticeRowAlwaysReserved(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	st := projection.NewState()
	empty := renderNoticeRow(st, 80)
	if lipgloss.Height(empty) != 1 {
		t.Fatalf("empty notice row height %d", lipgloss.Height(empty))
	}
}

func TestRenderTowerPlacesCards(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "payment adapter", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "execute"}),
		mkev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "search fix", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-2", map[string]any{"stage": "spec"}),
		mkev(t, core.EvMergeSequenced, "GH-2", map[string]any{"behind": "GH-1"}),
	})
	out := renderTower(m.State, m.stages, m.Ids, m.Focus, 100)
	lines := strings.Split(out, "\n")
	var execLine, specLine string
	for _, line := range lines {
		if strings.Contains(line, "EXECUTE") {
			execLine = line
		}
		if strings.Contains(line, "SPEC") {
			specLine = line
		}
	}
	if !strings.Contains(execLine, "PA GH-1") {
		t.Fatalf("PA not on execute floor: %q", execLine)
	}
	if !strings.Contains(specLine, "SF GH-2") || !strings.Contains(specLine, "behind:PA") {
		t.Fatalf("SF card wrong: %q", specLine)
	}
	if !strings.Contains(out, "shipping order:") || !strings.Contains(out, "questions 0") {
		t.Fatalf("war room missing:\n%s", out)
	}
	if strings.LastIndex(out, "MERGE") < strings.Index(out, "BRAINSTORM") {
		t.Fatal("merge floor not at the bottom")
	}
}
