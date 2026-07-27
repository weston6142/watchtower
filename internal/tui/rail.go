package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

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

// renderRail draws focused issue detail above the pending decision queue.
func renderRail(st *projection.State, ids map[string]Identity, det *proto.IssueDetail, width int) string {
	lines := []string{"RIGHT RAIL"}
	if det != nil {
		identity := ids[det.Issue.ID]
		lines = append(lines,
			fmt.Sprintf("%s %s", identity.Tag, det.Issue.ID),
			"state: "+det.Issue.State,
			"flow: "+det.Issue.Flow,
		)
		for _, run := range det.Runs {
			lines = append(lines, fmt.Sprintf("%s/%s %s %dt", run.Stage, run.Agent, run.Status, run.Tokens))
		}
		lines = append(lines, fmt.Sprintf("tokens: %d", det.Tokens))
		if len(det.Artifacts) > 0 {
			lines = append(lines, fmt.Sprintf("artifacts: %d", len(det.Artifacts)))
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

// renderToast draws the raised decision as a self-contained modal string.
func renderToast(d projection.DecisionView, id Identity, width int) string {
	lines := []string{
		fmt.Sprintf("DECISION [%d] %s %s", d.ID, id.Tag, d.Stage),
		d.Question,
	}
	for i, option := range d.Options {
		mark := "  "
		if i == d.Recommended {
			mark = "★ "
		}
		lines = append(lines, fmt.Sprintf("%s%s (%d)", mark, option, i))
	}
	lines = append(lines, "", "y accept ★ · n choose · o evidence · esc dismiss")
	content := boundedLines(lines, max(1, width-4))
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(id.Color)).Padding(1)
	return style.Render(content)
}
