package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
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

// esc out of a backlog-opened modal goes back to the backlog, the same way esc
// out of an edit modal already does.
func TestBacklogModalEscReturnsToBacklog(t *testing.T) {
	m := Model{State: backlogFixtureState()}
	m = pressKey(t, m, "b")
	m = pressKey(t, m, "n")
	m = pressKey(t, m, "esc")
	if m.modal != nil {
		t.Fatal("esc did not close the modal")
	}
	if m.backlog == nil {
		t.Fatal("esc from a backlog-opened modal landed on the grid")
	}
}

// A successful create lands the operator back in the backlog they filed from.
func TestBacklogModalSubmitReturnsToBacklog(t *testing.T) {
	m := Model{State: backlogFixtureState()}
	m = pressKey(t, m, "b")
	m = pressKey(t, m, "n")
	next, _ := m.Update(createIssueMsg{response: proto.Response{OK: true}})
	m = next.(Model)
	if m.modal != nil {
		t.Fatal("a successful create left the modal open")
	}
	if m.backlog == nil {
		t.Fatal("a successful create from the backlog landed on the grid")
	}
}

// With no daemon attached the submit short-circuits before any command is sent.
// That branch owes the return rule too — for a draft filed from the backlog and
// for an edit of an existing one.
func TestNilClientSubmitRestoresBacklog(t *testing.T) {
	t.Run("from backlog", func(t *testing.T) {
		m := Model{State: backlogFixtureState()}
		m = pressKey(t, m, "b")
		m = pressKey(t, m, "n")
		m.modal.Title = "a new draft"
		m = pressKey(t, m, "ctrl+s")
		if m.modal != nil {
			t.Fatal("clientless submit left the modal open")
		}
		if m.backlog == nil {
			t.Fatal("clientless submit from the backlog landed on the grid")
		}
	})
	t.Run("editing a draft", func(t *testing.T) {
		m := Model{State: backlogFixtureState()}
		m = pressKey(t, m, "b")
		m = pressKey(t, m, "enter")
		if m.modal == nil || m.modal.EditID == "" {
			t.Fatalf("enter did not open an edit modal: %+v", m.modal)
		}
		m = pressKey(t, m, "ctrl+s")
		if m.modal != nil {
			t.Fatal("clientless submit left the modal open")
		}
		if m.backlog == nil {
			t.Fatal("clientless submit of an edit landed on the grid")
		}
	})
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
	if m.toastSel != 3 {
		t.Fatalf("after j: toastSel = %d, want Add note index 3", m.toastSel)
	}
	m = pressKey(t, m, "j")
	if m.toastSel != 3 {
		t.Fatalf("j did not clamp at Add note: %d", m.toastSel)
	}
	m = pressKey(t, m, "up")
	if m.toastSel != 2 {
		t.Fatalf("after up: toastSel = %d, want 2", m.toastSel)
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

func TestChoiceToastAddNoteOpensEmptyEditor(t *testing.T) {
	m := toastModel(t)
	m = pressKey(t, m, "j")
	m = pressKey(t, m, "j")
	m = pressKey(t, m, "j")
	if m.toastSel != len(m.Toast.Options) {
		t.Fatalf("selection = %d, want Add note index %d", m.toastSel, len(m.Toast.Options))
	}
	m = pressKey(t, m, "enter")
	if m.decisionEditor == nil || m.decisionEditor.Value != "" {
		t.Fatalf("editor = %#v", m.decisionEditor)
	}
}

func TestDecisionSurfacesFitViewportAndKeepChrome(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	for _, tc := range []struct {
		name   string
		editor bool
	}{
		{name: "card"},
		{name: "editor", editor: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := FixtureModel("decision", 100, 40)
			if m.Toast == nil {
				t.Fatal("decision fixture did not raise a toast")
			}
			if tc.editor {
				m.decisionEditor = &decisionEditor{
					DecisionID: m.Toast.ID,
					Value:      "A response that stays inside the editor",
				}
			}
			base := m
			base.Toast = nil
			base.decisionEditor = nil
			basePlain := ansi.Strip(base.View())
			if !strings.Contains(basePlain, "floors") {
				t.Fatalf("base keybar moved or disappeared:\n%s", basePlain)
			}

			rendered := ansi.Strip(m.View())
			if got := lipgloss.Height(rendered); got != 40 {
				t.Fatalf("rendered height = %d, want 40", got)
			}
			plain := strings.TrimRight(rendered, "\n")
			lines := strings.Split(plain, "\n")
			if !strings.Contains(lines[0], "1 question for you") {
				t.Fatalf("header moved or disappeared: %q", lines[0])
			}
			want := "DECISION 1"
			if tc.editor {
				want = "RESPONSE 1"
			}
			if !strings.Contains(plain, want) {
				t.Fatalf("visible surface %q missing from view:\n%s", want, plain)
			}
		})
	}
}

func TestDecisionEditorEscReturnsToCardOverlay(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	m := FixtureModel("decision", 100, 40)
	if m.Toast == nil {
		t.Fatal("decision fixture did not raise a toast")
	}
	m.decisionEditor = &decisionEditor{DecisionID: m.Toast.ID, Value: "draft response"}

	m = pressKey(t, m, "esc")
	if m.decisionEditor != nil {
		t.Fatal("esc left the response editor open")
	}
	if m.Toast == nil {
		t.Fatal("esc dismissed the card instead of returning to it")
	}
	plain := ansi.Strip(m.View())
	if !strings.Contains(plain, "DECISION 1") || strings.Contains(plain, "RESPONSE 1") {
		t.Fatalf("view did not return to the decision card:\n%s", plain)
	}
	if got := lipgloss.Height(plain); got != 40 {
		t.Fatalf("card height after esc = %d, want 40", got)
	}
}

func TestDecisionAnswerResponsesKeepSurfaceContract(t *testing.T) {
	base := FixtureModel("decision", 100, 40)
	if base.Toast == nil {
		t.Fatal("decision fixture did not raise a toast")
	}
	base.decisionEditor = &decisionEditor{DecisionID: base.Toast.ID, Value: "draft response"}

	succeeded, _ := base.Update(answerMsg{
		decisionID: base.Toast.ID,
		response:   proto.Response{OK: true},
	})
	successModel := succeeded.(Model)
	if successModel.Toast != nil || successModel.decisionEditor != nil {
		t.Fatalf("successful answer kept active surface: toast=%+v editor=%+v",
			successModel.Toast, successModel.decisionEditor)
	}

	failed, _ := base.Update(answerMsg{
		decisionID: base.Toast.ID,
		response:   proto.Response{Error: "answer rejected"},
	})
	failureModel := failed.(Model)
	if failureModel.Toast == nil || failureModel.decisionEditor == nil {
		t.Fatalf("failed answer discarded active surface: toast=%+v editor=%+v",
			failureModel.Toast, failureModel.decisionEditor)
	}
	if failureModel.Err != "answer rejected" {
		t.Fatalf("failure error = %q, want answer rejected", failureModel.Err)
	}
}

func TestShelfAutoRetiresAndUnretiresMergedIssue(t *testing.T) {
	m := NewModel(nil, []string{"spec", "merge"})
	m.State.Issues["GH-1"] = &projection.IssueView{ID: "GH-1", Title: "shipped", Merged: true, MergedAt: time.Now().Add(-2 * time.Minute)}
	m.State.Shipped = []string{"GH-1"}
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
		m.State.Shipped = append(m.State.Shipped, id)
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

// A stale offset on reopen is the obvious regression, and the zero streamState
// means detached at row 0 rather than following — so popMode has to reset it.
func TestPopModeResetsStreamState(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	)
	m = pressKey(t, m, "T")
	if !m.stream.Follow {
		t.Fatalf("T opened the door detached: %+v", m.stream)
	}
	m.doorLines = deepStream(200)
	m = pressKey(t, m, "k")
	if m.stream.Follow || m.stream.Top == 0 {
		t.Fatalf("k did not detach: %+v", m.stream)
	}
	m = pressKey(t, m, "esc")
	if (m.stream != streamState{Top: 0, Follow: true}) {
		t.Fatalf("popMode left a stale offset: %+v", m.stream)
	}
	m = pressKey(t, m, "T")
	if (m.stream != streamState{Top: 0, Follow: true}) {
		t.Fatalf("reopened door = %+v, want {Top:0 Follow:true}", m.stream)
	}
}

// Inside the door j/k/d/u/g/G are the door's; re-arming follow refreshes at
// once rather than waiting up to a tick.
func TestTranscriptScrollKeys(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	)
	m = pressKey(t, m, "T")
	m.doorLines = deepStream(200)
	m.Width, m.Height = 100, 40

	m = pressKey(t, m, "k")
	if m.stream.Follow || m.stream.Top == 0 {
		t.Fatalf("k did not detach: %+v", m.stream)
	}
	up := m.stream.Top
	m = pressKey(t, m, "down")
	if m.stream.Top != up+1 {
		t.Fatalf("down did not behave as j: %d then %d", up, m.stream.Top)
	}
	m = pressKey(t, m, "g")
	if m.stream.Top != 0 || m.stream.Follow {
		t.Fatalf("g did not reach the oldest row: %+v", m.stream)
	}

	// fetchTranscript returns nil without a client, and the package has no stub,
	// so the re-arm assertion needs a client value. proto.Client's fields are all
	// unexported and none is set; the returned closure is never invoked.
	m.client = &proto.Client{}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	m = next.(Model)
	if !m.stream.Follow {
		t.Fatalf("G did not re-attach: %+v", m.stream)
	}
	if cmd == nil {
		t.Fatal("re-arming follow issued no fetch")
	}
}

