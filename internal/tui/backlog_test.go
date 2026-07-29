package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/projection"
)

// backlogDrafts builds n drafts whose titles are unique substrings, so a test
// can assert which rows the scroll window is showing.
func backlogDrafts(n int) []*projection.IssueView {
	entries := make([]*projection.IssueView, n)
	for i := range entries {
		entries[i] = &projection.IssueView{
			ID:    fmt.Sprintf("GH-%d", i),
			Title: fmt.Sprintf("draft<%d>", i),
			Body:  fmt.Sprintf("body<%d>", i),
		}
	}
	return entries
}

// The backlog is a browsing surface, not a prompt: it grows into the terminal
// instead of hugging its content. It still has to stay strictly inside the
// terminal, or overlayCenter drops the dimmed base and the overlay look with it.
func TestBacklogFillsTerminalWidth(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	entries := backlogDrafts(4)
	narrow := lipgloss.Width(renderBacklog(entries, 0, 80, 40))
	wide := lipgloss.Width(renderBacklog(entries, 0, 160, 40))
	if wide <= narrow {
		t.Fatalf("box did not grow with the terminal: %d cols at 160 vs %d at 80", wide, narrow)
	}
	if wide < 120 {
		t.Fatalf("box only %d cols of a 160-col terminal", wide)
	}
	if wide >= 160 {
		t.Fatalf("box %d cols overflows a 160-col terminal", wide)
	}
	if narrow >= 80 {
		t.Fatalf("box %d cols overflows an 80-col terminal", narrow)
	}
}

// Width is viewport-driven, so two backlogs with wildly different title lengths
// occupy the same frame — the box must not shrink back to its content.
func TestBacklogWidthIndependentOfContent(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	short := renderBacklog([]*projection.IssueView{{ID: "GH-1", Title: "x"}}, 0, 140, 40)
	long := renderBacklog([]*projection.IssueView{
		{ID: "GH-1", Title: strings.Repeat("long ", 20)}}, 0, 140, 40)
	if got, want := lipgloss.Width(short), lipgloss.Width(long); got != want {
		t.Fatalf("box width tracks content: %d cols short vs %d cols long", got, want)
	}
}

// A full queue of drafts uses the vertical space too, and stays inside it.
func TestBacklogFillsTerminalHeight(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	entries := backlogDrafts(40)
	short := lipgloss.Height(renderBacklog(entries, 0, 120, 20))
	tall := lipgloss.Height(renderBacklog(entries, 0, 120, 48))
	if tall <= short {
		t.Fatalf("box did not grow with the terminal: %d rows at 48 vs %d at 20", tall, short)
	}
	if tall >= 48 {
		t.Fatalf("box %d rows overflows a 48-row terminal", tall)
	}
	if short >= 20 {
		t.Fatalf("box %d rows overflows a 20-row terminal", short)
	}
}

// More drafts than rows: the window follows the cursor instead of clipping it.
func TestBacklogScrollsSelectionIntoView(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	entries := backlogDrafts(40)
	last := ansi.Strip(renderBacklog(entries, 39, 100, 20))
	if !strings.Contains(last, "draft<39>") {
		t.Fatalf("selected last draft not in window:\n%s", last)
	}
	if strings.Contains(last, "draft<0>") {
		t.Fatalf("window did not scroll off the first draft:\n%s", last)
	}
	first := ansi.Strip(renderBacklog(entries, 0, 100, 20))
	if !strings.Contains(first, "draft<0>") {
		t.Fatalf("selected first draft not in window:\n%s", first)
	}
	if strings.Contains(first, "draft<39>") {
		t.Fatalf("window showed the whole list in 20 rows:\n%s", first)
	}
}

// A clipped list says so, so the operator knows drafts exist off-window.
func TestBacklogReportsHiddenDrafts(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	out := ansi.Strip(renderBacklog(backlogDrafts(40), 0, 100, 20))
	if !strings.Contains(out, "40") {
		t.Fatalf("no draft count in:\n%s", out)
	}
}

