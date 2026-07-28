package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
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
	got := renderStreamDoor("GH-2 · brainstorm", lines, 100)
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
	out := renderStreamDoor("GH-1 · plan · 2 lines · 1 tool call", lines, 100)
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
	got := renderStreamDoor("GH-2", nil, 100)
	if !strings.Contains(got, "nothing here yet") {
		t.Fatalf("empty door = %q", got)
	}
}
