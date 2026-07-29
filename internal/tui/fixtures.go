package tui

import (
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
	return []string{"floor", "rows", "decision", "decisions-door", "tray", "modal", "backlog", "levers", "arch", "pager", "help", "stream"}
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
	m.stages = []string{"brainstorm", "spec", "plan", "execute", "review", "merge"}
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
	case "levers":
		m.leverEditor = newLeverEditor("ca-repo", m.stages, map[string]string{"review": "strict"})
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
		m.doorLines = []string{
			"brainstorm │ Requirements settled. brainstorm.md is written to the issue directory.",
			"brainstorm │ ↳ Read internal/engine/engine.go",
			"brainstorm │ ↳ Bash go test ./internal/engine",
			"brainstorm │ The rename target is real: the remote is weston6142/watchtower and go.mod already agrees.",
			"brainstorm │ — turn complete (13560 tokens) —",
		}
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
