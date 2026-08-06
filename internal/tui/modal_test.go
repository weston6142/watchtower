package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/weston6142/watchtower/internal/projection"
)

func TestModalTyping(t *testing.T) {
	m := modalState{Preset: "regular", FlowName: "default"}
	for _, r := range "add rate limiting" {
		m = m.input(string(r))
	}
	m = m.input("backspace")
	if m.Title != "add rate limitin" {
		t.Fatalf("title: %q", m.Title)
	}
	lipgloss.SetColorProfile(termenv.Ascii)
	out := renderModal(m, 70)
	if !strings.Contains(out, "add rate limitin") || !strings.Contains(out, "regular") {
		t.Fatalf("modal:\n%s", out)
	}
}

func TestModalEditorInsertsAndBackspacesByRune(t *testing.T) {
	e := newModalEditor("a界c")
	if e.Caret != 3 {
		t.Fatalf("initial caret = %d, want 3", e.Caret)
	}
	if !e.handle("left") || !e.handle("left") {
		t.Fatal("left must be consumed by the editor")
	}
	if e.Caret != 1 {
		t.Fatalf("caret after left twice = %d, want 1", e.Caret)
	}
	if !e.handle("é") || e.Value != "aé界c" || e.Caret != 2 {
		t.Fatalf("rune insertion = value %q caret %d, want aé界c/2", e.Value, e.Caret)
	}
	if !e.handle("backspace") || e.Value != "a界c" || e.Caret != 1 {
		t.Fatalf("rune backspace = value %q caret %d, want a界c/1", e.Value, e.Caret)
	}
	if !e.handle("backspace") || e.Value != "界c" || e.Caret != 0 {
		t.Fatalf("second backspace = value %q caret %d, want 界c/0", e.Value, e.Caret)
	}
	if !e.handle("backspace") || e.Value != "界c" || e.Caret != 0 {
		t.Fatalf("start backspace changed editor: value %q caret %d", e.Value, e.Caret)
	}
	if !e.handle("left") || !e.handle("left") || e.Caret != 0 {
		t.Fatalf("left boundary escaped editor: caret %d", e.Caret)
	}
	if !e.handle("right") || !e.handle("right") || e.Caret != 2 {
		t.Fatalf("right boundary escaped editor: caret %d", e.Caret)
	}
}

func TestModalEditorMovesVerticallyAndOwnsBoundaries(t *testing.T) {
	e := newModalEditor("ab\nlonger\nx")
	if !e.handle("up") || e.Caret != 4 {
		t.Fatalf("up from final line = %d, want 4", e.Caret)
	}
	if !e.handle("up") || e.Caret != 1 {
		t.Fatalf("up to first line = %d, want 1", e.Caret)
	}
	if !e.handle("up") || e.Caret != 1 {
		t.Fatalf("first-line up escaped editor: caret %d", e.Caret)
	}
	if !e.handle("down") || e.Caret != 4 {
		t.Fatalf("down to longer line = %d, want 4", e.Caret)
	}
	if !e.handle("down") || e.Caret != 11 {
		t.Fatalf("down to short line should clamp to its end, caret %d", e.Caret)
	}
	if !e.handle("down") || e.Caret != 11 {
		t.Fatalf("last-line down escaped editor: caret %d", e.Caret)
	}
	if !e.handle("tab") || e.Caret != 11 {
		t.Fatalf("tab must not move the editor caret: %d", e.Caret)
	}
}

