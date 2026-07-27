package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/wbushyeager/guildhall/internal/projection"
	"github.com/wbushyeager/guildhall/internal/proto"
)

var (
	themeDim   = lipgloss.NewStyle().Faint(true)
	themeLabel = lipgloss.NewStyle().Bold(true)
	statusBad  = "#e06c75"
	statusWarn = "#f2c14e"
	statusOk   = "#98c379"
)

func styleStatusBad() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(statusBad))
}
func styleStatusWarn() lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(statusWarn))
}
func styleStatusOk() lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(statusOk)) }

func renderHeader(ov *proto.Overview, width int) string {
	if ov == nil {
		ov = &proto.Overview{}
	}
	var attention []string
	if ov.Failing > 0 {
		attention = append(attention, fmt.Sprintf("%d build%s failing", ov.Failing, pluralSuffix(ov.Failing)))
	}
	if ov.NeedYou > 0 {
		attention = append(attention, fmt.Sprintf("%d question%s for you", ov.NeedYou, pluralSuffix(ov.NeedYou)))
	}
	if len(attention) == 0 {
		attention = append(attention, "all clear")
	}
	var details []string
	if ov.Building > 0 {
		details = append(details, fmt.Sprintf("%d building", ov.Building))
	}
	if ov.ShippedToday > 0 {
		details = append(details, fmt.Sprintf("%d shipped today", ov.ShippedToday))
	}
	if ov.TokensTotal > 0 {
		details = append(details, fmt.Sprintf("%s tokens", compactTokens(ov.TokensTotal)))
	}
	if ov.DollarsTotal > 0 {
		details = append(details, fmt.Sprintf("~$%.2f", ov.DollarsTotal))
	}
	text := strings.Join(attention, ", ")
	if len(details) > 0 {
		text += " — " + strings.Join(details, ", ")
	}
	style := styleStatusOk()
	if ov.Failing > 0 {
		style = styleStatusBad()
	} else if ov.NeedYou > 0 {
		style = styleStatusWarn()
	}
	return truncate(style.Render("●")+" "+text, width)
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func compactTokens(tokens int) string {
	if tokens >= 1000 {
		return fmt.Sprintf("%.0fk", float64(tokens)/1000)
	}
	return fmt.Sprintf("%d", tokens)
}

func renderNoticeRow(st *projection.State, width int) string {
	if st == nil || len(st.Notices) == 0 {
		if width > 0 {
			return strings.Repeat(" ", width)
		}
		return " "
	}
	text := strings.ReplaceAll(st.Notices[len(st.Notices)-1].Text, "\n", " ")
	return truncate(text, width)
}

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

func truncate(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	// ansi.Truncate is display-width-aware and never splits escape
	// sequences or wide runes (emoji flags would break a byte slice).
	return ansi.Truncate(s, width, "…")
}

const laneWidth = 14

func padCell(s string, width int) string {
	s = truncate(s, width)
	return s + strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
}

func titleLines(title string) [2]string {
	runes := []rune(strings.TrimSpace(title))
	if len(runes) > 24 {
		runes = runes[:24]
	}
	var lines [2]string
	for i := 0; i < 2 && len(runes) > 0; i++ {
		if len(runes) <= 12 {
			lines[i] = string(runes)
			break
		}
		cut := 12
		for j := 12; j > 0; j-- {
			if runes[j] == ' ' {
				cut = j
				break
			}
		}
		lines[i] = string(runes[:cut])
		runes = []rune(strings.TrimSpace(string(runes[cut:])))
	}
	return lines
}

func stageName(aliases map[string]string, stage string) string {
	if aliases != nil && aliases[stage] != "" {
		return aliases[stage]
	}
	return stage
}

func completedStage(iv *projection.IssueView, stage string) bool {
	for _, completed := range iv.Completed {
		if completed == stage {
			return true
		}
	}
	return false
}

func cellContent(iv *projection.IssueView, ids map[string]Identity, stageIdx, tick int, focused bool) string {
	return cellContentForStage(iv, ids, "", stageIdx, tick, focused, false)
}

func cellContentForStage(iv *projection.IssueView, ids map[string]Identity, stage string, stageIdx, tick int, focused, reducedMotion bool) string {
	if iv == nil {
		return ""
	}
	identity := ids[iv.ID]
	identityStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(identity.Color))
	if focused {
		identityStyle = identityStyle.Bold(true)
	}
	if iv.Merged && (stage == "merge" || stageIdx == len(iv.Completed)) {
		return identityStyle.Render("⇡")
	}
	if iv.Behind != "" && stage == "merge" {
		blocker := iv.Behind
		if blockerIdentity, ok := ids[iv.Behind]; ok {
			return themeDim.Render("after ") + lipgloss.NewStyle().Foreground(lipgloss.Color(blockerIdentity.Color)).Render("▐"+blockerIdentity.Tag+"▌")
		}
		return themeDim.Render("after " + blocker)
	}
	if iv.Paused || iv.Killed || iv.State == "paused" {
		return themeDim.Render("paused ⏸")
	}
	if iv.CurrentStage == stage && iv.State == "waiting_decision" {
		return styleStatusWarn().Render("NEED-YOU ◔")
	}
	if iv.CurrentStage == stage && iv.State == "failed" {
		style := styleStatusBad()
		if !reducedMotion && tick%2 == 1 {
			style = style.Reverse(true)
		}
		return style.Render("FAILED ✗")
	}
	if iv.State == "queued_for_slot" && iv.CurrentStage == stage {
		return themeDim.Render("queued ⧗")
	}
	if iv.CurrentStage == stage && iv.State == "running" {
		spinner := "◌"
		if !reducedMotion && (tick/2)%2 == 1 {
			spinner = "○"
		}
		return themeDim.Render(spinner)
	}
	if completedStage(iv, stage) {
		return identityStyle.Render("✓")
	}
	return themeDim.Render("·")
}

