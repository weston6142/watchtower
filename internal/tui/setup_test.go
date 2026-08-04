package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/proto"
)

// fixtureSetupView mirrors .watchtower/flows/default.yaml: six stages, review
// with three agents, treehouse resolved, loaded at a fixed time so nothing in
// a render path needs a clock.
func fixtureSetupView() proto.SetupView {
	agent := func(name string) proto.AgentSetup {
		return proto.AgentSetup{
			Package: name, Model: "gpt-5.6-luna", Effort: "xhigh",
			ToolSource:           "codex config",
			DeclaredAllowedTools: []string{"Bash", "Read", "Edit", "Glob", "Grep"},
			PromptLines:          24,
			PromptPreview:        []string{"Light single-pass clean code review of the branch diff."},
		}
	}
	return proto.SetupView{
		Flow: "default", IssueID: "fx-e2e", IssueTitle: "flaky e2e fix",
		Repo: proto.RepoSetup{
			Runner: "codex", Slots: 4, PricePerMTok: 0,
			CodexBin: "codex", CodexModel: "gpt-5.6-luna", CodexEffort: "xhigh",
			CodexPolicy:   "fallback_once",
			CodexPrimary:  &proto.CodexProfileSetup{Label: "primary", Bin: "codex", Model: "gpt-5.6-luna", Effort: "xhigh", FeatureOverrides: map[string]bool{"unified_exec": false}},
			CodexFallback: &proto.CodexProfileSetup{Label: "fallback", Bin: "codex", Model: "gpt-5.6-luna", Effort: "xhigh", FeatureOverrides: map[string]bool{"unified_exec": true}},
			ClaudeBin:     "claude",
			Pull:          true, Push: true, Workspace: "treehouse", LoadedAt: "12:55",
		},
		Stages: []proto.StageSetup{
			{Name: "brainstorm", Gate: "decision_queue", Workspace: "none", Completion: "all",
				Artifacts: []string{"brainstorm.md"}, Lever: "regular",
				Agents: []proto.AgentSetup{agent("brainstorm")}},
			{Name: "spec", Gate: "approve_artifact", Workspace: "none", Completion: "all",
				Artifacts: []string{"spec.md"}, Lever: "regular",
				Agents: []proto.AgentSetup{agent("spec-writer")}},
			{Name: "plan", Gate: "approve_artifact", Workspace: "none", Completion: "all",
				Artifacts: []string{"plan.md", "touchset.json"}, Lever: "regular",
				Agents: []proto.AgentSetup{agent("planner")}},
			{Name: "execute", Gate: "auto", Workspace: "worktree", Completion: "all",
				HeavySlot: true, Retries: 1, Lever: "regular",
				Agents: []proto.AgentSetup{agent("executor")}},
			{Name: "review", Gate: "auto", Workspace: "worktree", Completion: "all",
				Parallel: true, HeavySlot: true, Lever: "strict",
				Agents: []proto.AgentSetup{agent("clean-code-reviewer"), agent("reviewer"), agent("doc-writer")}},
			{Name: "merge", Gate: "approve_artifact", Workspace: "readonly", Completion: "all",
				MergeBarrier: true, Artifacts: []string{"merge-report.md"}, Lever: "regular",
				Agents: []proto.AgentSetup{agent("reviewer")}},
		},
	}
}

func fixtureClaudeSetupView() proto.SetupView {
	v := fixtureSetupView()
	v.Repo.Runner = "claude"
	v.Repo.CodexBin, v.Repo.CodexModel, v.Repo.CodexEffort = "", "", ""
	for i := range v.Stages {
		for j := range v.Stages[i].Agents {
			ag := &v.Stages[i].Agents[j]
			ag.Model, ag.Effort, ag.ThinkingTokens = "opus", "medium", "8192"
			ag.AllowedTools = append([]string(nil), ag.DeclaredAllowedTools...)
			ag.DeclaredAllowedTools = nil
			ag.ToolSource = ""
		}
	}
	return v
}

func fixtureSetupState() setupState {
	v := fixtureSetupView()
	return setupState{View: &v, Expanded: map[string]bool{"review": true}}
}

func ptrSetupView(v proto.SetupView) *proto.SetupView { return &v }

// The question that produced the issue — is this stage using a treehouse
// worktree? — is answered on the first screen, without expanding anything.
func TestSetupHeaderNamesResolvedWorkspace(t *testing.T) {
	got := ansi.Strip(renderSetup(fixtureSetupState(), 120, 50))
	for _, want := range []string{
		"workspace treehouse", "codex", "codex_bin codex", "gpt-5.6-luna", "xhigh",
		"4 slots", "loaded 12:55", "pull on", "push on", "fallback_once", "features.unified_exec=false",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("header missing %q in:\n%s", want, got)
		}
	}
}

