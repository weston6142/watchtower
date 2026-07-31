package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/weston6142/watchtower/internal/archmap"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/projection"
	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/store"
)

// Fixtures pose the Model for every flow so `watchtower snap` and the golden
// snapshot tests render through the real View(). Not used by the daemon or
// the live TUI.

// FixtureFlows lists every posable flow, in spec order.
func FixtureFlows() []string {
	return []string{"floor", "rows", "decision", "decisions-door", "tray", "modal", "backlog", "backlog-long", "levers", "arch", "pager", "help", "stream", "setup"}
}

func fixtureState() *projection.State {
	st := projection.NewState()
	// fx-dark is parked: it keeps a paused lane on the rendered grid so the
	// snapshots cover the one-cell paused marker and the notice-row hint.
	st.Order = []string{"ca-repo", "gh-importer", "fx-e2e", "fx-dark"}
	st.Issues["ca-repo"] = &projection.IssueView{
		ID: "ca-repo", Title: "create a repo for GH-1", Flow: "default",
		CurrentStage: "brainstorm", State: "waiting_decision", Tokens: 12000,
	}
	st.Issues["gh-importer"] = &projection.IssueView{
		ID: "gh-importer", Title: "issue importer — GitHub → watchtower", Flow: "default",
		CurrentStage: "execute", State: "running", Tokens: 96000,
		Completed: []string{"brainstorm", "spec", "plan"},
	}
	st.Issues["fx-e2e"] = &projection.IssueView{
		ID: "fx-e2e", Title: "flaky e2e fix — «unicode» plus a deliberately very long title to test truncation", Flow: "default",
		CurrentStage: "execute", State: "failed", LastError: "tests failed: importer_webhook_test.go", Tokens: 41000,
		Completed: []string{"brainstorm", "spec", "plan"}, Attempt: 2, AttemptOf: 3,
	}
	st.Decisions[1] = projection.DecisionView{
		ID: 1, IssueID: "ca-repo", Stage: "brainstorm",
		Question: "Should creating the repo mean just local version control, or also a hosted remote (GitHub) pushed from day one?",
		Options: []string{
			"Local git repo plus a hosted GitHub remote, pushed",
			"Local git repo only, add a remote later",
		},
		Recommended: 0,
		Why:         "The issue is numbered GH-1, suggesting GitHub is the intended home, and a remote gives backup and collaboration from day one.",
		Consequences: []string{
			"The project is immediately backed up and shareable, but requires GitHub credentials and picking an owner/name now",
			"Simpler setup with nothing published, but work stays only on this machine until a remote is added",
		},
		Reversible: "Cheap to change until others clone the remote or CI points at it.",
	}
	st.ShippedToday = []string{"ml-retry"}
	st.Parked = []string{"fx-dark"}
	st.Issues["ml-retry"] = &projection.IssueView{ID: "ml-retry", Title: "retry budget for marshal", Merged: true, State: "done"}
	st.Issues["fx-dark"] = &projection.IssueView{
		ID: "fx-dark", Title: "dark-mode audit", Flow: "default",
		Completed: []string{"brainstorm"}, CurrentStage: "spec",
		Paused: true, State: "paused",
	}
	return st
}

// applyBacklogDrafts seeds two drafts with distinct priorities so backlog
// ordering is visible in fixtures and tests.
func applyBacklogDrafts(s *projection.State) {
	for _, spec := range []struct {
		id, title string
		priority  int
	}{{"GH-2", "low fix", 0}, {"GH-3", "hot fix", 2}} {
		ev, _ := core.NewEvent(core.EvIssueDrafted, spec.id, map[string]any{
			"title": spec.title, "body": "b", "flow": "default", "preset": "regular",
			"priority": spec.priority})
		s.Apply(ev)
	}
}

