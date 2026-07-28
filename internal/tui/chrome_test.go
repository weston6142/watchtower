package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderChromeHeaderContainsBadgesAndRight(t *testing.T) {
	got := ansi.Strip(renderChromeHeader(120, []badge{
		{Text: "1 question for you", Kind: badgeWarn},
		{Text: "1 build failing", Kind: badgeErr},
	}, "2 building · 412k tokens"))
	for _, want := range []string{"1 question for you", "1 build failing", "412k tokens"} {
		if !strings.Contains(got, want) {
			t.Errorf("header missing %q in %q", want, got)
		}
	}
}

func TestKeybarDocksErrorRight(t *testing.T) {
	got := ansi.Strip(renderKeybar(100, [][2]string{{"j/k", "floors"}, {"?", "help"}}, errText("no pending decision 1")))
	if !strings.Contains(got, "no pending decision 1") || !strings.Contains(got, "floors") {
		t.Errorf("keybar wrong: %q", got)
	}
}

func TestCursorRow(t *testing.T) {
	sel := ansi.Strip(cursorRow(true, "hello", 40))
	if !strings.HasPrefix(sel, glyphCursor) {
		t.Errorf("selected row must start with cursor glyph: %q", sel)
	}
	unsel := ansi.Strip(cursorRow(false, "hello", 40))
	if !strings.HasPrefix(unsel, "  ") {
		t.Errorf("unselected row must keep a 2-cell gutter: %q", unsel)
	}
}
