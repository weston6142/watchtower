package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestOverlayCenterSplicesModal(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	base := strings.TrimSuffix(strings.Repeat("aaaaaaaaaa\n", 5), "\n") // 10x5
	out := overlayCenter(base, "XX\nXX", 10, 5)
	lines := strings.Split(out, "\n")
	if len(lines) != 5 {
		t.Fatalf("height = %d", len(lines))
	}
	// modal is 2x2 centered in 10x5 → x=4, y=1
	for _, row := range []int{1, 2} {
		plain := ansi.Strip(lines[row])
		if plain != "aaaaXXaaaa" {
			t.Fatalf("row %d = %q", row, plain)
		}
	}
	if ansi.Strip(lines[0]) != "aaaaaaaaaa" {
		t.Fatalf("row 0 = %q", ansi.Strip(lines[0]))
	}
	// base rows outside the modal are dimmed (faint SGR present)
	if !strings.Contains(lines[0], "\x1b[2m") {
		t.Fatalf("row 0 not dimmed: %q", lines[0])
	}
}

func TestOverlayCenterShortBase(t *testing.T) {
	// base shorter than height gets padded before splicing
	out := overlayCenter("aaaa", "XX", 10, 5)
	if got := len(strings.Split(out, "\n")); got != 5 {
		t.Fatalf("height = %d", got)
	}
}

func TestOverlayCenterModalTooBig(t *testing.T) {
	// modal wider than the screen degrades to a plain centered render
	out := overlayCenter("aa", strings.Repeat("X", 30), 10, 3)
	if !strings.Contains(ansi.Strip(out), "XXXX") {
		t.Fatalf("modal content lost: %q", out)
	}
}