// One predicate at both refetch sites, so the two cannot disagree. Batched
// tea.Cmds are opaque, so the predicate is asserted directly.
func TestFollowingTranscriptGatesRefetch(t *testing.T) {
	m := Model{}
	if m.followingTranscript() {
		t.Fatal("no mode must not fetch")
	}
	m.modes = []string{"timeline"}
	m.stream = streamState{Follow: true}
	if m.followingTranscript() {
		t.Fatal("timeline mode must not fetch the transcript")
	}
	m.modes = []string{"transcript"}
	if !m.followingTranscript() {
		t.Fatal("a following transcript door must keep fetching")
	}
	m.stream = streamState{Top: 10}
	if m.followingTranscript() {
		t.Fatal("a held transcript door must not fetch")
	}
}

// The gate stops new fetches; this drops the one already in flight when the
// operator scrolled up, which would otherwise shift the held window once.
func TestHeldTranscriptDropsLateFetch(t *testing.T) {
	m := Model{modes: []string{"transcript"}, stream: streamState{Top: 10},
		doorLines: []string{"brainstorm │ held"}}
	next, _ := m.Update(transcriptMsg{lines: []string{"brainstorm │ fresh"}})
	if got := next.(Model).doorLines; len(got) != 1 || got[0] != "brainstorm │ held" {
		t.Fatalf("held door took a late fetch: %v", got)
	}

	// Nothing to hold: the lines are accepted.
	empty := Model{modes: []string{"transcript"}, stream: streamState{Top: 10}}
	next, _ = empty.Update(transcriptMsg{lines: []string{"brainstorm │ fresh"}})
	if got := next.(Model).doorLines; len(got) != 1 || got[0] != "brainstorm │ fresh" {
		t.Fatalf("empty door refused the first fetch: %v", got)
	}

	// The error branch is untouched.
	bad := Model{modes: []string{"transcript"}, stream: streamState{Top: 10},
		doorLines: []string{"brainstorm │ held"}}
	next, _ = bad.Update(transcriptMsg{err: errors.New("boom")})
	if next.(Model).Err != "boom" {
		t.Fatalf("error branch = %q", next.(Model).Err)
	}
}

