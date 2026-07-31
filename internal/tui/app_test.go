package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/projection"
	"github.com/weston6142/watchtower/internal/proto"
)

func mkev(t *testing.T, typ core.EventType, issue string, payload any) core.Event {
	t.Helper()
	e, err := core.NewEvent(typ, issue, payload)
	if err != nil {
		t.Fatal(err)
	}
	seq++
	e.Seq = seq
	return e
}

var seq int64

type spyReporter struct {
	calls [][3]int
}

func (s *spyReporter) Report(needYou, failing, building int) {
	s.calls = append(s.calls, [3]int{needYou, failing, building})
}

func TestOverviewUpdateFeedsHerdrReporter(t *testing.T) {
	m := NewModel(nil, []string{"spec", "execute"})
	spy := &spyReporter{}
	m.SetHerdrReporter(spy)

	next, _ := m.Update(overviewMsg{overview: &proto.Overview{NeedYou: 2, Failing: 1, Building: 4}})
	m = next.(Model)

	if len(spy.calls) != 1 || spy.calls[0] != [3]int{2, 1, 4} {
		t.Fatalf("reporter calls = %v, want [[2 1 4]]", spy.calls)
	}

	// An errored overview poll must not report.
	m.Update(overviewMsg{err: errors.New("boom")})
	if len(spy.calls) != 1 {
		t.Fatalf("reporter called on overview error: %v", spy.calls)
	}
}

func TestApplyEventsBuildsStateAndToast(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "payment adapter", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "spec"}),
		mkev(t, core.EvDecisionRequired, "GH-1", map[string]any{
			"decision_id": float64(7), "stage": "spec", "question": "Approve?",
			"options": []any{"approve", "reject"}, "recommended": float64(0)}),
	})
	if m.State.Issues["GH-1"] == nil || m.Ids["GH-1"].Tag != "PA" {
		t.Fatalf("state/ids: %+v", m.Ids)
	}
	if m.Toast == nil || m.Toast.ID != 7 {
		t.Fatalf("toast not raised: %+v", m.Toast)
	}
	if m.lastSeq != 3 {
		t.Fatalf("lastSeq: %d", m.lastSeq)
	}
}

