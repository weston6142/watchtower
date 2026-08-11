package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/weston6142/watchtower/internal/proto"
	"github.com/weston6142/watchtower/internal/scaffold"
)

// setupState is the setup inspector's own state: the daemon's picture of the
// running config, which stages are expanded, and a single flat cursor. Prompt
// bodies are not held here — they go to the pager, which owns their scroll.
type setupState struct {
	View     *proto.SetupView
	Expanded map[string]bool // stage name → expanded
	Sel      int             // index into selectable rows
	Top      int             // outline scroll offset
}

type setupRowKind int

const (
	// setupRowHeader rows are information, not targets: the repo header, a
	// stage's detail line, and an agent's tools/prompt rows.
	setupRowHeader setupRowKind = iota
	setupRowHealth
	setupRowStage
	setupRowAgent
)

type setupRow struct {
	Kind  setupRowKind
	Text  string // pre-styled, already width-bounded
	Stage string
	Pkg   string
}

const (
	// setupRowWidth is the outline's content column. Fixed so a long tools list
	// or prompt preview can never reflow the panel. 90 leaves the box (border +
	// padding = 4 more) inside the 100-cell narrow snapshot, and is wide enough
	// that header row two keeps its load stamp even in the fake-runner case,
	// where the workspace clause alone is 40 cells.
	setupRowWidth = 90
	// setupChromeRows is everything renderSetup adds around the windowed
	// outline: renderBox's border, title band, and band gap, plus the footer and
	// its gap. Pinned by TestSetupOutlineScrollsOnShortTerminal — raise it if
	// that test says the box is too tall, never lower it on a hunch.
	setupChromeRows = 10
	setupMinRows    = 3
)

// setupNoWorkspace names the absence rather than printing a blank: under the
// fake runner nothing provisions a workspace at all.
const setupNoWorkspace = "workspace — (fake runner provisions none)"

// setupFakeNoPackages explains an agent row with nothing behind it under the
// fake runner: no packages are loaded, so this is expected, not broken.
const setupFakeNoPackages = "fake runner — no agent packages loaded"

// setupNoArtifacts states that a stage declares none, rather than leaving the
// slot blank and reading as a rendering bug.
const setupNoArtifacts = "artifacts —"

// setupMissingPackage names the directory to check. A blank model row would
// read as a package with no model rather than a package that is not there.
func setupMissingPackage(name string) string {
	return "package not loaded — check .watchtower/packages/" + name + "/"
}

func setupOnOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// retryWord pluralizes the retry count; "1 retry" reads as a bug.
func retryWord(n int) string {
	if n == 1 {
		return "retry"
	}
	return "retries"
}

// setupRepoLines renders the two-row repo header. Row two leads with the load
// stamp and the resolved workspace provider — the stamp first because the
// fake-runner workspace clause is long and truncation eats the tail, and the
// stamp is what tells the operator how stale the panel is. Row one drops
// test_cmd first for the same
// reason: it is the least load-bearing item on that line.
func setupRepoLines(r proto.RepoSetup) []string {
	t := activeTheme
	label := lipgloss.NewStyle().Foreground(t.Dim)
	value := lipgloss.NewStyle().Foreground(t.Structure)
	budget := "budget off"
	if r.Budget > 0 {
		budget = fmt.Sprintf("budget %d tokens", r.Budget)
	}
	runnerKind := r.Runner
	if runnerKind == "" {
		runnerKind = "unknown runner"
	}
	provider := "runner_bin —"
	switch r.Runner {
	case "codex":
		provider = "codex_bin " + r.CodexBin
	case "claude":
		provider = "claude_bin " + r.ClaudeBin
	}
	first := []string{runnerKind, fmt.Sprintf("%d slots", r.Slots), budget,
		fmt.Sprintf("$%.2f/Mtok", r.PricePerMTok), provider}
	if r.TestCmd != "" {
		first = append(first, "test_cmd "+r.TestCmd)
	}
	ws := "workspace " + r.Workspace
	if r.Workspace == "" {
		ws = setupNoWorkspace
	}
	second := []string{}
	if r.LoadedAt != "" {
		second = append(second, "loaded "+r.LoadedAt)
	}
	second = append(second, ws, "pull "+setupOnOff(r.Pull), "push "+setupOnOff(r.Push))
	if r.Runner == "codex" {
		second = append(second, r.CodexModel+"/"+r.CodexEffort)
	}
	const gutter = 7
	lines := []string{
		label.Render(padCell("REPO", gutter)) + value.Render(truncate(strings.Join(first, " · "), setupRowWidth-gutter)),
		label.Render(padCell("", gutter)) + value.Render(truncate(strings.Join(second, " · "), setupRowWidth-gutter)),
	}
	if r.Runner == "codex" && r.CodexPrimary != nil {
		detail := []string{"codex " + r.CodexPolicy}
		features := make([]string, 0, len(r.CodexPrimary.FeatureOverrides))
		for feature := range r.CodexPrimary.FeatureOverrides {
			features = append(features, feature)
		}
		sort.Strings(features)
		for _, feature := range features {
			enabled := r.CodexPrimary.FeatureOverrides[feature]
			detail = append(detail, fmt.Sprintf("features.%s=%t", feature, enabled))
		}
		if r.CodexFallback != nil {
			detail = append(detail, "fallback configured")
		}
		lines = append(lines, label.Render(padCell("", gutter))+
			value.Render(truncate(strings.Join(detail, " · "), setupRowWidth-gutter)))
	}
	return lines
}