// The height argument has to actually reach renderStreamDoor, and the whole
// screen has to fit the terminal — bubbletea keeps only the last r.height lines
// when a view overflows, so the header row disappears with no error at all.
func TestViewPlumbsHeightIntoStreamDoor(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	bodyRows := func(height int) int {
		m := FixtureModel("stream-long", 200, height)
		return strings.Count(ansi.Strip(m.View()), "brainstorm │")
	}
	short, tall := bodyRows(24), bodyRows(60)
	if tall <= short {
		t.Fatalf("taller terminal showed no more rows: %d at 60 vs %d at 24", tall, short)
	}
	for _, size := range [][2]int{{200, 50}, {100, 40}} {
		// stream-long clips at both golden sizes, so the screen fills the
		// terminal exactly rather than merely fitting under it. If this ever
		// reads as inequality, the ten-row chrome budget has drifted.
		m := FixtureModel("stream-long", size[0], size[1])
		if got := lipgloss.Height(m.View()); got != size[1] {
			t.Fatalf("stream-long at %dx%d rendered %d rows, want %d", size[0], size[1], got, size[1])
		}
		// The short fixture does not clip, so the door hugs its content.
		hugs := FixtureModel("stream", size[0], size[1])
		if got := lipgloss.Height(hugs.View()); got > size[1] {
			t.Fatalf("stream at %dx%d rendered %d rows, want <= %d", size[0], size[1], got, size[1])
		}
		for _, m := range []Model{m, hugs} {
			requireHeaderRow(t, m, size)
		}
	}
}