func pressKey(t *testing.T, m Model, key string) Model {
	t.Helper()
	var msg tea.KeyMsg
	switch key {
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "left":
		msg = tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		msg = tea.KeyMsg{Type: tea.KeyRight}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "ctrl+s":
		msg = tea.KeyMsg{Type: tea.KeyCtrlS}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	next, _ := m.Update(msg)
	return next.(Model)
}

func TestModalCtrlSValidatesTitle(t *testing.T) {
	m := Model{State: projection.NewState()}
	m = pressKey(t, m, "n")
	m = pressKey(t, m, "ctrl+s")
	if m.Err != "title is required" {
		t.Fatalf("Err = %q", m.Err)
	}
}

// The modal has no priority parse left to fail, so normal is what a new issue
// carries without anyone touching the field.
func TestModalPriorityDefaultsToNormal(t *testing.T) {
	m := Model{State: projection.NewState()}
	m = pressKey(t, m, "n")
	if m.modal == nil || m.modal.Priority != 0 {
		t.Fatalf("new modal priority = %+v, want 0", m.modal)
	}
	if out := ansi.Strip(renderModal(*m.modal, 80)); !strings.Contains(out, "normal") {
		t.Fatalf("modal does not show the level name:\n%s", out)
	}
}

// h/l cycle the chooser and wrap, so every reachable value is a named level.
func TestModalPriorityCycles(t *testing.T) {
	m := openPriorityField(t)
	for _, tc := range []struct {
		key  string
		want int
	}{{"l", 1}, {"l", 2}, {"l", -1}, {"h", 2}, {"h", 1}, {"h", 0}} {
		m = pressKey(t, m, tc.key)
		if m.modal.Priority != tc.want {
			t.Fatalf("after %q: Priority = %d, want %d", tc.key, m.modal.Priority, tc.want)
		}
	}
}

// left/right alias to h/l inside the modal: the global arrow→vim aliasing runs
// after the modal branch returns, so the cycler has to handle them itself.
func TestModalPriorityArrowKeys(t *testing.T) {
	m := openPriorityField(t)
	m = pressKey(t, m, "right")
	if m.modal.Priority != 1 {
		t.Fatalf("right: Priority = %d, want 1", m.modal.Priority)
	}
	m = pressKey(t, m, "left")
	if m.modal.Priority != 0 {
		t.Fatalf("left: Priority = %d, want 0", m.modal.Priority)
	}
}

// Guards the removal of priority from setFieldValue/fieldValue: text cannot
// land in the field at all, so a bad priority is unreachable, not just rejected.
func TestModalPriorityIgnoresTextInput(t *testing.T) {
	m := openPriorityField(t)
	for _, key := range []string{"a", "5", "backspace"} {
		m = pressKey(t, m, key)
		if m.modal.Priority != 0 {
			t.Fatalf("%q leaked into priority: %d", key, m.modal.Priority)
		}
	}
}

// The h/l interception is gated on the priority field; with the title focused
// they are ordinary letters. No golden can catch this, since a snapshot only
// poses a static state.
func TestModalTextFieldsStillAcceptHL(t *testing.T) {
	m := Model{State: projection.NewState()}
	m = pressKey(t, m, "n")
	for _, key := range []string{"h", "e", "l", "l", "o"} {
		m = pressKey(t, m, key)
	}
	if m.modal.Title != "hello" {
		t.Fatalf("Title = %q, want hello", m.modal.Title)
	}
}

// An out-of-set stored priority is rendered, never renumbered on open: losing
// the odd value takes a deliberate keypress.
func TestModalPriorityKeepsOutOfSetOnOpen(t *testing.T) {
	s := projection.NewState()
	ev, _ := core.NewEvent(core.EvIssueDrafted, "GH-9", map[string]any{
		"title": "stale", "body": "b", "flow": "default", "preset": "regular", "priority": 5})
	s.Apply(ev)
	m := Model{State: s}
	m = pressKey(t, m, "b")
	m = pressKey(t, m, "enter")
	if m.modal == nil || m.modal.Priority != 5 {
		t.Fatalf("edit modal renumbered priority: %+v", m.modal)
	}
	for i := 0; i < priorityField; i++ {
		m = pressKey(t, m, "tab")
	}
	m = pressKey(t, m, "h")
	if m.modal.Priority != 2 {
		t.Fatalf("h from 5: Priority = %d, want 2 (urgent)", m.modal.Priority)
	}
}

// openPriorityField opens a new-issue modal with the priority field focused.
func openPriorityField(t *testing.T) Model {
	t.Helper()
	m := Model{State: projection.NewState()}
	m = pressKey(t, m, "n")
	for i := 0; i < priorityField; i++ {
		m = pressKey(t, m, "tab")
	}
	if m.modal == nil || m.modal.Field != priorityField {
		t.Fatalf("priority field not focused: %+v", m.modal)
	}
	return m
}

func backlogFixtureState() *projection.State {
	s := projection.NewState()
	applyBacklogDrafts(s)
	return s
}

func TestBacklogEntriesSorted(t *testing.T) {
	entries := backlogEntries(backlogFixtureState())
	if len(entries) != 2 || entries[0].ID != "GH-3" || entries[1].ID != "GH-2" {
		t.Fatalf("order wrong: %v", entries)
	}
}

func TestBacklogViewKeys(t *testing.T) {
	m := Model{State: backlogFixtureState()}
	m = pressKey(t, m, "b")
	if m.backlog == nil {
		t.Fatal("b did not open the backlog view")
	}
	m = pressKey(t, m, "enter")
	if m.modal == nil || m.modal.EditID != "GH-3" || m.modal.Title != "hot fix" || m.modal.Priority != 2 {
		t.Fatalf("edit modal not prefilled: %+v", m.modal)
	}
	m = pressKey(t, m, "esc")
	m = pressKey(t, m, "l")
	if m.confirm == nil || m.confirm.Op != "launch_issue" || m.confirm.IssueID != "GH-3" {
		t.Fatalf("launch confirm wrong: %+v", m.confirm)
	}
	m = pressKey(t, m, "n")
	m = pressKey(t, m, "j")
	m = pressKey(t, m, "X")
	if m.confirm == nil || m.confirm.Op != "abandon_issue" || m.confirm.IssueID != "GH-2" {
		t.Fatalf("abandon confirm wrong: %+v", m.confirm)
	}
	m = pressKey(t, m, "esc")
	m = pressKey(t, m, "esc")
	if m.backlog != nil {
		t.Fatal("esc did not close the backlog view")
	}
}

// n inside the backlog opens the same new-issue modal the grid's n opens, and
// marks where to return.
func TestBacklogNOpensNewIssueModal(t *testing.T) {
	m := Model{State: backlogFixtureState()}
	m = pressKey(t, m, "b")
	m = pressKey(t, m, "n")
	if m.modal == nil {
		t.Fatal("n in the backlog did not open the modal")
	}
	if !m.modal.FromBacklog {
		t.Errorf("modal.FromBacklog = false, want true")
	}
	if m.modal.EditID != "" {
		t.Errorf("modal.EditID = %q, want empty: n creates, it does not edit", m.modal.EditID)
	}
	if m.modal.FlowName != "default" || m.modal.Preset != "regular" {
		t.Errorf("modal flow/preset = %q/%q, want default/regular", m.modal.FlowName, m.modal.Preset)
	}
	if m.backlog != nil {
		t.Errorf("backlog still open under the modal")
	}
}

// Filing the first draft into an empty backlog is precisely the case
// backlog.go:91 advertises, so n must not be gated on the selection.
func TestBacklogNOpensModalWhenEmpty(t *testing.T) {
	m := Model{State: projection.NewState()}
	m = pressKey(t, m, "b")
	if m.backlog == nil {
		t.Fatal("b did not open an empty backlog")
	}
	m = pressKey(t, m, "n")
	if m.modal == nil || !m.modal.FromBacklog {
		t.Fatalf("n with zero entries did not open a backlog-owned modal: %+v", m.modal)
	}
}

// The grid's n is unchanged: its modal returns to the grid.
func TestGridNLeavesFromBacklogFalse(t *testing.T) {
	m := Model{State: backlogFixtureState()}
	m = pressKey(t, m, "n")
	if m.modal == nil || m.modal.FromBacklog {
		t.Fatalf("grid modal = %+v, want FromBacklog false", m.modal)
	}
}

func toastModel(t *testing.T) Model {
	t.Helper()
	m := NewModel(nil, []string{"brainstorm", "spec"})
	return m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "payment adapter", "flow": "default"}),
		mkev(t, core.EvDecisionRequired, "GH-1", map[string]any{
			"decision_id": float64(7), "stage": "spec", "question": "Approve?",
			"options": []any{"approve", "reject", "defer"}, "recommended": float64(1)}),
	})
}