// setupStageLine is a stage's one-line summary: enough to answer "what gates
// this and where does it run?" without expanding.
func setupStageLine(st proto.StageSetup, expanded bool) string {
	t := activeTheme
	caret := "▸"
	if expanded {
		caret = "▾"
	}
	parts := []string{fmt.Sprintf("%d agent%s", len(st.Agents), pluralSuffix(len(st.Agents))), st.Gate}
	if st.CapabilityProfile != "" {
		parts = append(parts, "profile "+st.CapabilityProfile)
	}
	if st.Workspace == "none" || st.Workspace == "" {
		parts = append(parts, "no workspace")
	} else {
		parts = append(parts, st.Workspace)
	}
	if st.HeavySlot {
		parts = append(parts, "heavy")
	}
	if st.MergeBarrier {
		parts = append(parts, "barrier")
	}
	if st.Retries > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", st.Retries, retryWord(st.Retries)))
	}
	name := padCell(st.Name, 12)
	summary := truncate(strings.Join(parts, " · "), setupRowWidth-24)
	// Lever is blank when the view is repo-default, rather than a fabricated
	// "regular" that would read as a stored value.
	lever := st.Lever
	pad := max(1, setupRowWidth-2-lipgloss.Width(name)-lipgloss.Width(summary)-lipgloss.Width(lever))
	return lipgloss.NewStyle().Foreground(t.Accent).Render(caret) + " " +
		lipgloss.NewStyle().Foreground(t.Bright).Render(name) +
		lipgloss.NewStyle().Foreground(t.Structure).Render(summary) +
		strings.Repeat(" ", pad) +
		lipgloss.NewStyle().Foreground(t.Dim).Render(lever)
}

// setupStageDetail is the expanded stage's second line: the knobs that do not
// fit the summary.
func setupStageDetail(st proto.StageSetup) string {
	order := "sequential"
	if st.Parallel {
		order = "parallel"
	}
	parts := []string{order, "completion " + st.Completion, fmt.Sprintf("retries %d", st.Retries)}
	if len(st.Artifacts) == 0 {
		parts = append(parts, setupNoArtifacts)
	} else {
		parts = append(parts, "artifacts "+strings.Join(st.Artifacts, ", "))
	}
	if st.Lever != "" {
		parts = append(parts, "lever "+st.Lever)
	}
	if effective := st.EffectiveCapability; effective != nil {
		if effective.FailureReason != "" {
			parts = append(parts, "policy "+effective.FailureReason)
		}
		status := "preflight " + effective.Preflight + " · validation " + effective.Validation
		parts = append(parts, fmt.Sprintf("effective %d ops · %d/%d paths · %d/%d outputs · %s",
			len(effective.Operations), effective.ReadCount, effective.WriteCount,
			effective.AgentOutputs, effective.EngineOutputs, status))
	}
	if len(st.DocumentationPaths) > 0 {
		parts = append(parts, "docs "+strings.Join(st.DocumentationPaths, ", "))
	}
	return "  " + lipgloss.NewStyle().Foreground(activeTheme.Dim).
		Render(truncate(strings.Join(parts, " · "), setupRowWidth-4))
}

func setupCapabilityLines(effective *proto.EffectiveCapabilitySetup) []string {
	if effective == nil {
		return nil
	}
	lines := []string{fmt.Sprintf("  capability %s · preflight %s · validation %s · %s",
		truncate(strings.Join(effective.Operations, ","), 28), effective.Preflight, effective.Validation,
		truncate(effective.Provider+"/"+effective.Implementation, 24))}
	if effective.FailureReason != "" {
		failure := "  policy " + effective.FailureReason
		if effective.FailurePhase != "" {
			failure += " · " + effective.FailurePhase
		}
		if effective.RecoveryRequired != "" {
			failure += " · next " + effective.RecoveryRequired
		}
		lines = append(lines, failure)
	}
	for index := range lines {
		lines[index] = lipgloss.NewStyle().Foreground(activeTheme.Dim).Render(truncate(lines[index], setupRowWidth))
	}
	return lines
}