// A collapsed stage shows a one-line summary; expanding it reveals every agent,
// not just the first. review has three.
func TestSetupExpandedStageListsEveryAgent(t *testing.T) {
	got := ansi.Strip(renderSetup(fixtureSetupState(), 120, 50))
	for _, want := range []string{"clean-code-reviewer", "reviewer", "doc-writer",
		"gpt-5.6-luna · xhigh", "tools codex config",
		"declared tools Bash, Read, Edit, Glob, Grep — not applied", "prompt 24 lines"} {
		if !strings.Contains(got, want) {
			t.Errorf("expanded review missing %q in:\n%s", want, got)
		}
	}
	// Only review is expanded, so only its three agents contribute detail rows.
	if n := strings.Count(got, "prompt 24 lines"); n != 3 {
		t.Errorf("got %d agent detail rows, want 3 (only review is expanded):\n%s", n, got)
	}
	// Collapsed means no agent detail rows at all.
	collapsed := setupState{View: ptrSetupView(fixtureSetupView()), Expanded: map[string]bool{}}
	shut := ansi.Strip(renderSetup(collapsed, 120, 50))
	if n := strings.Count(shut, "prompt 24 lines"); n != 0 {
		t.Errorf("a fully collapsed outline rendered %d agent detail rows:\n%s", n, shut)
	}
	if !strings.Contains(shut, "review") {
		t.Errorf("collapsed outline lost its stage rows:\n%s", shut)
	}
}

func TestSetupClaudeRenderingRetainsBinaryThinkingAndTools(t *testing.T) {
	v := fixtureClaudeSetupView()
	state := setupState{View: &v, Expanded: map[string]bool{"review": true}}
	got := ansi.Strip(renderSetup(state, 120, 50))
	for _, want := range []string{
		"claude_bin claude", "8192 thinking tokens", "tools Bash, Read, Edit, Glob, Grep",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Claude setup missing %q in:\n%s", want, got)
		}
	}
}

func TestSetupCursorSkipsNonSelectableRows(t *testing.T) {
	s := fixtureSetupState()
	rows := setupRows(*s.View, s.Expanded)
	sel := setupSelectable(rows)
	if len(sel) == 0 {
		t.Fatal("no selectable rows")
	}
	for _, i := range sel {
		if rows[i].Kind == setupRowHeader {
			t.Fatalf("row %d (%q) is selectable but is a header row", i, rows[i].Text)
		}
	}
	// six stages plus review's three agents
	if len(sel) != 9 {
		t.Fatalf("got %d selectable rows, want 9", len(sel))
	}
	// Every selectable row carries the coordinates enter needs.
	for _, i := range sel {
		if rows[i].Stage == "" {
			t.Errorf("selectable row %d has no stage", i)
		}
		if rows[i].Kind == setupRowAgent && rows[i].Pkg == "" {
			t.Errorf("agent row %d has no package", i)
		}
	}
}

// Six collapsed stage rows plus the repo header plus chrome will not fit a
// short terminal, and overlayCenter degrades to lipgloss.Place the moment the
// box reaches the terminal's height. The outline must window itself first.
func TestSetupOutlineScrollsOnShortTerminal(t *testing.T) {
	s := fixtureSetupState()
	const height = 12
	box := renderSetup(s, 120, height)
	if got := lipgloss.Height(box); got >= height {
		t.Fatalf("box is %d rows tall at height %d — overlayCenter will clip it", got, height)
	}
	// G reaches the last row and the window follows the cursor.
	last := s.selectableCount() - 1
	s.Sel = last
	s.clampTop(height)
	got := ansi.Strip(renderSetup(s, 120, height))
	if !strings.Contains(got, "merge") {
		t.Fatalf("last row not in view after G:\n%s", got)
	}
}

// Under the fake runner nothing provisions a workspace and no packages are
// loaded. Both must be named, not rendered as blanks.
func TestSetupFakeRunnerRendersExplanation(t *testing.T) {
	v := fixtureSetupView()
	v.Repo.Runner = "fake"
	v.Repo.Workspace = ""
	for i := range v.Stages {
		for j := range v.Stages[i].Agents {
			v.Stages[i].Agents[j] = proto.AgentSetup{
				Package: v.Stages[i].Agents[j].Package, Missing: true,
			}
		}
	}
	s := setupState{View: &v, Expanded: map[string]bool{"review": true}}
	got := ansi.Strip(renderSetup(s, 120, 50))
	if !strings.Contains(got, setupNoWorkspace) {
		t.Errorf("missing %q in:\n%s", setupNoWorkspace, got)
	}
	if !strings.Contains(got, setupFakeNoPackages) {
		t.Errorf("missing %q in:\n%s", setupFakeNoPackages, got)
	}
	// The load stamp must survive the longest header row two can get: losing the
	// panel's staleness marker in exactly the degraded case is backwards.
	if !strings.Contains(got, "loaded 12:55") {
		t.Errorf("fake-runner header truncated away the load stamp:\n%s", got)
	}
	for _, forbidden := range []string{"thinking tokens", "tools ", "prompt 0 lines"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("fake runner rendered an empty %q row:\n%s", forbidden, got)
		}
	}
}