// The overlay is rendered before the first WindowSizeMsg lands, so a zero
// height must fall back rather than collapse the box to nothing.
func TestBacklogZeroHeightFallsBack(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	out := renderBacklog(backlogDrafts(10), 0, 120, 0)
	if got := lipgloss.Height(out); got < 10 {
		t.Fatalf("zero height collapsed the box to %d rows:\n%s", got, out)
	}
}

// A terminal too small for the frame still renders the selected row instead of
// panicking on a negative width.
func TestBacklogSurvivesTinyTerminal(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	out := ansi.Strip(renderBacklog(backlogDrafts(5), 4, 20, 6))
	if !strings.Contains(out, "draft<4>") {
		t.Fatalf("selected row missing in a tiny terminal:\n%s", out)
	}
}

// The screenspace buys a detail pane: the selected draft's body and metadata
// were previously invisible until you pressed enter to edit it.
func TestBacklogShowsSelectedDetail(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	entries := []*projection.IssueView{
		{ID: "GH-1", Title: "first", Body: "body-of-first", Flow: "default", Preset: "regular"},
		{ID: "GH-2", Title: "second", Body: "body-of-second", Flow: "hotfix", Preset: "strict"},
	}
	out := ansi.Strip(renderBacklog(entries, 1, 140, 40))
	for _, want := range []string{"second", "body-of-second", "hotfix", "strict"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "body-of-first") {
		t.Fatalf("detail pane showed an unselected draft's body:\n%s", out)
	}
}

// Too narrow for two panes: the list survives, the detail pane yields.
func TestBacklogNarrowTerminalDropsDetailPane(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	entries := []*projection.IssueView{
		{ID: "GH-1", Title: "first", Body: "body-of-first"},
		{ID: "GH-2", Title: "second", Body: "body-of-second"},
	}
	out := ansi.Strip(renderBacklog(entries, 1, 64, 40))
	if !strings.Contains(out, "second") {
		t.Fatalf("list dropped along with the detail pane:\n%s", out)
	}
	if strings.Contains(out, "body-of-second") {
		t.Fatalf("detail pane rendered at 64 cols:\n%s", out)
	}
}

// A body too tall for the pane says it was cut, rather than stopping mid
// sentence and passing for the whole of it.
func TestBacklogMarksClippedBody(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	entries := []*projection.IssueView{{ID: "GH-1", Title: "long one",
		Body: strings.Repeat("sentence of body prose ", 60)}}
	out := ansi.Strip(renderBacklog(entries, 0, 140, 22))
	if !strings.Contains(out, "enter to read it all") {
		t.Fatalf("clipped body not marked:\n%s", out)
	}
}

// The height argument has to actually reach renderBacklog. A zero there still
// renders a plausible box, and the fixture's two drafts make the goldens
// byte-identical either way, so only View() at two real heights catches it.
func TestViewPlumbsHeightIntoBacklog(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	visibleDrafts := func(height int) int {
		m := FixtureModel("backlog", 120, height)
		for i := 0; i < 40; i++ {
			ev, _ := core.NewEvent(core.EvIssueDrafted, fmt.Sprintf("GH-%d", 100+i),
				map[string]any{"title": fmt.Sprintf("draft<%d>", i), "body": "b",
					"flow": "default", "preset": "regular", "priority": 1})
			m.State.Apply(ev)
		}
		return strings.Count(ansi.Strip(m.View()), "draft<")
	}
	short, tall := visibleDrafts(24), visibleDrafts(60)
	if tall <= short {
		t.Fatalf("taller terminal showed no more drafts: %d at 60 rows vs %d at 24", tall, short)
	}
}

// The empty state keeps telling the operator how to file a draft.
func TestBacklogEmptyStateSurvivesResize(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	out := ansi.Strip(renderBacklog(nil, 0, 160, 48))
	if !strings.Contains(out, "backlog is empty") {
		t.Fatalf("empty state missing:\n%s", out)
	}
	if !strings.Contains(out, "ctrl+s") {
		t.Fatalf("empty state lost its instructions:\n%s", out)
	}
}