// requireHeaderRow is the observable form of "the screen fits the terminal":
// bubbletea keeps only the last r.height lines of an over-tall view, so the
// header is what overflow eats first, and it goes with no error at all.
func requireHeaderRow(t *testing.T, m Model, size [2]int) {
	t.Helper()
	first, _, _ := strings.Cut(ansi.Strip(m.View()), "\n")
	if !strings.Contains(first, "1 question for you") {
		t.Fatalf("header row lost off the top at %dx%d: %q", size[0], size[1], first)
	}
}

// The ten-row chrome budget spends exactly one row on the keybar, and the
// keybar's right slot is m.Err — which carries err.Error() from the daemon,
// where an error wrapping a command's CombinedOutput is routinely multi-line.
// A fetch failure while following is the case spec.md names, and it is also
// the one that would push the header off the top with no error at all.
func TestStreamDoorFitsTerminalWithMultiLineError(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	for _, size := range [][2]int{{200, 50}, {100, 40}} {
		m := FixtureModel("stream-long", size[0], size[1])
		m.Err = "git worktree add failed:\nfatal: destination path exists\nhint: retry with --force"
		if got := lipgloss.Height(m.View()); got != size[1] {
			t.Fatalf("stream-long with a multi-line error at %dx%d rendered %d rows, want %d",
				size[0], size[1], got, size[1])
		}
		requireHeaderRow(t, m, size)
	}
}

// shelfMerged is the fixed merge instant the shelf tests hang their cutoffs off.
// Explicitly UTC so the day boundary does not depend on the machine's zone.
var shelfMerged = time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)

// shippedShelfModel poses one retired, merged lane. Retired because shelfItems
// only lists shipped lanes that have already left the grid.
func shippedShelfModel(t *testing.T, mergedAt time.Time) Model {
	t.Helper()
	m := NewModel(nil, []string{"spec", "merge"})
	m.State.Issues["GH-1"] = &projection.IssueView{
		ID: "GH-1", Title: "shipped", State: "done", Merged: true, MergedAt: mergedAt}
	m.State.Shipped = []string{"GH-1"}
	m.retired["GH-1"] = true
	return m
}

// The shelf covers merges at or after local midnight and nothing earlier.
func TestShelfItemsScopesShippedToToday(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dayStart time.Time
		want     int
	}{
		{"merged today", core.StartOfDay(shelfMerged), 1},
		{"merged before midnight", core.StartOfDay(shelfMerged.Add(24 * time.Hour)), 0},
		{"merged exactly at the cutoff", shelfMerged, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := shippedShelfModel(t, shelfMerged)
			m.dayStart = tc.dayStart
			if got := m.shelfItems(); len(got) != tc.want {
				t.Fatalf("shelf items = %d (%+v), want %d", len(got), got, tc.want)
			}
		})
	}
}

// A zero MergedAt fails open and stays on the shelf.
func TestShelfItemsKeepsShippedLaneWithZeroMergedAt(t *testing.T) {
	m := shippedShelfModel(t, time.Time{})
	m.dayStart = core.StartOfDay(shelfMerged)
	if got := m.shelfItems(); len(got) != 1 || got[0].ID != "GH-1" {
		t.Fatalf("zero MergedAt was filtered out: %+v", got)
	}
}

// One state, evaluated either side of a midnight rollover.
func TestShelfClearsAcrossMidnightRollover(t *testing.T) {
	m := shippedShelfModel(t, shelfMerged)
	m.dayStart = core.StartOfDay(shelfMerged)
	if got := m.shelfItems(); len(got) != 1 {
		t.Fatalf("before rollover: shelf items = %d, want 1", len(got))
	}
	m.dayStart = core.StartOfDay(shelfMerged.Add(24 * time.Hour))
	if got := m.shelfItems(); len(got) != 0 {
		t.Fatalf("after rollover: shelf items = %d (%+v), want 0", len(got), got)
	}
}

