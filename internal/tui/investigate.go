package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	investigateAgents = []string{"claude", "codex"}
	investigateModels = map[string][]string{
		"claude": {"fable-5", "opus-5", "sonnet-5"},
		"codex":  {"gpt-5.6-luna", "gpt-5.4"},
	}
	investigateEfforts = []string{"low", "medium", "high", "xhigh"}
)

// investigateState is a pure chooser: three fixed-choice fields, no text.
type investigateState struct {
	Agent  int `json:"agent"`
	Model  int `json:"model"`
	Effort int `json:"effort"`
	Field  int `json:"-"`
}

const (
	investigateFieldAgent = iota
	investigateFieldModel
	investigateFieldEffort
	investigateFieldCount
)

func (s investigateState) agent() string {
	return investigateAgents[clampIdx(s.Agent, len(investigateAgents))]
}

func (s investigateState) model() string {
	models := investigateModels[s.agent()]
	return models[clampIdx(s.Model, len(models))]
}

func (s investigateState) effort() string {
	return investigateEfforts[clampIdx(s.Effort, len(investigateEfforts))]
}

func clampIdx(i, n int) int {
	if i < 0 || i >= n {
		return 0
	}
	return i
}

func cycleIdx(i, delta, n int) int {
	return ((clampIdx(i, n)+delta)%n + n) % n
}

func (s investigateState) cycle(delta int) investigateState {
	switch s.Field {
	case investigateFieldAgent:
		s.Agent = cycleIdx(s.Agent, delta, len(investigateAgents))
		s.Model = 0
	case investigateFieldModel:
		s.Model = cycleIdx(s.Model, delta, len(investigateModels[s.agent()]))
	case investigateFieldEffort:
		s.Effort = cycleIdx(s.Effort, delta, len(investigateEfforts))
	}
	return s
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func buildInvestigateCommand(agent, model, effort string) string {
	if agent == "codex" {
		return fmt.Sprintf("exec codex -m %s -c model_reasoning_effort=%s %s",
			model, effort, shellQuote(investigationPrompt))
	}
	return fmt.Sprintf("exec claude --model %s --effort %s --append-system-prompt %s",
		model, effort, shellQuote(investigationPrompt))
}

func investigatePrefsPath(repo string) string {
	return filepath.Join(repo, ".watchtower", "investigate.json")
}

func loadInvestigatePrefs(repo string) investigateState {
	var s investigateState
	data, err := os.ReadFile(investigatePrefsPath(repo))
	if err != nil {
		return investigateState{}
	}
	if json.Unmarshal(data, &s) != nil {
		return investigateState{}
	}
	s.Field = 0
	return s
}

// saveInvestigatePrefs is best-effort: a repo without .watchtower (or a
// read-only disk) must not break spawning.
func saveInvestigatePrefs(repo string, s investigateState) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.WriteFile(investigatePrefsPath(repo), data, 0o644)
}

func renderInvestigate(s investigateState, width int) string {
	dim := lipgloss.NewStyle().Foreground(activeTheme.Dim)
	lines := []string{
		modalChoiceField(s.Field == investigateFieldAgent, "agent", s.agent()),
		modalChoiceField(s.Field == investigateFieldModel, "model", s.model()),
		modalChoiceField(s.Field == investigateFieldEffort, "effort", s.effort()),
		"",
		keyChip("tab") + dim.Render(" next field  ") + keyChip("h/l") + dim.Render(" adjust  ") + keyChip("enter") + dim.Render(" open session"),
	}
	return renderBox("investigate", "chat before the formal flow", " esc cancel ", boundedLines(lines, max(1, width-6)))
}

// investigationPrompt is injected into the spawned session (claude: system
// prompt; codex: initial prompt). Agent-neutral by design.
const investigationPrompt = `You are in a watchtower INVESTIGATION session for this repository.
The operator wants to explore an idea, bug, or question in chat before deciding
whether it becomes tracked work. Investigate collaboratively: read code, answer
questions, prototype reasoning. Do not start a formal development workflow.

When the investigation winds down (or the operator types /wrap-up or says
"wrap up"), offer exactly three exits:
1. discard - just quit; nothing is saved.
2. save draft - file the findings as a backlog issue.
3. save + launch - file the issue and start the workflow on it immediately.

For exits 2 and 3: distill the investigation into a one-line title and a body
that captures the findings, open questions, and suggested approach. Show the
title and body to the operator for approval first, then run:
  save draft:   watchtower new --title "<title>" --body "<body>" --draft
  save+launch:  watchtower new --title "<title>" --body "<body>"
Then close this pane with: herdr pane close "$HERDR_PANE_ID"
If watchtower new fails, show the error and stay open; nothing is lost.`

const wrapUpSkill = `---
name: watchtower-wrap-up
description: Use when a watchtower investigation session is wrapping up - distill findings into an issue and exit via discard, save draft, or save + launch.
---

# Watchtower Wrap-Up

1. Distill this investigation into an issue: a one-line title and a body
   capturing findings, open questions, and a suggested approach. Suggest a
   priority (0 = default).
2. Show the title and body, then ask the operator to choose:
   **save draft** / **save + launch** / **discard**.
3. Run the exit:
   - save draft: watchtower new --title "<title>" --body "<body>" --draft
   - save + launch: watchtower new --title "<title>" --body "<body>"
   - discard: skip the command.
4. On success, close the pane: herdr pane close "$HERDR_PANE_ID"
   On failure, show the error and stay open.
`

// ensureWrapUpSkill installs the /wrap-up trigger for claude sessions. It
// never overwrites: the operator may have customized the skill.
func ensureWrapUpSkill(repo string) error {
	dir := filepath.Join(repo, ".claude", "skills", "watchtower-wrap-up")
	path := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(wrapUpSkill), 0o644)
}
