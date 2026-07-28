package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/projection"
	"github.com/weston6142/watchtower/internal/proto"
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
	empty := renderNoticeRow(st, nil, 80)
	if lipgloss.Height(empty) != 1 {
		t.Fatalf("empty notice row height %d", lipgloss.Height(empty))
	}
}

// A parked lane must still show how far it got. Painting every row "paused"
// hides the resume point and reads as a hung tower.
func TestPausedLaneMarksOnlyItsCurrentStage(t *testing.T) {
	iv := &projection.IssueView{
		ID: "GH-1", Title: "t",
		Completed: []string{"brainstorm"}, CurrentStage: "spec",
		Paused: true, State: "paused",
	}
	stages := []string{"brainstorm", "spec", "execute"}
	var got []string
	for i, stage := range stages {
		got = append(got, ansi.Strip(cellContentForStage(iv, nil, stage, i, 0, false, true)))
	}
	if strings.Contains(got[0], "paused") {
		t.Fatalf("completed stage shows paused: %q", got[0])
	}
	if !strings.Contains(got[0], glyphDone) {
		t.Fatalf("completed stage lost its tick: %q", got[0])
	}
	if !strings.Contains(got[1], "paused") {
		t.Fatalf("current stage missing paused: %q", got[1])
	}
	if strings.Contains(got[2], "paused") {
		t.Fatalf("later stage shows paused: %q", got[2])
	}
}

// Rehydrated and pre-payload lanes have no usable CurrentStage; the marker
// falls back to the first stage that has not finished.
func TestPausedLaneWithStaleCurrentStageFallsBack(t *testing.T) {
	iv := &projection.IssueView{
		ID: "GH-1", Title: "t",
		Completed: []string{"brainstorm"}, CurrentStage: "brainstorm",
		Paused: true, State: "paused",
	}
	stages := []string{"brainstorm", "spec", "execute"}
	var got []string
	for i, stage := range stages {
		got = append(got, ansi.Strip(cellContentForStage(iv, nil, stage, i, 0, false, true)))
	}
	if !strings.Contains(got[0], glyphDone) {
		t.Fatalf("completed stage lost its tick: %q", got[0])
	}
	if !strings.Contains(got[1], "paused") {
		t.Fatalf("expected fallback marker on spec: %q", got[1])
	}
}

// A lane waiting on a human under a blank notice row reads as a hung tower.
func TestNoticeRowNamesParkedLane(t *testing.T) {
	st := projection.NewState()
	st.Order = []string{"GH-2"}
	st.Issues["GH-2"] = &projection.IssueView{
		ID: "GH-2", Title: "rewrite refs",
		Completed: []string{"brainstorm"}, CurrentStage: "spec",
		Paused: true, State: "paused",
	}
	ids := map[string]Identity{"GH-2": {Tag: "RG"}}
	got := ansi.Strip(renderNoticeRow(st, ids, 80))
	if !strings.Contains(got, "RG") || !strings.Contains(got, "spec") || !strings.Contains(got, "p resumes") {
		t.Fatalf("notice row = %q", got)
	}
}

// Real notices outrank the derived hint.
func TestNoticeRowPrefersRealNotices(t *testing.T) {
	st := projection.NewState()
	st.Order = []string{"GH-2"}
	st.Issues["GH-2"] = &projection.IssueView{ID: "GH-2", Paused: true, State: "paused"}
	st.Notices = []projection.Notice{{Text: "✉ new idea from GH-3: something", Seq: 1}}
	got := ansi.Strip(renderNoticeRow(st, nil, 80))
	if !strings.Contains(got, "new idea") {
		t.Fatalf("notice row = %q", got)
	}
}

