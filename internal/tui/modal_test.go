package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
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
	for i := 0; i < 4; i++ {
		m = m.input("tab")
	}
	if m.Field != 4 {
		t.Fatalf("Field = %d, want 4 (priority)", m.Field)
	}
	m = m.input("tab")
	if m.Field != 0 {
		t.Fatalf("tab wrap: Field = %d, want 0", m.Field)
	}
}

// Priority is a fixed set, so the field is a selector: h/l pick an option,
// clamped at both ends, and stray text can never land a bad value in it.
func TestModalPriorityIsSelector(t *testing.T) {
	m := modalState{Field: 4}
	for _, key := range []string{"7", "x", "backspace"} {
		m = m.input(key)
		if m.Priority != "" {
			t.Fatalf("input(%q) leaked into priority: %q", key, m.Priority)
		}
	}
	m = m.input("l")
	if m.Priority != "1" {
		t.Fatalf("after l: Priority = %q, want 1", m.Priority)
	}
	for i := 0; i < 5; i++ {
		m = m.input("right")
	}
	if m.Priority != "3" {
		t.Fatalf("clamp high: Priority = %q, want 3", m.Priority)
	}
	m = m.input("h")
	if m.Priority != "2" {
		t.Fatalf("after h: Priority = %q, want 2", m.Priority)
	}
	for i := 0; i < 5; i++ {
		m = m.input("left")
	}
	if m.Priority != "0" {
		t.Fatalf("clamp low: Priority = %q, want 0", m.Priority)
	}
}

func TestRenderModalShowsPriorityOptions(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	out := ansi.Strip(renderModal(modalState{Field: 4, Priority: "2"}, 80))
	for _, want := range []string{"p0", "p1", "p2", "p3", "h/l"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
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