// applyBacklogLongDrafts seeds a queue deeper than a terminal shows at once, so
// the goldens cover what two drafts cannot: the window clipping, the id column
// widening for an 11-cell id, titles truncating, and a body wrapping past the
// pane. It is deliberately separate from applyBacklogDrafts, whose two drafts
// are asserted on by name elsewhere.
func applyBacklogLongDrafts(s *projection.State) {
	titles := []string{
		"lane gutter spacing is off by one column",
		"rehydrate drafts as editable when r is pressed on a shipped lane instead of reopening the pager",
		"add a --json flag to the backlog command",
		"stream door drops the last line of a turn when the transcript scrolls",
	}
	body := "The stage gutter is one column narrower than the lane it labels, so " +
		"every row below the header reads one cell to the left of where the header " +
		"says it is.\n\n" +
		"Reproduced at 100 and at 200 columns, so it is the gutter constant and " +
		"not the lane width. The fix is one number, but the goldens for every " +
		"flow move with it, which is why this is its own draft."
	// Ordered so GH-01 is urgent: it then sorts first (priority descending, id
	// ascending) and is what the cursor and the detail pane land on.
	levels := []int{1, 2, 0, -1}
	for i := 1; i <= 36; i++ {
		id := fmt.Sprintf("GH-%02d", i)
		if i == 5 {
			// A slug id as wide as the ones the tower really issues, and not one
			// fixtureState already uses — reusing an id there would redraft a
			// launched lane instead of adding a draft.
			id = "gh-webhooks"
		}
		drafted := fmt.Sprintf("draft %02d body", i)
		if i == 1 {
			drafted = body // sorts first, so it is what the detail pane shows
		}
		ev, _ := core.NewEvent(core.EvIssueDrafted, id, map[string]any{
			"title": titles[i%len(titles)], "body": drafted,
			"flow": "default", "preset": "regular", "priority": levels[i%len(levels)]})
		s.Apply(ev)
	}
}

func fixtureProposals() []store.ProposalRow {
	return []store.ProposalRow{
		{ID: 1, Title: "Add smoke test for the importer webhook", Body: "The last two importer regressions were webhook-shaped. A 30-second smoke test on PR would have caught both before review."},
		{ID: 2, Title: "Split marshal retries into their own lever", Body: "Retry budget currently rides on the global aggressiveness lever; tuning one shouldn't move the other."},
		{ID: 3, Title: "Park FX-9 until the token budget resets", Body: "Dark-mode audit is cosmetic and burning budget the failing e2e fix needs this week."},
	}
}

func fixtureArch() *archmap.Map {
	return &archmap.Map{
		Modules: []archmap.Module{
			{Name: "internal/tui", Files: 22}, {Name: "internal/marshal", Files: 9},
			{Name: "internal/store", Files: 4}, {Name: "internal/engine", Files: 7},
			{Name: "internal/flow", Files: 3}, {Name: "internal/levers", Files: 2},
		},
		Overlays: []archmap.Overlay{
			{IssueID: "ca-repo", Globs: []string{"internal/tui/**"}},
			{IssueID: "gh-importer", Globs: []string{"internal/marshal/**", "internal/store/**"}},
		},
	}
}

// FixtureModel returns a Model posed for the named flow at the given size.
// fixtureSetup poses the setup inspector against the shape of
// .watchtower/flows/default.yaml: eight sequential stages, review and
// documentation expanded, treehouse resolved, and a fixed load time — no render path may reach
// for a clock, or the goldens stop being byte-comparable.
func fixtureSetup() *setupState {
	agent := func(name string, preview string) proto.AgentSetup {
		return proto.AgentSetup{
			Package: name, Model: "opus", Effort: "medium", ThinkingTokens: "8192",
			AllowedTools:  []string{"Bash", "Read", "Edit", "Glob", "Grep"},
			PromptLines:   24,
			PromptPreview: []string{preview},
		}
	}
	view := proto.SetupView{
		Flow: "default", IssueID: "fx-e2e", IssueTitle: "flaky e2e fix",
		Repo: proto.RepoSetup{
			Runner: "claude", Slots: 4, ClaudeBin: "claude",
			Pull: true, Push: true, Workspace: "treehouse", LoadedAt: "12:55",
		},
		Stages: []proto.StageSetup{
			{Name: "brainstorm", Gate: "auto", Workspace: "worktree", Completion: "all",
				Artifacts: []string{"brainstorm.md"}, Lever: "regular",
				Agents: []proto.AgentSetup{agent("brainstorm", "Explore the request before proposing anything.")}},
			{Name: "spec", Gate: "auto", Workspace: "worktree", Completion: "all",
				Artifacts: []string{"spec.md"}, Lever: "regular",
				Agents: []proto.AgentSetup{agent("spec-writer", "Turn the brainstorm into a spec with resolved decisions.")}},
			{Name: "plan", Gate: "auto", Workspace: "worktree", Completion: "all",
				Artifacts: []string{"plan.md", "touchset.json"}, Lever: "regular",
				Agents: []proto.AgentSetup{agent("planner", "Produce ordered bite-sized TDD tasks with real code.")}},
			{Name: "execute", Gate: "auto", Workspace: "worktree", Completion: "all",
				HeavySlot: true, Retries: 1, Lever: "regular",
				Agents: []proto.AgentSetup{agent("executor", "Work the plan one task at a time, committing each.")}},
			{Name: "correctness-review", Gate: "auto", Workspace: "worktree", Completion: "all",
				HeavySlot: true, Lever: "strict",
				Agents: []proto.AgentSetup{agent("correctness-reviewer", "Verify the implementation against the approved spec.")}},
			{Name: "clean-code-review", Gate: "auto", Workspace: "worktree", Completion: "all",
				Lever:  "regular",
				Agents: []proto.AgentSetup{agent("clean-code-reviewer", "Light single-pass clean code review of the branch diff.")}},
			{Name: "librarian", Gate: "auto", Workspace: "worktree", Completion: "all",
				Lever:  "regular",
				Agents: []proto.AgentSetup{agent("librarian", "Reconcile canonical docs before integration.")}},
			{Name: "merge-verification", Gate: "auto", Workspace: "worktree", Completion: "all",
				MergeBarrier: true, HeavySlot: true,
				Artifacts: []string{"merge-report.md", "merge-decision.json", "verification.json"},
				Lever:     "regular",
				Agents:    []proto.AgentSetup{agent("merge-verifier", "Run the final gate and record the merge decision.")}},
		},
	}
	return &setupState{View: &view, Expanded: map[string]bool{
		"correctness-review": true, "clean-code-review": true, "librarian": true,
	}}
}

