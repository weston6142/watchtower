package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/evidence"
	"github.com/weston6142/watchtower/internal/projection"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/store"
)

func TestRenderToastMarksRecommended(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	d := projection.DecisionView{ID: 4, IssueID: "GH-1", Stage: "spec",
		Question: "Approve spec artifacts?", Options: []string{"approve", "reject"}, Recommended: 0}
	out := renderToast(d, Identity{Color: "#61afef", Tag: "PA"}, 0, 0, 60)
	if !strings.Contains(out, "Approve spec artifacts?") || !strings.Contains(out, "approve ★") {
		t.Fatalf("toast:\n%s", out)
	}
	if !strings.Contains(out, "accept ★") {
		t.Fatalf("keys missing:\n%s", out)
	}
}

func TestRenderToastAlwaysShowsAddNote(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	for _, allow := range []bool{false, true} {
		d := projection.DecisionView{
			ID: 4, Stage: "merge", Question: "Repair?",
			Options: []string{"Apply fix", "Hold"}, Recommended: 0, AllowFreeform: allow,
		}
		out := renderToast(d, Identity{Tag: "GH"}, len(d.Options), 0, 60)
		if !strings.Contains(out, "Add note...") || !strings.Contains(out, "send feedback instead of selecting an option") {
			t.Fatalf("allow_freeform=%v note row missing:\n%s", allow, out)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "Add note...") && !strings.Contains(line, "◉") {
				t.Fatalf("allow_freeform=%v note row is not selected:\n%s", allow, out)
			}
		}
	}

	d := projection.DecisionView{
		ID: 4, Stage: "merge", Question: "Repair?",
		Options: []string{"Apply fix", "Hold"}, Recommended: 0,
	}
	for _, width := range []int{48, 100} {
		out := renderToast(d, Identity{Tag: "GH"}, len(d.Options), 0, width)
		if !strings.Contains(out, "Add note...") || !strings.Contains(out, "send feedback instead of selecting") || !strings.Contains(out, "an option") {
			t.Fatalf("width %d wrapped note text missing:\n%s", width, out)
		}
		if width == 100 {
			for lineNo, line := range strings.Split(out, "\n") {
				if lipgloss.Width(line) > width {
					t.Fatalf("width %d line %d is %d cells wide:\n%s", width, lineNo, lipgloss.Width(line), out)
				}
			}
		}
	}
}

func TestRenderToastShowsFreeformRecommendation(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	d := projection.DecisionView{
		ID: 4, Stage: "spec", Kind: "freeform", Question: "Review spec.md",
		RecommendedResponse: "Approve spec.md as written.",
	}
	out := renderToast(d, Identity{Tag: "GH"}, 0, 0, 60)
	if !strings.Contains(out, "Approve spec.md as written.") || !strings.Contains(out, "edit") {
		t.Fatalf("freeform recommendation missing:\n%s", out)
	}
}

func TestRenderToastUsesTheme(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	d := projection.DecisionView{ID: 3, Stage: "plan", Question: "Pick one", Options: []string{"a", "b"}, Recommended: 0}
	out := renderToast(d, Identity{Tag: "st-1"}, 0, 0, 60)
	// theme accent (tokyo-night #bb9af7 → truecolor SGR 187;154;247) on border/keys
	if !strings.Contains(out, "187;154;247") {
		t.Fatalf("no accent color in toast:\n%q", out)
	}
	if strings.Contains(out, "\x1b[38;5;245m") {
		t.Fatal("hardcoded color 245 still present")
	}
}

func TestRenderToastWrapsLongLines(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	d := projection.DecisionView{ID: 4, IssueID: "GH-1", Stage: "brainstorm",
		Question:    "Should creating the repo mean just setting up local version control, or also creating a hosted remote somewhere?",
		Options:     []string{"local git repo plus a hosted remote", "local git repo only, add a remote later"},
		Why:         "the issue is numbered GH-1, suggesting GitHub is the intended home for this project",
		Recommended: 0}
	out := renderToast(d, Identity{Color: "#61afef", Tag: "PA"}, 0, 0, 48)
	if strings.Contains(out, "…") {
		t.Fatalf("toast truncated instead of wrapping:\n%s", out)
	}
	flat := strings.Join(strings.Fields(strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ").Replace(out)), " ")
	for _, want := range []string{"hosted remote somewhere?", "intended home for this project", "add a remote later"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("missing wrapped text %q:\n%s", want, out)
		}
	}
}

func TestRenderToastShowsSelectionCursor(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	d := projection.DecisionView{ID: 4, IssueID: "GH-1", Stage: "spec",
		Question: "Approve the spec?", Options: []string{"approve", "reject"}, Recommended: 0}
	out := renderToast(d, Identity{Color: "#61afef", Tag: "PA"}, 1, 0, 60)
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "reject") && !strings.Contains(line, "◉") {
			t.Fatalf("selected option missing radio:\n%s", out)
		}
		if strings.Contains(line, "approve") && strings.Contains(line, "◉") {
			t.Fatalf("radio on unselected option:\n%s", out)
		}
	}
	if !strings.Contains(out, "j/k") || !strings.Contains(out, "enter") {
		t.Fatalf("footer missing j/k · enter hints:\n%s", out)
	}
}

