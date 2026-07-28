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
