package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/wbushyeager/guildhall/internal/evidence"
	"github.com/wbushyeager/guildhall/internal/projection"
	"github.com/wbushyeager/guildhall/internal/proto"
)

func boundedLines(lines []string, width int) string {
	if width <= 0 {
		return strings.Join(lines, "\n")
	}
	bounded := make([]string, len(lines))
	for i, line := range lines {
		bounded[i] = truncate(line, width)
	}
	return strings.Join(bounded, "\n")
}

// renderRail draws the focused issue's plain-language FOCUS panel above the
// pending decision queue. Paths and session IDs intentionally stay here: they
// are diagnostic details, not grid copy.
func renderRail(st *projection.State, ids map[string]Identity, det *proto.IssueDetail, width int) string {
	lines := []string{"FOCUS"}
	if det != nil {
		identity := ids[det.Issue.ID]
		lines = append(lines,
			fmt.Sprintf("%s %s · %s", identity.Tag, det.Issue.ID, det.Issue.Title),
			fmt.Sprintf("%s · %s", det.Issue.Flow, focusStatus(det.Issue.State)),
		)
		if strings.HasPrefix(det.Issue.State, "failed") {
			lines = append(lines, fmt.Sprintf("error: %s · attempt %d of %d", det.LastError, det.Attempt, det.AttemptOf))
		}
		if det.Budget > 0 {
			percent := det.Tokens * 100 / det.Budget
			percent = max(0, min(100, percent))
			filled := (percent*4 + 50) / 100
			bar := strings.Repeat("▰", filled) + strings.Repeat("▱", 4-filled)
			cost := ""
			if det.Dollars > 0 {
				cost = fmt.Sprintf(" ($%.2f)", det.Dollars)
			}
			lines = append(lines, fmt.Sprintf("budget %s %d%%%s", bar, percent, cost))
		} else {
			lines = append(lines, "spent "+compactTokens(det.Tokens)+" tokens")
		}
		if leverLine := renderLeverLine(det.Levers); leverLine != "" {
			lines = append(lines, leverLine)
		}
		for i := len(det.Runs) - 1; i >= 0; i-- {
			run := det.Runs[i]
			if run.Worktree != "" || run.SessionID != "" {
				lines = append(lines, themeDim.Render(fmt.Sprintf("%s · session %s", run.Worktree, run.SessionID)))
				break
			}
		}
	}
	lines = append(lines, "", "DECISION QUEUE")
	if st == nil || len(st.Decisions) == 0 {
		lines = append(lines, themeDim.Render("—"))
	} else {
		idsInQueue := make([]int64, 0, len(st.Decisions))
		for id := range st.Decisions {
			idsInQueue = append(idsInQueue, id)
		}
		sort.Slice(idsInQueue, func(i, j int) bool { return idsInQueue[i] < idsInQueue[j] })
		for i, id := range idsInQueue {
			d := st.Decisions[id]
			identity := ids[d.IssueID]
			mark := "  "
			if i == 0 {
				mark = "▶ "
			}
			lines = append(lines, fmt.Sprintf("%s[%d] %s %s — %s", mark, d.ID, identity.Tag, d.Stage, d.Question))
		}
	}
	return boundedLines(lines, width)
}

func focusStatus(state string) string {
	switch {
	case strings.HasPrefix(state, "running"):
		return "building"
	case strings.HasPrefix(state, "queued"):
		return "queued"
	case strings.HasPrefix(state, "waiting"):
		return "needs you"
	case strings.HasPrefix(state, "failed"):
		return "failed"
	case strings.HasPrefix(state, "paused"):
		return "paused"
	case state == "done":
		return "shipped"
	default:
		return state
	}
}

func renderLeverLine(levers map[string]string) string {
	if len(levers) == 0 {
		return ""
	}
	stages := orderedLeverStages(levers)
	parts := make([]string, 0, len(stages))
	for _, stage := range stages {
		value := levers[stage]
		label := value
		switch value {
		case "yolo":
			label = "auto"
		case "strict":
			label = "you"
		}
		letter := "?"
		if stage != "" {
			letter = strings.ToUpper(string([]rune(stage)[0]))
		}
		parts = append(parts, letter+":"+label)
	}
	return "levers " + strings.Join(parts, " ")
}