func FixtureModel(flowName string, width, height int) Model {
	m := Model{
		State: fixtureState(),
		Overview: &proto.Overview{
			Building: 2, NeedYou: 1, Failing: 1, ShippedToday: 1,
			TokensTotal: 412000, DollarsTotal: 6.18,
		},
		Ids: map[string]Identity{
			"ca-repo":     {Tag: "CA", Color: "#bb9af7"},
			"gh-importer": {Tag: "GH", Color: "#7aa2f7"},
			"fx-e2e":      {Tag: "FX", Color: "#7dcfff"},
			"ml-retry":    {Tag: "ML", Color: "#9ece6a"},
			"fx-dark":     {Tag: "FX", Color: "#7dcfff"},
		},
		Focus:  Focus{Issue: "ca-repo"},
		Width:  width,
		Height: height,
	}
	m.stages = []string{
		"brainstorm", "spec", "plan", "execute", "correctness-review",
		"clean-code-review", "librarian", "merge-verification",
	}
	m.dismissed = map[int64]bool{}
	m.retired = map[string]bool{"ml-retry": true} // shipped lanes appear on the shelf once retired
	m.evidenceOpened = map[int64]bool{}
	switch flowName {
	case "rows":
		m.rows = true
	case "decision":
		d := m.State.Decisions[1]
		m.Toast = &d
	case "decisions-door":
		m.modes = []string{"decisions"}
	case "tray":
		m.modes = []string{"tray"}
		m.proposals = fixtureProposals()
	case "modal":
		m.modal = &modalState{Title: "Wire importer smoke test into CI", Field: 0,
			Attach: "/Users/me/Desktop/failing-run.png"}
	case "backlog":
		applyBacklogDrafts(m.State)
		m.backlog = &backlogState{}
	case "backlog-long":
		applyBacklogLongDrafts(m.State)
		m.backlog = &backlogState{}
	case "levers":
		m.leverEditor = newLeverEditor(
			"ca-repo", m.stages, map[string]string{"correctness-review": "strict"})
	case "arch":
		m.archMode = "full"
		m.Arch = fixtureArch()
	case "pager":
		m.pager = pagerState{Mode: "pager", Title: "evidence · decision 1", Lines: []string{
			"docs/brainstorm/repo-scope.md",
			"@@ options considered @@",
			"  The issue tracker prefix (GH-) implies a GitHub home.",
			"+ Recommendation: create remote now; renaming later breaks",
			"+ clone URLs and CI triggers once anything points at it.",
			"- Alternative: defer remote until first collaborator.",
			"  Cost of reversal stays low until CI or clones exist.",
		}}
	case "help":
		m.help = true
	case "stream":
		m.modes = []string{"transcript"}
		// The fixture pushes modes directly rather than going through T, so it
		// has to set the reading position itself.
		m.stream = streamState{Follow: true}
		m.doorLines = []string{
			"brainstorm │ Requirements settled. brainstorm.md is written to the issue directory.",
			"brainstorm │ ↳ Read internal/engine/engine.go",
			"brainstorm │ ↳ Bash go test ./internal/engine",
			"brainstorm │ The rename target is real: the remote is weston6142/watchtower and go.mod already agrees.",
			"brainstorm │ — turn complete (13560 tokens) —",
		}
	case "setup":
		m.Focus = Focus{Issue: "fx-e2e"}
		m.setup = fixtureSetup()
	}
	return m
}

// SnapshotFlow renders one flow's full screen for snapshots and goldens.
// The color profile is pinned so output is identical in tests, pipes, and
// real terminals.
func SnapshotFlow(flowName string, width, height int) string {
	lipgloss.SetColorProfile(termenv.TrueColor)
	m := FixtureModel(flowName, width, height)
	return m.View()
}
