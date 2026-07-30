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
	for i := 0; i < 5; i++ {
		m = m.input("tab")
	}
	if m.Field != 5 {
		t.Fatalf("Field = %d, want 5 (priority)", m.Field)
	}
	m = m.input("tab")
	if m.Field != 0 {
		t.Fatalf("tab wrap: Field = %d, want 0", m.Field)
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

// The direct guard against the index-shift bug: text typed at index 4 must
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