func TestToastSelectionStartsOnRecommended(t *testing.T) {
	m := toastModel(t)
	if m.Toast == nil || m.toastSel != 1 {
		t.Fatalf("toastSel = %d, want recommended 1 (toast %+v)", m.toastSel, m.Toast)
	}
}

func TestToastJKAndArrowsMoveSelection(t *testing.T) {
	m := toastModel(t)
	m = pressKey(t, m, "j")
	if m.toastSel != 2 {
		t.Fatalf("after j: toastSel = %d, want 2", m.toastSel)
	}
	m = pressKey(t, m, "j")
	if m.toastSel != 2 {
		t.Fatalf("j did not clamp at last option: %d", m.toastSel)
	}
	m = pressKey(t, m, "up")
	if m.toastSel != 1 {
		t.Fatalf("after up: toastSel = %d, want 1", m.toastSel)
	}
	m = pressKey(t, m, "k")
	m = pressKey(t, m, "k")
	if m.toastSel != 0 {
		t.Fatalf("k did not clamp at first option: %d", m.toastSel)
	}
}

func TestToastEscDismissesAndEnterDoesNotPanic(t *testing.T) {
	m := toastModel(t)
	m = pressKey(t, m, "enter") // nil client: no command, no panic
	m = pressKey(t, m, "esc")
	if m.Toast != nil {
		t.Fatalf("esc did not dismiss toast")
	}
}

