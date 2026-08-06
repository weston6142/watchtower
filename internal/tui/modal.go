package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/priority"
)

type modalEditor struct {
	Value string
	Caret int // rune offset between 0 and len([]rune(Value))
}

func newModalEditor(value string) modalEditor {
	return modalEditor{Value: value, Caret: len([]rune(value))}
}

func (e *modalEditor) handle(key string) bool {
	runes := []rune(e.Value)
	e.Caret = max(0, min(e.Caret, len(runes)))
	switch key {
	case "left":
		if e.Caret > 0 {
			e.Caret--
		}
	case "right":
		if e.Caret < len(runes) {
			e.Caret++
		}
	case "up":
		e.moveVertical(runes, -1)
	case "down":
		e.moveVertical(runes, 1)
	case "backspace":
		if e.Caret > 0 {
			runes = append(runes[:e.Caret-1], runes[e.Caret:]...)
			e.Caret--
			e.Value = string(runes)
		}
	case "tab":
		return true
	default:
		if key != "" && !strings.ContainsAny(key, "\n\r\t") {
			insert := []rune(key)
			runes = append(runes[:e.Caret], append(insert, runes[e.Caret:]...)...)
			e.Caret += len(insert)
			e.Value = string(runes)
		}
	}
	return true
}

func (e modalEditor) styledCaretText() string {
	runes := []rune(e.Value)
	caret := max(0, min(e.Caret, len(runes)))
	bright := lipgloss.NewStyle().Foreground(activeTheme.Bright)
	accent := lipgloss.NewStyle().Foreground(activeTheme.Accent)
	return styleModalLines(bright, string(runes[:caret])) + accent.Render("▏") + styleModalLines(bright, string(runes[caret:]))
}

func styleModalLines(style lipgloss.Style, value string) string {
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = style.Render(line)
		}
	}
	return strings.Join(lines, "\n")
}

func (e *modalEditor) lineBounds(runes []rune) (start, end int) {
	for i := 0; i < e.Caret; i++ {
		if runes[i] == '\n' {
			start = i + 1
		}
	}
	end = len(runes)
	for i := e.Caret; i < len(runes); i++ {
		if runes[i] == '\n' {
			end = i
			break
		}
	}
	return start, end
}

func (e *modalEditor) moveVertical(runes []rune, delta int) {
	start, end := e.lineBounds(runes)
	column := e.Caret - start
	var targetStart, targetEnd int
	if delta < 0 {
		if start == 0 {
			return
		}
		targetEnd = start - 1
		targetStart = 0
		for i := targetEnd - 1; i >= 0; i-- {
			if runes[i] == '\n' {
				targetStart = i + 1
				break
			}
		}
	} else {
		if end == len(runes) {
			return
		}
		targetStart = end + 1
		targetEnd = len(runes)
		for i := targetStart; i < len(runes); i++ {
			if runes[i] == '\n' {
				targetEnd = i
				break
			}
		}
	}
	e.Caret = targetStart + min(column, targetEnd-targetStart)
}

// modalState holds the modal payload fields and their transient editor state.
// Editor state stays local to the TUI and is never included in commands.
type modalState struct {
	Title       string
	Body        string
	Field       int
	FlowName    string
	Preset      string
	editors     [modalEditableFieldCount]modalEditor
	editorReady [modalEditableFieldCount]bool
	// DependsOn is comma-separated issue IDs. The command boundary trims and
	// deduplicates them before graph validation.
	DependsOn string
	// Attach is the raw field text: comma-separated paths, or the stored name
	// of an existing attachment to retain it. Resolution happens client-side
	// before the command is sent.
	Attach string
	// OrigAttach is the stored names this issue already has, so a bare name in
	// Attach is understood as "keep that one" rather than a relative path.
	OrigAttach []string
	// Priority is the level's stored int, not text: the modal is a chooser, so
	// there is nothing to parse and no way to hold an unparseable value. The
	// zero value is priority.Levels' normal, which is what a new issue wants.
	Priority int
	EditID   string // non-empty: editing this backlog draft instead of creating
	// FromBacklog records that the backlog opened this modal, so esc and a
	// successful submit go back there instead of dumping the operator on the
	// grid. It is a return-destination marker only: renderModal never reads it
	// and submit dispatch never branches on it.
	FromBacklog bool
}

