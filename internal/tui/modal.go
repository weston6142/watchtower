package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/weston6142/watchtower/internal/flow"
)

// modalState is intentionally small: the control room only needs plain rune
// input for a title and a few optional text fields.
type modalState struct {
	Title    string
	Body     string
	Field    int
	FlowName string
	Preset   string
}

func (m modalState) input(key string) modalState {
	switch key {
	case "backspace":
		value := m.fieldValue()
		runes := []rune(value)
		if len(runes) > 0 {
			m.setFieldValue(string(runes[:len(runes)-1]))
		}
	case "tab":
		m.Field = (m.Field + 1) % 4
	default:
		if key != "" && !strings.ContainsAny(key, "\n\r\t") {
			m.setFieldValue(m.fieldValue() + key)
		}
	}
	return m
}

func (m *modalState) setFieldValue(value string) {
	switch m.Field {
	case 0:
		m.Title = value
	case 1:
		m.Body = value
	case 2:
		m.FlowName = value
	case 3:
		m.Preset = value
	}
}

func (m modalState) fieldValue() string {
	switch m.Field {
	case 0:
		return m.Title
	case 1:
		return m.Body
	case 2:
		return m.FlowName
	case 3:
		return m.Preset
	default:
		return ""
	}
}

// renderBox wraps content in the themed overlay chrome: accent border,
// bright title, dim subtitle, inverted chip pinned to the right.
func renderBox(title, sub, chipText, content string) string {
	t := activeTheme
	head := lipgloss.NewStyle().Foreground(t.Bright).Bold(true).Render(title)
	if sub != "" {
		head += lipgloss.NewStyle().Foreground(t.Dim).Render(" " + sub)
	}
	chip := lipgloss.NewStyle().Foreground(t.Panel).Background(t.Accent).Bold(true).Render(chipText)
	inner := max(lipgloss.Width(content), lipgloss.Width(head)+lipgloss.Width(chip)+2)
	gap := max(1, inner-lipgloss.Width(head)-lipgloss.Width(chip))
	header := head + strings.Repeat(" ", gap) + chip
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(t.Accent).
		Padding(0, 1).
		Render(header + "\n\n" + content)
}

func renderModal(m modalState, width int) string {
	flowName := m.FlowName
	if flowName == "" {
		flowName = "default"
	}
	preset := m.Preset
	if preset == "" {
		preset = string(flow.LeverRegular)
	}
	t := activeTheme
	key := lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	lines := []string{
		modalField(m.Field == 0, "title", m.Title, true),
		modalField(m.Field == 1, "body", m.Body, false),
		modalField(m.Field == 2, "flow", flowName, false),
		modalField(m.Field == 3, "preset", preset, false),
		"",
		key.Render("tab") + dim.Render(" next field · ") + key.Render("enter") + dim.Render(" create"),
	}
	return renderBox("new issue", "", " esc cancel ", boundedLines(lines, max(1, width-6)))
}

func modalField(selected bool, name, value string, required bool) string {
	mark := "  "
	if selected {
		mark = "▸ "
	}
	requiredMark := ""
	if required {
		requiredMark = " *"
	}
	return fmt.Sprintf("%s%s%s: %s", mark, name, requiredMark, value)
}

func renderConfirm(prompt string, width int) string {
	t := activeTheme
	key := lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	hint := key.Render("y") + dim.Render(" confirm")
	content := boundedLines([]string{prompt, "", hint}, max(1, width-6))
	return renderBox("confirm", "", " n cancel ", content)
}

var leverCycle = []string{string(flow.LeverYolo), string(flow.LeverRegular), string(flow.LeverStrict)}

type leverEditorState struct {
	IssueID  string
	Stages   []string
	Matrix   map[string]string
	Original map[string]string
	Sel      int
}

func newLeverEditor(issueID string, stages []string, values map[string]string) *leverEditorState {
	matrix := make(map[string]string, len(stages))
	original := make(map[string]string, len(stages))
	for _, stage := range stages {
		value := values[stage]
		if value == "" {
			value = string(flow.LeverRegular)
		}
		matrix[stage] = value
		original[stage] = value
	}
	return &leverEditorState{IssueID: issueID, Stages: append([]string(nil), stages...), Matrix: matrix, Original: original}
}

func renderLeverEditor(stages []string, matrix map[string]string, sel int) string {
	lines := []string{"LEVERS · j/k row · h/l value · enter apply · esc cancel"}
	for i, stage := range stages {
		mark := "  "
		if i == sel {
			mark = "▸ "
		}
		value := matrix[stage]
		if value == "" {
			value = string(flow.LeverRegular)
		}
		lines = append(lines, fmt.Sprintf("%s%-12s %s", mark, stage, value))
	}
	return renderBox("levers", "per-stage autonomy", " esc close ", strings.Join(lines, "\n"))
}

func cycleLever(current string, delta int) string {
	index := 1
	for i, value := range leverCycle {
		if value == current {
			index = i
			break
		}
	}
	index = (index + delta + len(leverCycle)) % len(leverCycle)
	return leverCycle[index]
}