func TestFreeformToastEnterOpensRecommendedResponseEditor(t *testing.T) {
	m := NewModel(nil, []string{"spec"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "spec", "flow": "default"}),
		mkev(t, core.EvDecisionRequired, "GH-1", map[string]any{
			"decision_id": float64(7), "stage": "spec", "kind": "freeform",
			"question": "Review spec.md", "recommended_response": "Approve spec.md as written."}),
	})
	m = pressKey(t, m, "enter")
	if m.decisionEditor == nil || m.decisionEditor.Value != "Approve spec.md as written." {
		t.Fatalf("editor = %#v", m.decisionEditor)
	}
	m = pressKey(t, m, "backspace")
	if strings.HasSuffix(m.decisionEditor.Value, ".") {
		t.Fatalf("backspace did not edit response: %#v", m.decisionEditor)
	}
}

func TestChoiceToastOtherOpensEmptyEditor(t *testing.T) {
	m := toastModel(t)
	m.Toast.AllowFreeform = true
	m = pressKey(t, m, "j")
	m = pressKey(t, m, "j")
	if m.toastSel != len(m.Toast.Options) {
		t.Fatalf("selection = %d, want Other index %d", m.toastSel, len(m.Toast.Options))
	}
	m = pressKey(t, m, "enter")
	if m.decisionEditor == nil || m.decisionEditor.Value != "" {
		t.Fatalf("editor = %#v", m.decisionEditor)
	}
}

func TestShelfAutoRetiresAndUnretiresMergedIssue(t *testing.T) {
	m := NewModel(nil, []string{"spec", "merge"})
	m.State.Issues["GH-1"] = &projection.IssueView{ID: "GH-1", Title: "shipped", Merged: true, MergedAt: time.Now().Add(-2 * time.Minute)}
	m.State.ShippedToday = []string{"GH-1"}
	m.SetRetireAfter(time.Minute)
	m.autoRetire(time.Now())
	if items := m.shelfItems(); len(items) != 1 || items[0].ID != "GH-1" {
		t.Fatalf("merged issue was not retired: %+v", items)
	}
	delete(m.retired, "GH-1")
	if items := m.shelfItems(); len(items) != 0 {
		t.Fatalf("unretire did not remove shelf item: %+v", items)
	}
}

// retireModel poses two merged lanes on the grid's last floor, both focusable.
func retireModel(t *testing.T) Model {
	t.Helper()
	m := NewModel(nil, []string{"spec", "merge"})
	m.State.Order = []string{"GH-1", "GH-2"}
	for _, id := range m.State.Order {
		m.State.Issues[id] = &projection.IssueView{ID: id, Title: "shipped " + id, State: "done", Merged: true, MergedAt: time.Now()}
		m.State.ShippedToday = append(m.State.ShippedToday, id)
	}
	m.Ids = map[string]Identity{"GH-1": {Tag: "G1"}, "GH-2": {Tag: "G2"}}
	m.Width, m.Height = 120, 40
	return m
}

