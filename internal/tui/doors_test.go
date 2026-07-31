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

func TestDecisionsDoorSelectable(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	ds := []projection.DecisionView{
		{ID: 2, IssueID: "GH-2", Stage: "spec", Question: "big blocker?"},
		{ID: 1, IssueID: "GH-1", Stage: "merge", Question: "small one?"},
	}
	out := renderDecisionsDoor(ds, map[string]Identity{"GH-1": {Tag: "01"}, "GH-2": {Tag: "02"}}, 1, 80)
	if strings.Index(out, "big blocker?") > strings.Index(out, "small one?") {
		t.Fatal("server order not preserved")
	}
	if !strings.Contains(out, "▸") {
		t.Fatalf("no selection marker:\n%s", out)
	}
}

func TestTimelineHumanizes(t *testing.T) {
	lines := humanizeEvents([]core.Event{
		mkev(t, core.EvIssueMerged, "GH-1", map[string]any{"branch": "issue/GH-1"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "review"}),
	}, "GH-1")
	if len(lines) != 2 || !strings.Contains(lines[0], "merged") || strings.Contains(lines[1], "stage_started") {
		t.Fatalf("humanize: %v", lines)
	}
}

// The stream door is the one reading surface an operator stares at while a
// stage runs; it wears the same chrome as every other box in the room.
func TestStreamDoorWearsBoxChrome(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	lines := []string{
		"brainstorm │ Requirements settled.",
		"brainstorm │ ↳ Bash go test ./...",
		"brainstorm │ — turn complete (13560 tokens) —",
	}
	got := renderStreamDoor("GH-2 · brainstorm", lines, streamState{Follow: true}, 100, 32)
	if !strings.Contains(got, "─") {
		t.Fatalf("no border: %q", got)
	}
	if !strings.Contains(got, "stream") {
		t.Fatalf("no title: %q", got)
	}
	if !strings.Contains(got, "GH-2 · brainstorm") {
		t.Fatalf("no subtitle: %q", got)
	}
	if !strings.Contains(got, "esc close") {
		t.Fatalf("no close chip: %q", got)
	}
	if !strings.Contains(got, "Requirements settled.") {
		t.Fatalf("body missing: %q", got)
	}
	if !strings.Contains(got, "↳ Bash go test ./...") {
		t.Fatalf("tool line missing: %q", got)
	}
	if strings.Contains(got, "turn complete") {
		t.Fatalf("turn marker should render as a rule, not prose: %q", got)
	}
}

func TestStreamDoorCountsAndBrightensToolCalls(t *testing.T) {
	lines := []string{"plan │ ↳ Bash(go test ./...)", "plan │ thinking about tests"}
	out := renderStreamDoor("GH-1 · plan · 2 lines · 1 tool call", lines, streamState{Follow: true}, 100, 32)
	if !strings.Contains(out, "Bash(go test ./...)") {
		t.Fatal("tool line missing")
	}
	if !strings.Contains(out, "1 tool call") {
		t.Fatal("subtitle counts missing from box band")
	}
}

func TestStreamSubtitleCounts(t *testing.T) {
	m := Model{Focus: Focus{Issue: "GH-1"},
		doorLines: []string{"plan │ ↳ Read(a.go)", "plan │ ↳ Read(b.go)", "plan │ ok"}}
	got := m.streamSubtitle()
	if !strings.Contains(got, "3 lines") || !strings.Contains(got, "2 tool calls") {
		t.Fatalf("subtitle = %q", got)
	}
}

// An empty buffer says so rather than rendering a hollow box.
func TestStreamDoorEmpty(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	got := renderStreamDoor("GH-2", nil, streamState{Follow: true}, 100, 32)
	if !strings.Contains(got, "nothing here yet") {
		t.Fatalf("empty door = %q", got)
	}
}

// Update and View must agree about how many rows exist and how many fit, so the
// geometry lives in one helper per axis and the fallbacks live inside them.
func TestStreamGeometry(t *testing.T) {
	if got := streamInner(200); got != 192 {
		t.Fatalf("streamInner(200) = %d, want 192", got)
	}
	if got := streamInner(10); got != 20 {
		t.Fatalf("streamInner(10) = %d, want 20 (floor)", got)
	}
	for _, c := range []struct{ height, want int }{
		{50, 40}, {40, 30}, {11, 1}, {5, 1}, {0, backlogFallbackRows - streamChromeRows}, {-3, 22},
	} {
		if got := streamRows(c.height); got != c.want {
			t.Fatalf("streamRows(%d) = %d, want %d", c.height, got, c.want)
		}
	}
	if got := (Model{Width: 0}).layoutWidth(); got != 120 {
		t.Fatalf("layoutWidth(0) = %d, want 120", got)
	}
	if got := (Model{Width: 88}).layoutWidth(); got != 88 {
		t.Fatalf("layoutWidth(88) = %d, want 88", got)
	}
}