func newModalState(state modalState) *modalState {
	for field := 0; field < modalEditableFieldCount; field++ {
		state.editors[field] = newModalEditor(state.fieldValueFor(field))
		state.editorReady[field] = true
	}
	return &state
}

func (m modalState) input(key string) modalState {
	if key == "tab" {
		m.Field = (m.Field + 1) % modalFieldCount
		return m
	}
	if editor, ok := (&m).editorForField(m.Field); ok {
		editor.handle(key)
		m.setFieldValueFor(m.Field, editor.Value)
	}
	return m
}

func isModalEditableField(field int) bool {
	return field >= 0 && field < modalEditableFieldCount
}

func (m *modalState) editorForField(field int) (*modalEditor, bool) {
	if !isModalEditableField(field) {
		return nil, false
	}
	value := m.fieldValueFor(field)
	if !m.editorReady[field] || m.editors[field].Value != value {
		m.editors[field] = newModalEditor(value)
		m.editorReady[field] = true
	}
	return &m.editors[field], true
}

func (m *modalState) setFieldValueFor(field int, value string) {
	switch field {
	case 0:
		m.Title = value
	case 1:
		m.Body = value
	case 2:
		m.FlowName = value
	case 3:
		m.Preset = value
	case dependenciesField:
		m.DependsOn = value
	case attachField:
		m.Attach = value
	}
}

func (m modalState) fieldValue() string {
	return m.fieldValueFor(m.Field)
}

func (m modalState) fieldValueFor(field int) string {
	switch field {
	case 0:
		return m.Title
	case 1:
		return m.Body
	case 2:
		return m.FlowName
	case 3:
		return m.Preset
	case dependenciesField:
		return m.DependsOn
	case attachField:
		return m.Attach
	default:
		return ""
	}
}

// renderBox wraps content in the shared overlay chrome: a Bg2 header band
// holding the bright title, dim subtitle, and inverted accent chip, over an
// accent-bordered body. Every overlay uses this box.
func renderBox(title, sub, chipText, content string) string {
	t := activeTheme
	head := lipgloss.NewStyle().Foreground(t.Bright).Background(t.Bg2).Bold(true).Render(" " + title)
	if sub != "" {
		head += lipgloss.NewStyle().Foreground(t.Dim).Background(t.Bg2).Render(" " + sub)
	}
	chip := lipgloss.NewStyle().Foreground(t.Bg0).Background(t.Accent).Bold(true).Render(chipText)
	inner := max(lipgloss.Width(content), lipgloss.Width(head)+lipgloss.Width(chip)+2)
	gap := max(1, inner-lipgloss.Width(head)-lipgloss.Width(chip))
	band := head +
		lipgloss.NewStyle().Background(t.Bg2).Render(strings.Repeat(" ", gap)) +
		chip
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(t.Accent).
		Padding(0, 1).
		Render(band + "\n\n" + content)
}

func renderModal(m modalState, width int) string {
	flowName := m.FlowName
	if flowName == "" {
		flowName = "default"
		m.FlowName = flowName
	}
	preset := m.Preset
	if preset == "" {
		preset = string(flow.LeverRegular)
		m.Preset = preset
	}
	dim := lipgloss.NewStyle().Foreground(activeTheme.Dim)
	submit := keyChip("enter") + dim.Render(" create  ") + keyChip("ctrl+s") + dim.Render(" backlog")
	boxTitle := "new issue"
	if m.EditID != "" {
		submit = keyChip("enter") + dim.Render(" save")
		boxTitle = "edit issue"
	}
	lines := []string{
		m.renderModalField(0, "title", m.Title, true),
		m.renderModalField(1, "body", m.Body, false),
		m.renderModalField(2, "flow", flowName, false),
		m.renderModalField(3, "preset", preset, false),
		m.renderModalField(dependenciesField, "depends on", m.DependsOn, false),
		m.renderModalField(attachField, "attach", m.Attach, false),
		modalChoiceField(m.Field == priorityField, "priority", priority.Label(m.Priority)),
		"",
		keyChip("tab") + dim.Render(" next field  ") + keyChip("h/l") + dim.Render(" adjust  ") + submit,
	}
	return renderBox(boxTitle, "", " esc cancel ", boundedLines(lines, max(1, width-6)))
}