// A package the flow names but the daemon never loaded is a real problem, and
// the panel says where to look.
func TestSetupMissingPackageNamesTheDirectory(t *testing.T) {
	v := fixtureSetupView()
	v.Stages[4].Agents[1] = proto.AgentSetup{Package: "ghost", Missing: true}
	s := setupState{View: &v, Expanded: map[string]bool{"review": true}}
	got := ansi.Strip(renderSetup(s, 120, 50))
	if !strings.Contains(got, setupMissingPackage("ghost")) {
		t.Errorf("missing %q in:\n%s", setupMissingPackage("ghost"), got)
	}
}

// Fields watchtower parses but never passes to the CLI must never read as
// effective settings.
func TestSetupLabelsDeclaredButUnappliedFields(t *testing.T) {
	v := fixtureSetupView()
	v.Stages[4].Agents[0].DeclaredModel = "sonnet"
	v.Stages[4].Agents[0].MaxTurns = 12
	s := setupState{View: &v, Expanded: map[string]bool{"review": true}}
	got := ansi.Strip(renderSetup(s, 120, 50))
	if !strings.Contains(got, "declared model sonnet — not applied") {
		t.Errorf("declared model not labelled in:\n%s", got)
	}
	if !strings.Contains(got, "declared max_turns 12 — not applied") {
		t.Errorf("declared max_turns not labelled in:\n%s", got)
	}
}

// An empty artifacts: list is a fact worth stating; a blank is not.
func TestSetupEmptyArtifactsRendersDash(t *testing.T) {
	got := ansi.Strip(renderSetup(fixtureSetupState(), 120, 50))
	if !strings.Contains(got, setupNoArtifacts) {
		t.Errorf("missing %q in:\n%s", setupNoArtifacts, got)
	}
}

// f opens from the grid only. Inside a door the door's key handler consumes it
// first — the same rule d/t/e/T/u/z follow.
func TestSetupKeyOpensPanelFromGrid(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	)
	m = pressKey(t, m, "f")
	if !m.wantSetup {
		t.Fatal("f on the grid did not request the setup outline")
	}

	// laneModel focuses GH-1, so the door model uses the same id.
	inDoor := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	)
	inDoor = pressKey(t, inDoor, "e") // timeline door
	if len(inDoor.modes) == 0 {
		t.Fatal("timeline door did not open")
	}
	inDoor = pressKey(t, inDoor, "f")
	if inDoor.wantSetup {
		t.Fatal("f inside a door opened the setup panel")
	}
}

// The panel paints over the grid, so it has to swallow the grid's keys —
// otherwise p pauses a lane and x arms a kill behind a screen you cannot see.
func TestSetupPanelSwallowsGridKeys(t *testing.T) {
	m := laneModel(t,
		mkev(t, core.EvIssueCreated, "GH-1", map[string]any{"title": "t", "flow": "default"}),
		mkev(t, core.EvStageStarted, "GH-1", map[string]any{"stage": "brainstorm"}),
	)
	state := fixtureSetupState()
	m.setup = &state
	for _, key := range []string{"p", "x", "n", "b", "d", "t", "u", "z"} {
		m = pressKey(t, m, key)
		if m.setup == nil {
			t.Fatalf("key %q closed the panel", key)
		}
		if m.confirm != nil || m.modal != nil || m.backlog != nil || len(m.modes) != 0 || m.rows {
			t.Fatalf("key %q drove the screen beneath the panel", key)
		}
	}
	m = pressKey(t, m, "esc")
	if m.setup != nil {
		t.Fatal("esc did not close the panel")
	}
}