// The box frame sizes to its widest content line, so once the body is windowed
// a single over-wide or unpadded row makes the frame wobble as the window
// moves. Every row being exactly inner is the precondition that prevents it.
func TestStreamBodyRowsAreExactlyInner(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	lines := []string{
		"brainstorm │ short",
		"brainstorm │ " + strings.Repeat("prose that has to wrap ", 20),
		"brainstorm │ ↳ Bash(" + strings.Repeat("go test ./internal/tui ", 20) + ")",
		"brainstorm │ — turn complete (13560 tokens) —",
		// The longest real stage name: "merge-verification │ " is 21 cells, so
		// at inner 20 the lead alone overruns the frame.
		"merge-verification │ some prose here",
		"merge-verification │ ↳ Bash(go test ./...)",
		"no gutter at all on this one",
	}
	for _, inner := range []int{20, 92} {
		rows := streamBody(lines, inner)
		if len(rows) == 0 {
			t.Fatalf("inner %d: no rows", inner)
		}
		for i, row := range rows {
			if got := lipgloss.Width(row); got != inner {
				t.Fatalf("inner %d: row %d width = %d, want %d: %q", inner, i, got, inner, row)
			}
		}
	}
	// The empty state is prose too: it wraps to the frame instead of past it.
	for _, row := range streamBody(nil, 20) {
		if got := lipgloss.Width(row); got != 20 {
			t.Fatalf("empty-state row width = %d, want 20: %q", got, row)
		}
	}
	if first := ansi.Strip(streamBody(nil, 20)[0]); !strings.HasPrefix(first, "nothing here yet") {
		t.Fatalf("empty state first row = %q", first)
	}
}

// Follow is derived from the clamped Top, never toggled, so returning to the
// bottom by any route re-attaches and a body that fits can never detach.
func TestStreamScrollClampsAndDerivesFollow(t *testing.T) {
	for _, c := range []struct {
		name       string
		in         streamState
		key        string
		rows       int
		total      int
		wantTop    int
		wantFollow bool
	}{
		{"j from follow detaches nowhere at the bottom", streamState{Follow: true}, "j", 10, 60, 50, true},
		{"k from follow detaches", streamState{Follow: true}, "k", 10, 60, 49, false},
		{"j walks down", streamState{Top: 20}, "j", 10, 60, 21, false},
		{"k walks up", streamState{Top: 20}, "k", 10, 60, 19, false},
		{"k clamps at the oldest row", streamState{Top: 0}, "k", 10, 60, 0, false},
		{"d pages half a window", streamState{Top: 20}, "d", 10, 60, 25, false},
		{"u pages half a window", streamState{Top: 20}, "u", 10, 60, 15, false},
		{"d clamps at the newest row", streamState{Top: 48}, "d", 10, 60, 50, true},
		{"g jumps to the oldest row held", streamState{Top: 48}, "g", 10, 60, 0, false},
		{"G re-attaches", streamState{Top: 3}, "G", 10, 60, 50, true},
		{"a body that fits can never detach", streamState{Follow: true}, "k", 40, 5, 0, true},
		{"g on a body that fits still follows", streamState{Top: 0}, "g", 40, 5, 0, true},
		{"rows 0 normalises to 1", streamState{Top: 5}, "j", 0, 60, 6, false},
		{"d does not move at one row", streamState{Top: 5}, "d", 1, 60, 5, false},
		{"an unknown key only re-derives", streamState{Top: 20}, "x", 10, 60, 20, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := c.in.scroll(c.key, c.rows, c.total)
			if got.Top != c.wantTop || got.Follow != c.wantFollow {
				t.Fatalf("scroll(%q, %d, %d) = %+v, want {Top:%d Follow:%v}",
					c.key, c.rows, c.total, got, c.wantTop, c.wantFollow)
			}
		})
	}
}