const modalFieldWidth = 44

// Dependency and attachment values are text fields; priority is a selector,
// not an input, and is last so tab wraps after it.
const (
	dependenciesField       = 4
	attachField             = 5
	priorityField           = 6
	modalEditableFieldCount = priorityField
	modalFieldCount         = priorityField + 1
)

// modalChoiceField renders a fixed-choice field as the lever editor's
// ◂ value ▸ control rather than modalField's caret: a caret invites typing, and
// this field takes none.
func modalChoiceField(selected bool, name, value string) string {
	t := activeTheme
	labelLine := lipgloss.NewStyle().Foreground(t.Dim).Render(strings.ToUpper(name))
	border := t.Dimmer
	body := lipgloss.NewStyle().Foreground(t.Structure).Render(value)
	if selected {
		border = t.Accent
		arrow := lipgloss.NewStyle().Foreground(t.Accent)
		body = arrow.Render("◂ ") + lipgloss.NewStyle().Foreground(t.Bright).Render(value) + arrow.Render(" ▸")
	}
	field := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(modalFieldWidth).Render(body)
	return labelLine + "\n" + field
}

func (m *modalState) renderModalField(field int, name, value string, required bool) string {
	var editor *modalEditor
	if m.Field == field {
		editor, _ = m.editorForField(field)
	}
	return modalField(m.Field == field, name, value, required, editor)
}

// modalField renders a labelled input: dim uppercase label over a bordered
// value box; the active field gets an accent border and a block caret.
func modalField(selected bool, name, value string, required bool, editor *modalEditor) string {
	t := activeTheme
	label := strings.ToUpper(name)
	if required {
		label += " *"
	}
	labelLine := lipgloss.NewStyle().Foreground(t.Dim).Render(label)
	border := t.Dimmer
	body := lipgloss.NewStyle().Foreground(t.Text).Render(value)
	if value == "" {
		body = lipgloss.NewStyle().Foreground(t.Dimmer).Render("…")
	}
	if selected {
		border = t.Accent
		if editor != nil {
			body = editor.styledCaretText()
		} else {
			body = lipgloss.NewStyle().Foreground(t.Bright).Render(value) +
				lipgloss.NewStyle().Foreground(t.Accent).Render("▏")
		}
	}
	if strings.Contains(body, "\n") {
		lines := strings.Split(body, "\n")
		for i, line := range lines {
			lines[i] = line + strings.Repeat(" ", max(0, modalFieldWidth-lipgloss.Width(line)))
		}
		body = strings.Join(lines, "\n")
	}
	field := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(border).
		Padding(0, 1).Width(modalFieldWidth).Render(body)
	return labelLine + "\n" + field
}

func renderConfirm(prompt string, width int) string {
	dim := lipgloss.NewStyle().Foreground(activeTheme.Dim)
	hint := keyChip("y") + dim.Render(" confirm")
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
	t := activeTheme
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	const rowWidth = 40
	var lines []string
	for i, stage := range stages {
		value := matrix[stage]
		if value == "" {
			value = string(flow.LeverRegular)
		}
		// Values are nouns, not states: Structure cyan-blue; the selected
		// lever's value becomes an adjustable ◂ value ▸ control.
		styled := lipgloss.NewStyle().Foreground(t.Structure).Render(value)
		if i == sel {
			arrow := lipgloss.NewStyle().Foreground(t.Accent)
			styled = arrow.Render("◂ ") + lipgloss.NewStyle().Foreground(t.Bright).Render(value) + arrow.Render(" ▸")
		}
		name := padCell(stage, 14)
		pad := max(1, rowWidth-lipgloss.Width(name)-lipgloss.Width(styled))
		lines = append(lines, cursorRow(i == sel, name+strings.Repeat(" ", pad)+styled, rowWidth+4))
	}
	lines = append(lines, "", keyChip("j/k")+dim.Render(" lever  ")+keyChip("h/l")+dim.Render(" adjust  ")+keyChip("enter")+dim.Render(" apply"))
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