func setupHealthLine(health *scaffold.ConfigurationHealth) string {
	if health == nil {
		return ""
	}
	parts := []string{"CONFIG", string(health.Overall), "defaults " + health.DefaultsVersion}
	if health.ReloadRequired {
		parts = append(parts, "reload required")
	} else {
		parts = append(parts, "reload ok")
	}
	for _, class := range []scaffold.FileClass{
		scaffold.FileStale, scaffold.FileCustomized, scaffold.FileLegacy,
		scaffold.FileMissing, scaffold.FileExtra, scaffold.FileInvalid,
	} {
		if count := health.Counts[class]; count > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", class, count))
		}
	}
	if len(health.AffectedPaths) > 0 {
		parts = append(parts, fmt.Sprintf("%d paths", len(health.AffectedPaths)))
	}
	return lipgloss.NewStyle().Foreground(activeTheme.Structure).
		Render(truncate(strings.Join(parts, " · "), setupRowWidth))
}

// setupAgentLines renders one agent: its selectable head row first, then its
// detail rows. runnerKind distinguishes "the package is missing" from "the fake
// runner loads none", which are different facts.
func setupAgentLines(ag proto.AgentSetup, runnerKind string) []string {
	t := activeTheme
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	name := padCell(ag.Package, 22)
	if ag.Missing {
		note, style := setupMissingPackage(ag.Package), lipgloss.NewStyle().Foreground(t.Err)
		if runnerKind == "fake" {
			note, style = setupFakeNoPackages, dim
		}
		return []string{lipgloss.NewStyle().Foreground(t.Bright).Render(name) +
			style.Render(truncate(note, setupRowWidth-24))}
	}
	model := ag.Model
	if model == "" {
		model = "cli default"
	}
	effort := ag.Effort
	if effort == "" {
		effort = "default"
	}
	head := model + " · " + effort
	if ag.ThinkingTokens != "" {
		head += " (" + ag.ThinkingTokens + " thinking tokens)"
	}
	lines := []string{lipgloss.NewStyle().Foreground(t.Bright).Render(name) +
		lipgloss.NewStyle().Foreground(t.Structure).Render(truncate(head, setupRowWidth-24))}
	tools := "effective tools — supplied by stage capability"
	lines = append(lines, "    "+dim.Render(truncate(tools, setupRowWidth-6)))
	warn := lipgloss.NewStyle().Foreground(t.Warn)
	if len(ag.LegacyAllowedTools) > 0 {
		lines = append(lines, "    "+warn.Render(truncate(
			"legacy tools "+strings.Join(ag.LegacyAllowedTools, ", ")+" — "+ag.LegacyToolsNotice,
			setupRowWidth-6)))
	}
	if ag.DeclaredModel != "" {
		lines = append(lines, "    "+warn.Render(truncate(
			"declared model "+ag.DeclaredModel+" — not applied (runner passes the package model)",
			setupRowWidth-6)))
	}
	if ag.MaxTurns > 0 {
		lines = append(lines, "    "+warn.Render(truncate(
			fmt.Sprintf("declared max_turns %d — not applied (watchtower never passes it)", ag.MaxTurns),
			setupRowWidth-6)))
	}
	lines = append(lines, "    "+dim.Render(fmt.Sprintf("prompt %d line%s · enter opens",
		ag.PromptLines, pluralSuffix(ag.PromptLines))))
	for _, preview := range ag.PromptPreview {
		lines = append(lines, "    "+dim.Render("┃ "+truncate(preview, setupRowWidth-8)))
	}
	return lines
}

// setupRows flattens the outline into display rows. Only stage and agent rows
// are selectable; the repo header and the detail lines are information.
func setupRows(v proto.SetupView, expanded map[string]bool) []setupRow {
	rows := make([]setupRow, 0, 16)
	for _, line := range setupRepoLines(v.Repo) {
		rows = append(rows, setupRow{Kind: setupRowHeader, Text: line})
	}
	rows = append(rows, setupRow{Kind: setupRowHeader})
	if v.ConfigurationHealth != nil {
		rows = append(rows, setupRow{Kind: setupRowHealth, Text: setupHealthLine(v.ConfigurationHealth)})
		if v.ConfigurationHealth.Overall != scaffold.HealthCurrent && v.ConfigurationHealth.NextAction != "" {
			rows = append(rows, setupRow{Kind: setupRowHeader,
				Text: "  next: " + truncate(v.ConfigurationHealth.NextAction, setupRowWidth-2)})
		}
	}
	for _, st := range v.Stages {
		open := expanded[st.Name]
		rows = append(rows, setupRow{Kind: setupRowStage, Text: setupStageLine(st, open), Stage: st.Name})
		if !open {
			continue
		}
		rows = append(rows, setupRow{Kind: setupRowHeader, Text: setupStageDetail(st)})
		for _, line := range setupCapabilityLines(st.EffectiveCapability) {
			rows = append(rows, setupRow{Kind: setupRowHeader, Text: line})
		}
		for _, ag := range st.Agents {
			lines := setupAgentLines(ag, v.Repo.Runner)
			rows = append(rows, setupRow{Kind: setupRowAgent, Text: "  " + lines[0],
				Stage: st.Name, Pkg: ag.Package})
			for _, extra := range lines[1:] {
				rows = append(rows, setupRow{Kind: setupRowHeader, Text: "  " + extra})
			}
		}
	}
	return rows
}

