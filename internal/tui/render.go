package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/wbushyeager/guildhall/internal/projection"
)

var (
	themeDim   = lipgloss.NewStyle().Faint(true)
	themeLabel = lipgloss.NewStyle().Bold(true)
	statusBad  = "#e06c75"
	statusWarn = "#f2c14e"
	statusOk   = "#98c379"
)

func styleStatusBad() lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(statusBad)) }
func styleStatusWarn() lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(statusWarn)) }
func styleStatusOk() lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(statusOk)) }

// floorCards returns issue IDs on a rendered stage floor in creation order.
func floorCards(st *projection.State, stages []string, floor int) []string {
	if st == nil || floor <= 0 || floor > len(stages) {
		return nil
	}
	stage := stages[floor-1]
	var out []string
	for _, id := range st.Order {
		iv := st.Issues[id]
		if iv == nil {
			continue
		}
		current := iv.CurrentStage
		if iv.State == "done" || iv.Merged {
			current = stages[len(stages)-1]
		}
		if current == stage {
			out = append(out, id)
		}
	}
	return out
}

func issueGlyph(iv *projection.IssueView) string {
	if iv.Merged {
		return "⇡"
	}
	switch iv.State {
	case "waiting_decision":
		return "◔"
	case "queued_for_slot":
		return "⧗"
	case "failed":
		return "✗"
	case "done":
		return "✓"
	default:
		return "●"
	}
}

// progressBarWidth is the number of cells in a card's stage-progress bar.
const progressBarWidth = 5

func progressBar(iv *projection.IssueView, total int) string {
	if total <= 0 {
		return strings.Repeat("░", progressBarWidth)
	}
	filled := len(iv.Completed) * progressBarWidth / total
	if filled > progressBarWidth {
		filled = progressBarWidth
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", progressBarWidth-filled)
}

func card(iv *projection.IssueView, identity Identity, focused bool, stages int, ids map[string]Identity) string {
	if iv == nil {
		return ""
	}
	flags := make([]string, 0, 2)
	if iv.Behind != "" {
		tag := iv.Behind
		if behind, ok := ids[iv.Behind]; ok {
			tag = behind.Tag
		}
		flags = append(flags, "🔒behind:"+tag)
	}
	if iv.Unmerged {
		flags = append(flags, "!unmerged")
	}
	text := fmt.Sprintf("▐%s %s %s %s", identity.Tag, iv.ID, issueGlyph(iv), progressBar(iv, stages))
	if len(flags) > 0 {
		text += " " + strings.Join(flags, " ")
	}
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(identity.Color))
	if focused {
		style = style.Bold(true).BorderLeft(true).PaddingLeft(1)
	}
	return style.Render(text)
}

func floorLine(name string, cards []string, width int) string {
	label := themeLabel.Render(strings.ToUpper(name))
	if len(cards) == 0 {
		return label + "  " + themeDim.Render("—")
	}
	line := label + "  " + strings.Join(cards, "  ")
	if width > 0 && lipgloss.Width(line) > width {
		return truncate(line, width)
	}
	return line
}

func truncate(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	// ansi.Truncate is display-width-aware and never splits escape
	// sequences or wide runes (emoji flags would break a byte slice).
	return ansi.Truncate(s, width, "…")
}

func warRoom(st *projection.State, ids map[string]Identity) string {
	if st == nil {
		return "⚖ MERGE LANE: —  ·  SLOTS busy:0  ·  TRAY 0  ·  DECISIONS 0"
	}
	var lane []string
	for _, id := range st.Order {
		iv := st.Issues[id]
		if iv == nil {
			continue
		}
		if iv.Merged || iv.CurrentStage == "merge" || iv.Behind != "" || iv.State == "done" {
			tag := id
			if identity, ok := ids[id]; ok {
				tag = identity.Tag
			}
			if iv.Behind != "" {
				tag = themeDim.Render(tag)
			}
			lane = append(lane, tag)
		}
	}
	if len(lane) == 0 {
		lane = []string{"—"}
	}
	busy := 0
	for _, iv := range st.Issues {
		if iv.State == "queued_for_slot" {
			busy++
		}
	}
	return fmt.Sprintf("⚖ MERGE LANE: %s  ·  SLOTS busy:%d  ·  TRAY %d  ·  DECISIONS %d",
		strings.Join(lane, " ⇢ "), busy, st.ProposalCount, len(st.Decisions))
}

// renderTower draws the war room and stage floors from top to bottom.
func renderTower(st *projection.State, stages []string, ids map[string]Identity, focus Focus, width int) string {
	lines := []string{warRoom(st, ids)}
	for floor, stage := range stages {
		issueIDs := floorCards(st, stages, floor+1)
		cards := make([]string, 0, len(issueIDs))
		for _, id := range issueIDs {
			iv := st.Issues[id]
			identity := ids[id]
			cards = append(cards, card(iv, identity, focus.Issue == id, len(stages), ids))
		}
		lines = append(lines, floorLine(stage, cards, width))
	}
	return strings.Join(lines, "\n")
}