func orderedLeverStages(levers map[string]string) []string {
	preferred := []string{"brainstorm", "spec", "plan", "execute", "review", "merge"}
	seen := map[string]bool{}
	stages := make([]string, 0, len(levers))
	for _, stage := range preferred {
		if _, ok := levers[stage]; ok {
			stages = append(stages, stage)
			seen[stage] = true
		}
	}
	remaining := make([]string, 0, len(levers)-len(stages))
	for stage := range levers {
		if !seen[stage] {
			remaining = append(remaining, stage)
		}
	}
	sort.Strings(remaining)
	return append(stages, remaining...)
}

// renderToast draws the raised decision as a self-contained modal string.
func renderToast(d projection.DecisionView, id Identity, streak, width int) string {
	lines := []string{
		fmt.Sprintf("DECISION [%d] %s %s", d.ID, id.Tag, d.Stage),
		d.Question,
	}
	if d.Why != "" {
		lines = append(lines, "why: "+d.Why)
	}
	for i, option := range d.Options {
		mark := "  "
		if i == d.Recommended {
			mark = "★ "
		}
		line := fmt.Sprintf("%s%s (%d)", mark, option, i)
		if i < len(d.Consequences) && d.Consequences[i] != "" {
			line += " → " + d.Consequences[i]
		}
		lines = append(lines, line)
	}
	if d.Reversible != "" {
		lines = append(lines, "reversible: "+d.Reversible)
	}
	if streak >= 3 {
		lines = append(lines, "", fmt.Sprintf("you've accepted %d recommendations in a row without opening evidence", streak))
	}
	lines = append(lines, "", "y accept ★ · n choose · o evidence · esc dismiss")
	content := boundedLines(lines, max(1, width-4))
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(id.Color)).Padding(1)
	return style.Render(content)
}

func renderEvidence(b evidence.Bundle, title string, width int) string {
	return renderEvidenceDetails(b, title, "", nil, width)
}

func renderEvidenceDetails(b evidence.Bundle, title, lastError string, artifacts []string, width int) string {
	lines := []string{
		"EVIDENCE · " + title,
		fmt.Sprintf("%d files · +%d · −%d", len(b.Files), b.Added, b.Removed),
	}
	if b.Biggest != "" {
		lines = append(lines, "biggest: "+b.Biggest)
	}
	if len(b.AreaWeight) > 0 {
		type areaStat struct {
			name   string
			weight int
		}
		areas := make([]areaStat, 0, len(b.AreaWeight))
		for name, weight := range b.AreaWeight {
			areas = append(areas, areaStat{name: name, weight: weight})
		}
		sort.Slice(areas, func(i, j int) bool {
			if areas[i].weight == areas[j].weight {
				return areas[i].name < areas[j].name
			}
			return areas[i].weight > areas[j].weight
		})
		lines = append(lines, "areas:")
		for _, area := range areas[:min(5, len(areas))] {
			lines = append(lines, fmt.Sprintf("  %s %d", area.name, area.weight))
		}
	}
	if lastError != "" {
		lines = append(lines, "error: "+lastError)
	} else if len(artifacts) > 0 {
		lines = append(lines, "tests: see available review artifacts")
	} else {
		lines = append(lines, "tests: no test result artifact available")
	}
	lines = append(lines, "enter diff · esc back")
	return boundedLines(lines, width)
}

func renderEvidenceFallback(d projection.DecisionView, det *proto.IssueDetail, width int) string {
	lines := []string{"EVIDENCE · " + d.IssueID, "no evidence bundle yet"}
	if len(d.Paths) > 0 {
		lines = append(lines, "paths:")
		for _, path := range d.Paths {
			lines = append(lines, "  "+path)
		}
	}
	if det != nil && len(det.Artifacts) > 0 {
		lines = append(lines, "available artifacts:")
		for _, artifact := range det.Artifacts {
			lines = append(lines, "  "+artifact)
		}
	} else {
		lines = append(lines, "available artifacts: none")
	}
	lines = append(lines, "esc back")
	return boundedLines(lines, width)
}
