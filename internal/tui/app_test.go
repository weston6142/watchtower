package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/projection"
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
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	next, _ := m.Update(msg)
	return next.(Model)
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