// Nothing parked, nothing to say — the row stays blank at full width.
func TestNoticeRowBlankWhenNothingParked(t *testing.T) {
	st := projection.NewState()
	st.Order = []string{"GH-2"}
	st.Issues["GH-2"] = &projection.IssueView{ID: "GH-2", State: "running", CurrentStage: "spec"}
	if got := renderNoticeRow(st, nil, 20); strings.TrimSpace(got) != "" {
		t.Fatalf("notice row = %q, want blank", got)
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
	out := renderTower(m.State, m.stages, m.Ids, m.Focus, 0, 100)
	if !strings.Contains(out, "PA") || !strings.Contains(out, "GH-1") || !strings.Contains(out, "payment") {
		t.Fatalf("payment lane missing:\n%s", out)
	}
	if !strings.Contains(out, "SF") || !strings.Contains(out, "GH-2") || !strings.Contains(out, "after") {
		t.Fatalf("search lane wrong:\n%s", out)
	}
	if !strings.Contains(out, "shipping order:") || !strings.Contains(out, "questions 0") {
		t.Fatalf("war room missing:\n%s", out)
	}
	if strings.LastIndex(out, "MERGE") < strings.Index(out, "BRAINSTORM") {
		t.Fatal("merge floor not at the bottom")
	}
}

func TestWarRoomExpanded(t *testing.T) {
	st := projection.NewState()
	collapsed := warRoomLines(st, nil, false)
	if len(collapsed) != 1 {
		t.Fatalf("collapsed war room = %d lines, want 1", len(collapsed))
	}
	expanded := warRoomLines(st, nil, true)
	if len(expanded) != 2 {
		t.Fatalf("expanded war room = %d lines, want 2", len(expanded))
	}
	if !strings.Contains(expanded[1], "shipping order") {
		t.Fatalf("breakout line missing detail: %q", expanded[1])
	}
}

func TestCellWordsAndStates(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "payment adapter", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "execute", "attempt": float64(1), "of": float64(2)}),
		mkev(t, core.EvIssueCreated, "GH-2", map[string]any{"title": "search fix", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-2", map[string]any{"stage": "spec", "attempt": float64(1), "of": float64(1)}),
		mkev(t, core.EvDecisionRequired, "GH-2", map[string]any{
			"decision_id": float64(1), "stage": "spec", "question": "q",
			"options": []any{"a"}, "recommended": float64(0)}),
		mkev(t, core.EvMergeSequenced, "GH-2", map[string]any{"behind": "GH-1"}),
		mkev(t, core.EvIssueCreated, "GH-3", map[string]any{"title": "auth patch", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-3", map[string]any{"stage": "execute", "attempt": float64(2), "of": float64(2)}),
		mkev(t, core.EvStageFailed, "GH-3", map[string]any{"stage": "execute", "error": "boom", "attempt": float64(2), "of": float64(2), "final": true}),
	})
	out := renderTower(m.State, m.stages, m.Ids, m.Focus, 0, 120)
	for _, want := range []string{"need-you", "failed", "after", "payment", "search fix"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "behind:") || strings.Contains(out, "🔒") {
		t.Fatalf("old jargon rendering survived:\n%s", out)
	}
}

func TestStageAliases(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	m := NewModel(nil, []string{"spec", "execute"})
	m.aliases = map[string]string{"spec": "AGREE", "execute": "BUILD"}
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}),
	})
	out := renderTowerConfigured(m.State, m.stages, m.Ids, m.Focus, m.aliases, false, 0, 100, false)
	if !strings.Contains(out, "AGREE") || strings.Contains(out, "SPEC") {
		t.Fatalf("aliases not applied:\n%s", out)
	}
}

func TestVisibleLanesCompaction(t *testing.T) {
	order := []string{"A", "B", "C", "D", "E", "F", "G", "H"}
	full, left, right := visibleLanes(order, 4 /* focused=E */, 100)
	if len(left)+len(full)+len(right) != 8 {
		t.Fatalf("lanes lost: %v %v %v", left, full, right)
	}
	found := false
	for _, lane := range full {
		if lane == "E" {
			found = true
		}
	}
	if !found {
		t.Fatal("focused lane not full-width")
	}
	if len(left) == 0 && len(right) == 0 {
		t.Fatal("no compaction at 8 lanes/100 cols")
	}
}

func TestShelfRendersShippedAndParked(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	out := renderShelf([]shelfItem{{ID: "GH-1", Title: "payment adapter", Parked: false},
		{ID: "GH-3", Title: "auth patch", Parked: true}},
		map[string]Identity{"GH-1": {Tag: "PA"}, "GH-3": {Tag: "AP"}}, 100)
	if !strings.Contains(out, "SHIPPED") || !strings.Contains(out, "PARKED") || !strings.Contains(out, "auth patch") {
		t.Fatalf("shelf:\n%s", out)
	}
}

func TestRowsReuseStateWords(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	st := projection.NewState()
	st.Issues["GH-1"] = &projection.IssueView{ID: "GH-1", Title: "broken", CurrentStage: "execute", State: "failed"}
	st.Order = []string{"GH-1"}
	out := renderRows(st, []string{"spec", "execute"}, map[string]Identity{"GH-1": {Tag: "BR", Color: "#61afef"}}, Focus{Issue: "GH-1"}, 1, 100)
	if !strings.Contains(out, "failed") {
		t.Fatalf("rows did not reuse failed cell:\n%s", out)
	}
}

func TestRenderHelpOverlay(t *testing.T) {
	out := ansi.Strip(renderHelpOverlay(100))
	for _, want := range []string{
		"help", "esc close",
		"NAVIGATION", "CONTROL", "DOORS", "DECISIONS",
		"war room", "lever editor", "architecture pane / map",
		"? / esc", "close", "q / ctrl+c", "quit", "STATES",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SYSTEM") {
		t.Fatal("SYSTEM section should be gone")
	}
	// bordered box
	if !strings.Contains(out, "┌") || !strings.Contains(out, "└") {
		t.Fatal("expected box border")
	}
}