func TestRetireHidesLaneFromGridAndRestoresItFromShelf(t *testing.T) {
	m := retireModel(t)
	m = pressKey(t, m, "1")
	if m.Focus.Issue != "GH-1" {
		t.Fatalf("expected GH-1 focused, got %q", m.Focus.Issue)
	}
	m = pressKey(t, m, "c")
	if cards := floorCards(m.State, m.stages, len(m.stages), m.retired); len(cards) != 1 || cards[0] != "GH-2" {
		t.Fatalf("retired lane still on the grid: %v", cards)
	}
	if strings.Contains(renderTowerConfigured(m.State, m.stages, m.Ids, m.Focus, nil, false, 0, 100, false, m.retired), "GH-1") {
		t.Fatal("tower still renders the retired lane")
	}
	if m.Focus.Issue == "GH-1" {
		t.Fatal("focus stayed on the retired lane")
	}

	m = pressKey(t, m, "u")
	m = pressKey(t, m, "enter")
	if cards := floorCards(m.State, m.stages, len(m.stages), m.retired); len(cards) != 2 {
		t.Fatalf("un-retire did not restore the lane: %v", cards)
	}
}

func TestDigitKeysSkipRetiredLanes(t *testing.T) {
	m := retireModel(t)
	m.retired = map[string]bool{"GH-1": true}
	m = pressKey(t, m, "1")
	if m.Focus.Issue != "GH-2" {
		t.Fatalf("digit key focused a hidden lane: %q", m.Focus.Issue)
	}
}

func TestIssueOpKeysHintWhenNothingFocused(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "payment adapter", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	})
	if m.Focus.Issue != "" {
		t.Fatalf("expected empty focus at startup, got %q", m.Focus.Issue)
	}
	for _, key := range []string{"L", "p", "R"} {
		m = pressKey(t, m, key)
		if m.Err != "no lane focused — press j or 1-9 to focus" {
			t.Fatalf("key %q: expected no-focus hint, got %q", key, m.Err)
		}
		if m.leverEditor != nil {
			t.Fatal("lever editor should not open without focus")
		}
	}
	// Focusing a lane clears the hint.
	m = pressKey(t, m, "j")
	if m.Focus.Issue == "" {
		t.Fatal("expected j to focus a lane")
	}
	if m.Err != "" {
		t.Fatalf("expected hint cleared after focus, got %q", m.Err)
	}
}

// laneModel builds a focused single-lane model in the given state.
func laneModel(t *testing.T, evs ...core.Event) Model {
	t.Helper()
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents(evs)
	m = pressKey(t, m, "j")
	if m.Focus.Issue != "GH-1" {
		t.Fatalf("expected GH-1 focused, got %q", m.Focus.Issue)
	}
	return m
}

// T is issue-scoped like p, x, and R. Opening an empty door instead of saying
// why is the same silent no-op those keys were fixed for.
func TestTranscriptKeyHintsWhenNothingFocused(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec", "execute", "review", "merge"})
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	})
	m = pressKey(t, m, "T")
	if m.Err != "no lane focused — press j or 1-9 to focus" {
		t.Fatalf("expected no-focus hint, got %q", m.Err)
	}
	if len(m.modes) != 0 {
		t.Fatalf("transcript door opened without focus: %v", m.modes)
	}
}

// With a lane focused the door still opens.
func TestTranscriptKeyOpensDoorWhenFocused(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	)
	m = pressKey(t, m, "T")
	if m.currentMode() != "transcript" {
		t.Fatalf("mode = %q, want transcript", m.currentMode())
	}
}

func TestTimelineRequiresFocus(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec"})
	m.Focus = Focus{}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	nm := next.(Model)
	if nm.currentMode() == "timeline" {
		t.Fatal("timeline opened with no lane focused")
	}
	if nm.Err != msgNoLaneFocused {
		t.Fatalf("Err = %q, want no-lane message", nm.Err)
	}
}