func TestSetupEnterTogglesStageAndOpensAgentPrompt(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec", "plan", "execute", "review", "merge"})
	state := setupState{View: ptrSetupView(fixtureSetupView()), Expanded: map[string]bool{}}
	m.setup = &state
	m.Height = 50
	// Sel 0 is the first stage row: enter expands it.
	m = pressKey(t, m, "enter")
	if !m.setup.Expanded["brainstorm"] {
		t.Fatal("enter on a stage row did not expand it")
	}
	m = pressKey(t, m, "enter")
	if m.setup.Expanded["brainstorm"] {
		t.Fatal("enter on an expanded stage row did not collapse it")
	}
	// Expand review, move the cursor onto one of its agents, and check enter
	// targets that agent rather than toggling a stage.
	m.setup.Expanded["review"] = true
	rows := setupRows(*m.setup.View, m.setup.Expanded)
	sel := setupSelectable(rows)
	agentIdx := -1
	for i, r := range sel {
		if rows[r].Kind == setupRowAgent && rows[r].Pkg == "reviewer" {
			agentIdx = i
			break
		}
	}
	if agentIdx < 0 {
		t.Fatal("no reviewer agent row")
	}
	m.setup.Sel = agentIdx
	row, ok := m.setup.selectedRow()
	if !ok {
		t.Fatal("cursor resolved to no row")
	}
	if row.Kind != setupRowAgent || row.Stage != "review" || row.Pkg != "reviewer" {
		t.Fatalf("enter would open %s/%s, want the review/reviewer agent row", row.Stage, row.Pkg)
	}
}

// The panel yields to its own prompt pager. Both the key branch and the render
// branch are compound, and this is the guard for both: a bare m.setup != nil
// would swallow j/k/esc and win the View() if/else-if chain outright.
func TestSetupPromptPagerScrollsAndEscReturnsToOutline(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm", "spec", "plan", "execute", "review", "merge"})
	state := fixtureSetupState()
	state.Sel = 4
	m.setup = &state
	m.Height = 20
	lines := make([]string, 60)
	for i := range lines {
		lines[i] = fmt.Sprintf("prompt line %d", i)
	}
	next, _ := m.Update(setupPromptMsg{stage: "review", pkg: "reviewer", lines: lines})
	m = next.(Model)
	if m.pager.Mode != "pager" || m.pager.Title != "review · reviewer · prompt" {
		t.Fatalf("pager not opened from the prompt: %+v", m.pager)
	}
	m = pressKey(t, m, "j")
	if m.pager.Top != 1 {
		t.Fatalf("j did not scroll the prompt (Top=%d) — the panel swallowed it", m.pager.Top)
	}
	// The prompt must render, not the outline.
	if !strings.Contains(ansi.Strip(m.View()), "prompt line 1") {
		t.Fatal("View() rendered the outline over the open prompt")
	}
	m = pressKey(t, m, "esc")
	if m.pager.Mode != "" || len(m.pager.Lines) != 0 {
		t.Fatalf("esc left the pager open: %+v", m.pager)
	}
	if m.setup == nil {
		t.Fatal("esc closed the outline instead of returning to it")
	}
	if m.setup.Sel != 4 {
		t.Fatalf("outline cursor moved to %d — it must land back on the agent row", m.setup.Sel)
	}
	if !strings.Contains(ansi.Strip(m.View()), "workspace treehouse") {
		t.Fatal("outline did not reappear after esc")
	}
}

// enter fires a socket round trip, and f can close the panel before it lands.
// The pager must not open on top of nothing: with m.setup gone there is no
// outline to return to, and esc would fall into the artifact-list branch with
// Files nil — an empty "no artifacts" box the operator never asked for.
func TestSetupPromptArrivingAfterCloseIsDropped(t *testing.T) {
	m := NewModel(nil, []string{"review"})
	next, _ := m.Update(setupPromptMsg{stage: "review", pkg: "reviewer",
		lines: []string{"prompt line 0"}})
	m = next.(Model)
	if m.pager.Mode != "" {
		t.Fatalf("pager opened with the panel closed: %+v", m.pager)
	}
}

func TestSetupErrorSurfacesInKeybarAndPanelStaysShut(t *testing.T) {
	m := NewModel(nil, []string{"brainstorm"})
	next, _ := m.Update(setupMsg{err: errors.New("unknown flow nope")})
	m = next.(Model)
	if m.setup != nil {
		t.Fatal("panel opened on an error response")
	}
	if m.Err != "unknown flow nope" {
		t.Fatalf("Err = %q, want the daemon's error", m.Err)
	}
	if m.wantSetup {
		t.Fatal("wantSetup left set after an error")
	}
}

// A key nobody can discover is a key nobody uses.
func TestSetupKeyAppearsInHelp(t *testing.T) {
	got := ansi.Strip(renderHelpOverlay(200))
	if !strings.Contains(got, "setup inspector") {
		t.Fatalf("help does not list the setup inspector:\n%s", got)
	}
}