func TestToastV2RendersRationaleAndConsequences(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	d := projection.DecisionView{ID: 4, IssueID: "GH-1", Stage: "spec",
		Question: "Approve the spec?", Options: []string{"approve", "reject"}, Recommended: 0,
		Why: "scope is settled", Consequences: []string{"planning starts now", "agent revises (~10 min)"},
		Reversible: "changeable until build"}
	out := renderToast(d, Identity{Color: "#61afef", Tag: "PA"}, 0, 4, 70)
	for _, want := range []string{"scope is settled", "planning starts now", "agent revises", "changeable until build", "4 recommendations in a row"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if out2 := renderToast(d, Identity{Color: "#61afef", Tag: "PA"}, 0, 0, 70); strings.Contains(out2, "in a row") {
		t.Fatal("friction line shown with zero streak")
	}
}

func TestEvidencePanelFromBundle(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	b := evidence.Bundle{Added: 412, Removed: 88, Biggest: "payments/gateway/client.go",
		Files:      make([]evidence.FileStat, 14),
		AreaWeight: map[string]int{"payments": 300, "api": 40}}
	out := renderEvidence(b, "GH-1 payment adapter", 76)
	for _, want := range []string{"14 files", "+412", "−88", "payments/gateway/client.go", "payments"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestRenderRailShowsQueueOrder(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	m := navModel(t)
	m = m.applyEvents([]core.Event{
		mkev(t, core.EvDecisionRequired, "GH-1", map[string]any{
			"decision_id": float64(1), "stage": "spec", "question": "first?",
			"options": []any{"a"}, "recommended": float64(0)}),
		mkev(t, core.EvDecisionRequired, "GH-2", map[string]any{
			"decision_id": float64(2), "stage": "spec", "question": "second?",
			"options": []any{"a"}, "recommended": float64(0)}),
	})
	out := renderRail(m.State, m.Ids, nil, 40)
	if strings.Index(out, "first?") > strings.Index(out, "second?") {
		t.Fatalf("queue order wrong:\n%s", out)
	}
}

func TestRenderRailFocusV2(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	det := &proto.IssueDetail{
		Issue:     store.IssueRow{ID: "GH-1", Title: "payment adapter", State: "failed", Flow: "default"},
		Tokens:    740,
		Budget:    1000,
		Dollars:   2.10,
		LastError: "tests failed",
		Attempt:   3,
		AttemptOf: 4,
		Levers:    map[string]string{"brainstorm": "yolo", "spec": "strict", "execute": "regular"},
		Runs:      []store.StageRun{{Worktree: "/tmp/GH-1", SessionID: "sess-7"}},
	}
	out := renderRail(nil, map[string]Identity{"GH-1": {Tag: "PA", Color: "#61afef"}}, det, 100)
	for _, want := range []string{
		"payment adapter", "failed", "error: tests failed · attempt 3 of 4",
		"budget", "74%", "$2.10", "levers", "B:auto", "S:you", "E:regular",
		"/tmp/GH-1", "sess-7",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestRailShowsModelAndEffort(t *testing.T) {
	det := &proto.IssueDetail{Issue: store.IssueRow{ID: "GH-1", Title: "t", Flow: "default", State: "running"},
		Model: "sonnet", Effort: "medium"}
	out := renderRail(nil, map[string]Identity{}, det, 60)
	if !strings.Contains(out, "model sonnet · effort medium") {
		t.Fatalf("rail missing model line:\n%s", out)
	}
	det.Model, det.Effort = "", ""
	out = renderRail(nil, map[string]Identity{}, det, 60)
	if !strings.Contains(out, "model cli default") {
		t.Fatalf("rail missing cli-default line:\n%s", out)
	}
}

func TestRailShowsCleanupOperations(t *testing.T) {
	det := &proto.IssueDetail{
		Issue: store.IssueRow{
			ID: "GH-1", Title: "landed task", Flow: "default", State: "cleanup_needed",
		},
		Cleanup: []string{"delete_branch:issue/GH-1"},
	}
	out := renderRail(nil, map[string]Identity{}, det, 80)
	for _, want := range []string{
		"shipped · cleanup needed", "cleanup needed: delete_branch:issue/GH-1",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestRailIsBoxed(t *testing.T) {
	out := renderRail(nil, map[string]Identity{}, nil, 40)
	if !strings.Contains(out, "─") || !strings.Contains(out, "│") {
		t.Fatalf("rail has no border:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > 40 {
			t.Fatalf("rail line %d cells wide, budget 40: %q", w, line)
		}
	}
}