// staleGridModel poses one lane merged just before midnight, with `now` just
// after it, so retireAfter has deliberately not elapsed.
func staleGridModel(t *testing.T) (Model, time.Time) {
	t.Helper()
	now := time.Date(2026, 7, 31, 0, 0, 20, 0, time.UTC)
	mergedAt := time.Date(2026, 7, 30, 23, 59, 50, 0, time.UTC)
	m := NewModel(nil, []string{"spec", "merge"})
	m.SetRetireAfter(5 * time.Minute)
	m.State.Order = []string{"GH-1"}
	m.State.Issues["GH-1"] = &projection.IssueView{
		ID: "GH-1", Title: "shipped", State: "done", Merged: true, MergedAt: mergedAt}
	m.State.Shipped = []string{"GH-1"}
	m.dayStart = core.StartOfDay(now)
	if !now.Before(mergedAt.Add(m.retireAfter)) {
		t.Fatal("setup no longer discriminates: retireAfter has already elapsed, " +
			"so the existing timer would retire this lane on its own")
	}
	return m, now
}

// Staleness retires a lane the retireAfter timer has not reached.
func TestAutoRetireRetiresStaleShippedBeforeRetireAfter(t *testing.T) {
	m, now := staleGridModel(t)
	m.autoRetire(now)
	if !m.retired["GH-1"] {
		t.Fatal("lane merged before local midnight was not retired")
	}
}

// The stale lane leaves the grid, not only the shelf.
func TestVisibleOrderDropsStaleShippedLane(t *testing.T) {
	m, now := staleGridModel(t)
	if got := visibleOrder(m.State, m.retired); len(got) != 1 {
		t.Fatalf("before autoRetire: visibleOrder = %v, want [GH-1]", got)
	}
	m.autoRetire(now)
	if got := visibleOrder(m.State, m.retired); len(got) != 0 {
		t.Fatalf("stale lane still on the grid: %v", got)
	}
	if got := m.shelfItems(); len(got) != 0 {
		t.Fatalf("stale lane still on the shelf: %+v", got)
	}
}

// retireAfter is unreachably non-positive in production, which is what makes
// autoRetire's retained `retireAfter <= 0` early return harmless.
// This pins existing behavior; it passes before the change too.
func TestRetireAfterStaysPositive(t *testing.T) {
	m := NewModel(nil, []string{"spec", "merge"})
	if m.retireAfter <= 0 {
		t.Fatalf("NewModel retireAfter = %v, want positive", m.retireAfter)
	}
	before := m.retireAfter
	m.SetRetireAfter(0)
	if m.retireAfter != before {
		t.Fatalf("SetRetireAfter(0) changed retireAfter to %v, want %v", m.retireAfter, before)
	}
	m.SetRetireAfter(-time.Hour)
	if m.retireAfter != before {
		t.Fatalf("SetRetireAfter(-1h) changed retireAfter to %v, want %v", m.retireAfter, before)
	}
}

// The tick assigns dayStart from a single clock read and does so before
// autoRetire, so a stale lane is retired on that same tick.
func TestTickRefreshesDayStartBeforeRetiring(t *testing.T) {
	m := NewModel(nil, []string{"spec", "merge"})
	m.SetRetireAfter(1000 * time.Hour) // the timer can never fire here
	m.State.Order = []string{"GH-1"}
	m.State.Issues["GH-1"] = &projection.IssueView{
		ID: "GH-1", Title: "shipped", State: "done", Merged: true,
		MergedAt: time.Now().Add(-24 * time.Hour)}
	m.State.Shipped = []string{"GH-1"}

	before := core.StartOfDay(time.Now())
	next, _ := m.Update(tickMsg{})
	after := core.StartOfDay(time.Now())

	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", next)
	}
	if !got.dayStart.Equal(before) && !got.dayStart.Equal(after) {
		t.Fatalf("dayStart = %v, want %v or %v", got.dayStart, before, after)
	}
	if !got.retired["GH-1"] {
		t.Fatal("stale lane was not retired on the tick that set dayStart — " +
			"dayStart must be assigned before autoRetire runs")
	}
	if len(got.shelfItems()) != 0 {
		t.Fatalf("stale lane still on the shelf: %+v", got.shelfItems())
	}
}

// A lane that took a final stage failure (or was killed) and later merged
// sits in both State.Parked and State.Shipped — GH-6 in the live store is
// exactly this shape. Once stale it must leave the shelf outright, not slide
// from SHIPPED today into PARKED.
func TestStaleShippedDoesNotResurfaceAsParked(t *testing.T) {
	m := shippedShelfModel(t, shelfMerged)
	m.State.Parked = []string{"GH-1"}
	m.dayStart = core.StartOfDay(shelfMerged.Add(24 * time.Hour))
	if got := m.shelfItems(); len(got) != 0 {
		t.Fatalf("stale shipped lane resurfaced on the shelf: %+v", got)
	}
}