func TestTimelineRefreshesOnEvents(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "plan"})
	m.Focus.Issue = "GH-1"
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	nm := next.(Model)
	ev := mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "plan"})
	next, _ = nm.Update(Msg{Events: []core.Event{ev}})
	nm = next.(Model)
	if len(nm.doorLines) == 0 || !strings.Contains(nm.doorLines[len(nm.doorLines)-1], "plan started") {
		t.Fatalf("timeline did not pick up new event: %v", nm.doorLines)
	}
}

func TestWarRoomKeyToggles(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec"})
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	if !next.(Model).warExpanded {
		t.Fatal("g did not expand the war room")
	}
	next, _ = next.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	if next.(Model).warExpanded {
		t.Fatal("second g did not collapse the war room")
	}
}

// The overlay paints over the grid, so it has to swallow the grid's keys.
// Driving a lane you cannot see is worse than the key doing nothing.
func TestHelpOverlaySwallowsIssueKeys(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	)
	m = pressKey(t, m, "?")
	if !m.help {
		t.Fatal("? did not open help")
	}
	for _, key := range []string{"p", "x", "X", "L", "d", "T", "n"} {
		m = pressKey(t, m, key)
		if !m.help {
			t.Fatalf("key %q closed the help overlay", key)
		}
		if m.confirm != nil || m.modal != nil || m.leverEditor != nil || len(m.modes) != 0 {
			t.Fatalf("key %q drove the screen beneath the overlay", key)
		}
	}
}

// esc closes it, as the overlay's own footer promises.
func TestHelpOverlayEscCloses(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	)
	m = pressKey(t, m, "?")
	m = pressKey(t, m, "esc")
	if m.help {
		t.Fatal("esc did not close help")
	}
	// ? still toggles it closed too.
	m = pressKey(t, m, "?")
	m = pressKey(t, m, "?")
	if m.help {
		t.Fatal("? did not close help")
	}
}

func TestKillGuardOnIdleLane(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "dead lane", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
		mkev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "brainstorm", "error": "boom", "final": true}),
	)
	m = pressKey(t, m, "x")
	if m.confirm != nil {
		t.Fatal("kill confirm opened for idle lane")
	}
	if m.Err != "nothing running — R retries · X abandons" {
		t.Fatalf("expected kill hint, got %q", m.Err)
	}
}

func TestKillStillConfirmsOnRunningLane(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "busy lane", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	)
	m = pressKey(t, m, "x")
	if m.confirm == nil || m.confirm.Op != "kill_stage" {
		t.Fatalf("expected kill confirm, got %+v", m.confirm)
	}
}

func TestAbandonConfirmSendsOp(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "dead lane", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
		mkev(t, core.EvStageFailed, "GH-1", map[string]any{"stage": "brainstorm", "error": "boom", "final": true}),
	)
	m = pressKey(t, m, "X")
	if m.confirm == nil || m.confirm.Op != "abandon_issue" {
		t.Fatalf("expected abandon confirm, got %+v", m.confirm)
	}
	if !strings.Contains(m.confirm.Prompt, "abandon") {
		t.Fatalf("prompt: %q", m.confirm.Prompt)
	}
	m = pressKey(t, m, "y")
	if m.confirm != nil {
		t.Fatal("confirm not cleared after y")
	}
}

// pressKeyCmd is pressKey but surfaces the returned command so tests can
// detect a quit.
func pressKeyCmd(t *testing.T, m Model, key string) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	return next.(Model), cmd
}

func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

func TestModalSwallowsQuitAndHelpKeys(t *testing.T) {
	m := Model{State: projection.NewState()}
	m = pressKey(t, m, "n")
	m2, cmd := pressKeyCmd(t, m, "q")
	if isQuit(cmd) {
		t.Fatal("q quit the app while the modal was open")
	}
	if m2.modal == nil || m2.modal.Title != "q" {
		t.Fatalf("q was not typed into the modal: %+v", m2.modal)
	}
	m3, _ := pressKeyCmd(t, m2, "?")
	if m3.help {
		t.Fatal("? opened help while the modal was open")
	}
	if m3.modal.Title != "q?" {
		t.Fatalf("? was not typed into the modal: %+v", m3.modal)
	}
}

