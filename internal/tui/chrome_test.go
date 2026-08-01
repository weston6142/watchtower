package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
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

// The keybar is a reserved single row that every screen's height budget counts
// on — the stream door's is exact. m.Err carries err.Error() from the daemon,
// and errors that wrap a command's CombinedOutput are routinely multi-line, so
// the row has to collapse them the way renderNoticeRow already does.
func TestKeybarStaysOneRowWithMultiLineError(t *testing.T) {
	got := renderKeybar(100, [][2]string{{"j/k", "floors"}}, errText("git worktree add failed:\nfatal: destination path exists\nhint: use --force"))
	if rows := lipgloss.Height(got); rows != 1 {
		t.Fatalf("keybar rendered %d rows, want 1: %q", rows, ansi.Strip(got))
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("keybar still holds a newline: %q", ansi.Strip(got))
	}
	if plain := ansi.Strip(got); !strings.Contains(plain, "git worktree add failed: fatal: destination path exists") {
		t.Fatalf("collapsed error lost its text: %q", plain)
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