// setupSelectable returns the indices of rows the cursor may land on.
func setupSelectable(rows []setupRow) []int {
	out := make([]int, 0, len(rows))
	for i, row := range rows {
		if row.Kind != setupRowHeader {
			out = append(out, i)
		}
	}
	return out
}

// selectableCount is how many rows the cursor may land on; 0 when the panel is
// posed without a view.
func (s *setupState) selectableCount() int {
	if s.View == nil {
		return 0
	}
	return len(setupSelectable(setupRows(*s.View, s.Expanded)))
}

// selectedRow is the row the cursor points at, false when the panel is posed
// without a view or the cursor is out of range. The one place that resolves
// Sel into a row, so the key handler and its tests cannot disagree.
func (s *setupState) selectedRow() (setupRow, bool) {
	if s.View == nil {
		return setupRow{}, false
	}
	rows := setupRows(*s.View, s.Expanded)
	sel := setupSelectable(rows)
	if s.Sel < 0 || s.Sel >= len(sel) {
		return setupRow{}, false
	}
	return rows[sel[s.Sel]], true
}

// setupWindow is how many outline rows fit, after chrome. Clamped before the
// content reaches renderBox: overlayCenter degrades to lipgloss.Place once the
// box reaches the terminal's height, which clips the panel.
func setupWindow(height int) int {
	return max(setupMinRows, height-setupChromeRows)
}

// clampTop scrolls Top the minimum distance that keeps the cursor's row inside
// the window, so j past the bottom edge advances the outline instead of
// stranding the cursor off screen.
func (s *setupState) clampTop(height int) {
	if s.View == nil {
		return
	}
	rows := setupRows(*s.View, s.Expanded)
	sel := setupSelectable(rows)
	if len(sel) == 0 {
		s.Sel, s.Top = 0, 0
		return
	}
	s.Sel = min(max(s.Sel, 0), len(sel)-1)
	window := setupWindow(height)
	cursor := sel[s.Sel]
	if cursor < s.Top {
		s.Top = cursor
	}
	if cursor >= s.Top+window {
		s.Top = cursor - window + 1
	}
	s.Top = min(max(s.Top, 0), max(0, len(rows)-window))
}

// renderSetup draws the read-only setup inspector. It reports the config the
// daemon is running, stamped with the load time — it does not stat files.
func renderSetup(s setupState, width, height int) string {
	dim := lipgloss.NewStyle().Foreground(activeTheme.Dim)
	foot := keyChip("j/k") + dim.Render(" move  ") +
		keyChip("enter") + dim.Render(" expand / open prompt  ") +
		keyChip("esc") + dim.Render(" close")
	if s.View == nil {
		return renderBox("setup", "", " esc close ",
			strings.Join([]string{dim.Render("no setup loaded"), "", foot}, "\n"))
	}
	rows := setupRows(*s.View, s.Expanded)
	sel := setupSelectable(rows)
	cursor := -1
	if s.Sel >= 0 && s.Sel < len(sel) {
		cursor = sel[s.Sel]
	}
	body := make([]string, 0, len(rows))
	for i, row := range rows {
		if row.Kind == setupRowHeader {
			body = append(body, "  "+row.Text)
			continue
		}
		body = append(body, cursorRow(i == cursor, row.Text, setupRowWidth+4))
	}
	window := setupWindow(height)
	top := min(max(s.Top, 0), max(0, len(body)-window))
	body = body[top:min(top+window, len(body))]
	sub := "repo default · no lane focused"
	if s.View.IssueID != "" {
		sub = s.View.IssueID + " " + s.View.IssueTitle
	}
	sub += " · flow " + s.View.Flow
	return renderBox("setup", truncate(sub, max(20, width/2)), " esc close ",
		strings.Join(append(body, "", foot), "\n"))
}
