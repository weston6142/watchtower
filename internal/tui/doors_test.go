package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/projection"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/store"
)

func TestProposalDoorRetainsDependencies(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	out := ansi.Strip(renderProposalsDoor([]store.ProposalRow{{
		Title: "proposal", Body: "body", DependsOn: []string{"GH-1", "GH-2"},
	}}, 0, 80))
	if !strings.Contains(out, "depends on GH-1, GH-2") {
		t.Fatalf("proposal dependencies missing:\n%s", out)
	}
}

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

func TestDecisionsDoorDecisionContextNoTruncation(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	d := projection.DecisionView{ID: 2, IssueID: "GH-2", Stage: "spec", Question: "Proceed?", Context: longDecisionContext()}
	out := ansi.Strip(renderDecisionsDoor([]projection.DecisionView{d}, map[string]Identity{"GH-2": {Tag: "02"}}, 0, 48))
	for _, want := range []string{d.Context.TaskSummary, d.Context.AgentName, d.Context.AgentColor, d.Context.AgentSymbol, d.Question} {
		if !containsWrapped(out, want) {
			t.Fatalf("door missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "…") {
		t.Fatalf("door truncated decision context:\n%s", out)
	}
	for lineNo, line := range strings.Split(out, "\n") {
		if lipgloss.Width(line) > 48 {
			t.Fatalf("door line %d is %d cells wide:\n%s", lineNo, lipgloss.Width(line), out)
		}
	}
	legacy := ansi.Strip(renderDecisionsDoor([]projection.DecisionView{{ID: 3, IssueID: "GH-3", Stage: "spec", Question: "Legacy?"}}, map[string]Identity{"GH-3": {Tag: "03"}}, 0, 48))
	if strings.Contains(legacy, "task ·") || strings.Contains(legacy, "agent ·") || !strings.Contains(legacy, "Legacy?") {
		t.Fatalf("legacy door rendering changed:\n%s", legacy)
	}
}

func TestDecisionsDoorShowsArtifactReviewIdentity(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	digest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	d := projection.DecisionView{ID: 26, IssueID: "GH-26", Stage: "plan", Question: "Approve?", Review: &review.Target{
		IssueID: "GH-26", Stage: "plan", CheckpointID: 17,
		Artifacts:       []contextpack.Artifact{{Name: "touchset.json", SHA256: digest}},
		ArtifactVersion: "17|touchset.json=" + digest, NextStage: "execute",
	}}
	out := ansi.Strip(renderDecisionsDoor([]projection.DecisionView{d}, map[string]Identity{"GH-26": {Tag: "26"}}, 0, 48))
	for _, want := range []string{"artifact review", "checkpoint: 17", "artifact_version:", "next: execute", "touchset.json"} {
		if !containsWrapped(out, want) {
			t.Fatalf("door missing %q:\n%s", want, out)
		}
	}
	for start := 0; start < len(digest); start += 16 {
		end := min(start+16, len(digest))
		if !strings.Contains(out, digest[start:end]) {
			t.Fatalf("door missing digest fragment %q:\n%s", digest[start:end], out)
		}
	}
	if strings.Contains(out, "…") {
		t.Fatalf("door truncated review identity:\n%s", out)
	}
	for lineNo, line := range strings.Split(out, "\n") {
		if lipgloss.Width(line) > 48 {
			t.Fatalf("door line %d is %d cells wide:\n%s", lineNo, lipgloss.Width(line), out)
		}
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

func TestHumanAndPolicyPlanApprovalHaveDistinctTimelineText(t *testing.T) {
	policy := map[string]any{"mode": "regular", "policy_id": "team-ci", "policy_version": "2026-08-03"}
	approvedByAlice, err := json.Marshal(map[string]any{
		"approval_kind": "human", "actor_id": "alice", "review_policy": policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	autoApproved, err := json.Marshal(map[string]any{
		"approval_kind": "policy", "policy_id": "team-ci", "policy_version": "2026-08-03", "review_policy": policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []core.Event{
		{Type: core.EvPlanReviewRequested, IssueID: "GH-35", At: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC), Payload: json.RawMessage(`{"human_required":true,"mode":"regular","policy_id":"manual-default","policy_version":"1"}`)},
		{Type: core.EvPlanReviewHumanApproved, IssueID: "GH-35", At: time.Date(2026, 8, 3, 12, 1, 0, 0, time.UTC), Payload: approvedByAlice},
		{Type: core.EvPlanReviewPolicyApproved, IssueID: "GH-35", At: time.Date(2026, 8, 3, 12, 2, 0, 0, time.UTC), Payload: autoApproved},
		{Type: core.EvExecutionStarted, IssueID: "GH-35", At: time.Date(2026, 8, 3, 12, 3, 0, 0, time.UTC), Payload: json.RawMessage(`{"stage":"execute"}`)},
	}
	lines := humanizeEvents(events, "GH-35")
	for _, want := range []string{
		"plan review requested: human approval required",
		"plan approved by alice",
		"plan approved automatically by policy team-ci@2026-08-03",
		"execute authorized",
	} {
		found := false
		for _, line := range lines {
			if strings.Contains(line, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("timeline missing %q: %v", want, lines)
		}
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

// The stream-long goldens only prove the height plumbing if the fixture wraps
// narrow and not wide — otherwise both goldens are the same shape and the row
// arithmetic is untested. The gutter "brainstorm │ " is 13 cells, so prose
// wraps at 79 cells on the narrow golden and 179 on the wide one.
func TestStreamLongFixtureWrapsNarrowOnly(t *testing.T) {
	lines := fixtureStreamLong()
	if len(lines) < 60 {
		t.Fatalf("fixture is %d lines, want at least 60", len(lines))
	}
	prose := 0
	for i, line := range lines {
		stage, text, found := strings.Cut(line, streamGutterSep)
		if !found || stage != "brainstorm" {
			t.Fatalf("line %d has no brainstorm gutter: %q", i, line)
		}
		if strings.HasPrefix(text, streamToolPrefix) || strings.Contains(text, "turn complete") {
			continue
		}
		prose++
		if w := lipgloss.Width(text); w <= 79 || w > 179 {
			t.Fatalf("line %d width %d is outside (79, 179]: %q", i, w, text)
		}
	}
	if prose < 30 {
		t.Fatalf("only %d prose lines; too few to clip the narrow golden", prose)
	}
	if got := len(streamBody(lines, 192)); got <= streamRows(50) {
		t.Fatalf("wide body is %d rows, want more than %d", got, streamRows(50))
	}
	if got := len(streamBody(lines, 92)); got <= streamRows(40) {
		t.Fatalf("narrow body is %d rows, want more than %d", got, streamRows(40))
	}
}