func TestBacklogSwallowsQuitAndHelpKeys(t *testing.T) {
	m := Model{State: backlogFixtureState()}
	m = pressKey(t, m, "b")
	m2, cmd := pressKeyCmd(t, m, "q")
	if isQuit(cmd) {
		t.Fatal("q quit the app while the backlog was open")
	}
	if m2.backlog == nil {
		t.Fatal("backlog closed on q")
	}
	m3, _ := pressKeyCmd(t, m2, "?")
	if m3.help {
		t.Fatal("? opened help while the backlog was open")
	}
	if m3.backlog == nil {
		t.Fatal("backlog closed on ?")
	}
}

// A refusal must leave the modal open with the typed paths intact so the typo
// is fixed in place.
func TestModalRefusalKeepsModalOpen(t *testing.T) {
	m := Model{State: projection.NewState()}
	m.modal = &modalState{Title: "t", Attach: "/tmp/ghost.log"}
	next, _ := m.Update(createIssueMsg{response: proto.Response{
		OK: false, Error: `attachment "/tmp/ghost.log": no such file`}})
	m = next.(Model)
	if m.modal == nil {
		t.Fatal("modal closed on refusal; the typed paths are gone")
	}
	if m.Err == "" {
		t.Fatal("refusal did not surface an error")
	}
	if m.modal.Attach != "/tmp/ghost.log" {
		t.Fatalf("typed attach text lost: %q", m.modal.Attach)
	}
}

// Opening a draft for edit shows its current attachments, so "retain
// everything" is the default and needs no typing.
func TestEditModalPrefillsAttachments(t *testing.T) {
	s := projection.NewState()
	s.Apply(mkev(t, core.EvIssueDrafted, "GH-4", map[string]any{
		"title": "with files", "body": "b", "flow": "default", "preset": "regular",
		"attachments": []string{"app.log", "shot.png"}}))
	m := Model{State: s}
	m = pressKey(t, m, "b")
	m = pressKey(t, m, "enter")
	if m.modal == nil || m.modal.EditID != "GH-4" {
		t.Fatalf("edit modal not open: %+v", m.modal)
	}
	if m.modal.Attach != "app.log, shot.png" {
		t.Fatalf("attach prefill = %q", m.modal.Attach)
	}
	if len(m.modal.OrigAttach) != 2 || m.modal.OrigAttach[0] != "app.log" {
		t.Fatalf("OrigAttach = %v", m.modal.OrigAttach)
	}
}

// Retain-by-name beats reading a file of the same name out of the cwd.
func TestResolveModalAttachRetainsExistingNames(t *testing.T) {
	got, err := resolveModalAttach(modalState{
		Attach: "app.log, /tmp/new.log", OrigAttach: []string{"app.log"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "app.log" || got[1] != "/tmp/new.log" {
		t.Fatalf("resolved = %v", got)
	}
}

// Paths are full of h and l — /Users, .log, html — and the priority cycler
// claims both keys. It is guarded on priorityField, so reaching attach through
// the real key router (not modalState.input directly) must insert them as text
// and leave the priority chooser alone.
func TestAttachFieldTakesHAndLThroughKeyRouter(t *testing.T) {
	m := Model{State: projection.NewState()}
	m = pressKey(t, m, "n")
	for i := 0; i < attachField; i++ {
		m = pressKey(t, m, "tab")
	}
	if m.modal == nil || m.modal.Field != attachField {
		t.Fatalf("attach field not focused: %+v", m.modal)
	}
	const path = "/tmp/html/app.log"
	for _, r := range path {
		m = pressKey(t, m, string(r))
	}
	if m.modal.Attach != path {
		t.Fatalf("Attach = %q, want %q", m.modal.Attach, path)
	}
	if m.modal.Priority != 0 {
		t.Fatalf("typing a path cycled Priority to %d", m.modal.Priority)
	}
}