func TestRenderBoxChrome(t *testing.T) {
	out := ansi.Strip(renderBox("confirm", "", " n cancel ", "really?"))
	for _, want := range []string{"confirm", "n cancel", "really?", "┌", "└"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestLeverEditorCycles(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	out := renderLeverEditor([]string{"spec", "execute"}, map[string]string{"spec": "strict", "execute": "yolo"}, 1)
	if !strings.Contains(out, "spec") || !strings.Contains(out, "yolo") || !strings.Contains(out, "▸") {
		t.Fatalf("editor:\n%s", out)
	}
}

func TestModalPriorityField(t *testing.T) {
	m := modalState{}
	for i := 0; i < priorityField; i++ {
		m = m.input("tab")
	}
	if m.Field != priorityField {
		t.Fatalf("Field = %d, want %d (priority)", m.Field, priorityField)
	}
	m = m.input("tab")
	if m.Field != 0 {
		t.Fatalf("tab wrap: Field = %d, want 0", m.Field)
	}
}

func TestModalDependencyFieldTakesCommaSeparatedIDs(t *testing.T) {
	m := modalState{Field: dependenciesField}
	for _, r := range "GH-1, GH-2" {
		m = m.input(string(r))
	}
	if m.DependsOn != "GH-1, GH-2" {
		t.Fatalf("DependsOn = %q", m.DependsOn)
	}
	out := ansi.Strip(renderModal(m, 80))
	if !strings.Contains(out, "DEPENDS ON") || !strings.Contains(out, "GH-1, GH-2") {
		t.Fatalf("dependency field missing:\n%s", out)
	}
}

// The field shows the level name, not a number, and advertises the adjust keys
// so the option set is not something the operator has to guess.
func TestRenderModalShowsPriorityLevel(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	out := ansi.Strip(renderModal(modalState{Field: priorityField, Priority: 2}, 80))
	for _, want := range []string{"urgent", "h/l", "adjust", "\u25c2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// An out-of-set priority renders as its number, and the padding keeps the title
// column fixed — padCell truncates, so the label must be padded before styling.
func TestBacklogRendersOutOfSetPriority(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	row := func(p int) string {
		out := ansi.Strip(renderBacklog([]*projection.IssueView{
			{ID: "GH-9", Title: "stale", Priority: p}}, 0, 80, 40))
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "GH-9") {
				return line
			}
		}
		t.Fatalf("no GH-9 row in:\n%s", out)
		return ""
	}
	odd := row(5)
	if !strings.Contains(odd, "5") || strings.Contains(odd, "p5") {
		t.Fatalf("out-of-set priority not shown bare: %q", odd)
	}
	if got, want := strings.Index(odd, "stale"), strings.Index(row(0), "stale"); got != want {
		t.Fatalf("title column moved: %d vs %d\n%q\n%q", got, want, odd, row(0))
	}
}

func TestRenderModalHints(t *testing.T) {
	create := renderModal(modalState{}, 80)
	if !strings.Contains(create, "ctrl+s") || !strings.Contains(create, "create") {
		t.Fatalf("create-mode hints missing:\n%s", create)
	}
	edit := renderModal(modalState{EditID: "GH-1", Title: "t"}, 80)
	if !strings.Contains(edit, "save") || strings.Contains(edit, "create") {
		t.Fatalf("edit-mode hints wrong:\n%s", edit)
	}
	if !strings.Contains(edit, "edit issue") {
		t.Fatalf("edit-mode title wrong:\n%s", edit)
	}
}

// The direct guard against the index-shift bug: text typed at attachField must
// land in Attach and must not touch the priority selector.
func TestModalAttachFieldTakesRunes(t *testing.T) {
	m := modalState{}
	for i := 0; i < attachField; i++ {
		m = m.input("tab")
	}
	if m.Field != attachField {
		t.Fatalf("Field = %d, want %d", m.Field, attachField)
	}
	for _, r := range "/tmp/app.log" {
		m = m.input(string(r))
	}
	if m.Attach != "/tmp/app.log" {
		t.Fatalf("Attach = %q", m.Attach)
	}
	if m.Priority != 0 {
		t.Fatalf("typing in attach moved Priority to %d", m.Priority)
	}
	m = m.input("backspace")
	if m.Attach != "/tmp/app.lo" {
		t.Fatalf("backspace: Attach = %q", m.Attach)
	}
}

// The modal.go invariant: priority has no case in setFieldValue/fieldValue, so
// rune input physically cannot reach it. Fields are compared one by one because
// OrigAttach []string makes modalState non-comparable with ==.
func TestModalPriorityStillRejectsRunes(t *testing.T) {
	got := modalState{Field: priorityField}.input("x")
	if got.Title != "" || got.Body != "" || got.FlowName != "" || got.Preset != "" ||
		got.Attach != "" || got.Priority != 0 || got.Field != priorityField {
		t.Fatalf("rune input reached the priority field: %+v", got)
	}
	// h/l cycling lives in the key router (Model.Update), not in input; it is
	// already covered by TestModalPriorityCycles in app_test.go, which tabs via
	// the priorityField constant and so follows the shift for free.
}

func TestRenderModalShowsAttachField(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	out := ansi.Strip(renderModal(modalState{Field: attachField, Attach: "/tmp/app.log"}, 80))
	for _, want := range []string{"ATTACH", "/tmp/app.log"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