// The keys are the way out of the door, so when the position will not fit
// beside them the position is what yields — exactly as backlogFooter does.
func TestStreamFooterYieldsPositionWhenNarrow(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	// inner 30: the keys (21 cells) fit, the position does not.
	narrow := streamFooter(streamState{Top: 0}, 0, 40, 60, 30)
	if strings.Contains(ansi.Strip(narrow), " of ") {
		t.Fatalf("position survived a narrow footer: %q", ansi.Strip(narrow))
	}
	if got := lipgloss.Width(narrow); got > 30 {
		t.Fatalf("narrow footer width = %d, want <= 30", got)
	}
	if !strings.Contains(ansi.Strip(narrow), "esc") {
		t.Fatalf("keys missing from narrow footer: %q", ansi.Strip(narrow))
	}
	wide := ansi.Strip(streamFooter(streamState{Top: 0}, 0, 40, 60, 92))
	if !strings.Contains(wide, "1–40 of 60") {
		t.Fatalf("wide footer = %q", wide)
	}
}

func deepStream(n int) []string {
	lines := make([]string, 0, n)
	for i := 0; i < n; i++ {
		lines = append(lines, fmt.Sprintf("brainstorm │ line<%d>", i))
	}
	return lines
}

// The inverse of the reported bug: the door opens on the newest output.
func TestStreamDoorFollowsNewestByDefault(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	got := ansi.Strip(renderStreamDoor("GH-1", deepStream(200), streamState{Follow: true}, 100, 40))
	if !strings.Contains(got, "line<199>") {
		t.Fatalf("newest line not on screen:\n%s", got)
	}
	if strings.Contains(got, "line<0>") {
		t.Fatalf("oldest line still on screen — the door is not windowed:\n%s", got)
	}
}

// Reading holds still while the stage keeps emitting. The window is what must
// be byte-identical, not the whole door: the footer's "of N" total legitimately
// grows with the transcript while the rows under the eye do not move.
func TestStreamDoorHeldWindowStaysPut(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	held := streamState{Top: 40}
	window := func(lines []string) string {
		var body []string
		for _, row := range strings.Split(ansi.Strip(renderStreamDoor("GH-1", lines, held, 100, 40)), "\n") {
			if strings.Contains(row, "line<") {
				body = append(body, row)
			}
		}
		return strings.Join(body, "\n")
	}
	before := window(deepStream(200))
	after := window(append(deepStream(200), deepStream(20)...))
	if before == "" {
		t.Fatal("no body rows matched")
	}
	if before != after {
		t.Fatalf("held window moved when lines were appended:\n%s\n---\n%s", before, after)
	}
	if !strings.Contains(before, "line<40>") || strings.Contains(before, "line<39>") {
		t.Fatalf("window does not start at Top 40:\n%s", before)
	}
}

// Without the exact-inner invariant in streamBody the frame tracks whatever the
// visible window happens to hold, and nothing else says so.
func TestStreamDoorFrameWidthHoldsWhileScrolling(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	lines := deepStream(200)
	lines[10] = "brainstorm │ ↳ Bash(" + strings.Repeat("go test ./internal/tui ", 20) + ")"
	lines[11] = "brainstorm │ " + strings.Repeat("wrapping prose ", 30)
	lines[12] = "brainstorm │ — turn complete (13560 tokens) —"
	want := -1
	for _, top := range []int{0, 5, 10, 12, 60, 150, 190} {
		got := lipgloss.Width(renderStreamDoor("GH-1", lines, streamState{Top: top}, 100, 40))
		if want == -1 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("frame width = %d at Top %d, want %d", got, top, want)
		}
	}
}

// The position readout tracks the window and names the reading mode.
func TestStreamDoorShowsHeldAndFollowing(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	deep := deepStream(200)
	following := ansi.Strip(renderStreamDoor("GH-1", deep, streamState{Follow: true}, 100, 40))
	if !strings.Contains(following, "· following") || !strings.Contains(following, "of ") {
		t.Fatalf("following footer missing:\n%s", following)
	}
	if strings.Contains(following, "held") {
		t.Fatalf("a live view must not read as held:\n%s", following)
	}
	held := ansi.Strip(renderStreamDoor("GH-1", deep, streamState{Top: 10}, 100, 40))
	if !strings.Contains(held, "· held · G to follow") {
		t.Fatalf("held footer missing:\n%s", held)
	}
	short := ansi.Strip(renderStreamDoor("GH-1", deepStream(3), streamState{Follow: true}, 100, 40))
	if strings.Contains(short, " of ") || strings.Contains(short, "following") {
		t.Fatalf("a body that fits must show no position:\n%s", short)
	}
}