func headerCell(content string, identity Identity, focused bool) string {
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(identity.Color))
	if focused {
		style = style.Bold(true)
	}
	return padCell(style.Render(content), laneWidth)
}

func renderTower(st *projection.State, stages []string, ids map[string]Identity, focus Focus, tick, width int) string {
	return renderTowerConfigured(st, stages, ids, focus, nil, false, tick, width)
}

func renderTowerConfigured(st *projection.State, stages []string, ids map[string]Identity, focus Focus, aliases map[string]string, reducedMotion bool, tick, width int) string {
	if st == nil {
		st = projection.NewState()
	}
	lines := []string{warRoom(st, ids), "MAP · " + mapInsight(st.Issues) + " · a full map"}
	if len(st.Order) == 0 {
		for _, stage := range stages {
			lines = append(lines, themeLabel.Render(strings.ToUpper(stageName(aliases, stage)))+"  "+themeDim.Render("—"))
		}
		lines = append(lines, themeDim.Render("◌ working · ✓ done · ✗ FAILED · ◔ your turn · ▼ merging · ⇡ shipped · ? help"))
		return boundedLines(lines, width)
	}
	var chips, first, second, issueIDs []string
	for _, id := range st.Order {
		iv := st.Issues[id]
		if iv == nil {
			continue
		}
		issueIDs = append(issueIDs, id)
		identity := ids[id]
		marker := " "
		if focus.Issue == id {
			marker = "▸"
		}
		chips = append(chips, headerCell(marker+identity.Tag, identity, focus.Issue == id))
		wrapped := titleLines(iv.Title)
		first = append(first, headerCell(wrapped[0], identity, focus.Issue == id))
		second = append(second, headerCell(wrapped[1], identity, focus.Issue == id))
	}
	// The stage label occupies the left gutter; each issue lane remains fixed at
	// fourteen cells so transient status text can never reflow the grid.
	lines = append(lines, padCell("", 12)+" "+strings.Join(chips, ""))
	lines = append(lines, padCell("", 12)+" "+strings.Join(first, ""))
	lines = append(lines, padCell("", 12)+" "+strings.Join(second, ""))
	var idRow []string
	for _, id := range issueIDs {
		identity := ids[id]
		idRow = append(idRow, headerCell(id, identity, focus.Issue == id))
	}
	lines = append(lines, padCell("", 12)+" "+strings.Join(idRow, ""))
	for stageIdx, stage := range stages {
		label := padCell(strings.ToUpper(stageName(aliases, stage)), 12) + " "
		var cells []string
		for _, id := range issueIDs {
			cells = append(cells, padCell(cellContentForStage(st.Issues[id], ids, stage, stageIdx, tick, focus.Issue == id, reducedMotion), laneWidth))
		}
		lines = append(lines, label+strings.Join(cells, ""))
	}
	lines = append(lines, themeDim.Render("◌ working · ✓ done · ✗ FAILED · ◔ your turn · ▼ merging · ⇡ shipped · ? help"))
	return boundedLines(lines, width)
}

func warRoom(st *projection.State, ids map[string]Identity) string {
	if st == nil {
		return "shipping order: — · builders 0/4 busy · ideas 0 · questions 0"
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
	} else if len(lane) > 3 {
		lane = append(lane[:3], fmt.Sprintf("+%d", len(lane)-3))
	}
	busy := 0
	for _, iv := range st.Issues {
		if iv.State == "running" && iv.CurrentStage != "" {
			busy++
		}
	}
	return fmt.Sprintf("shipping order: %s · builders %d/4 busy · ideas %d · questions %d",
		strings.Join(lane, "→"), busy, st.ProposalCount, len(st.Decisions))
}
