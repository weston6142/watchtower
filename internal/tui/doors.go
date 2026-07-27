package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/wbushyeager/guildhall/internal/core"
	"github.com/wbushyeager/guildhall/internal/projection"
	"github.com/wbushyeager/guildhall/internal/store"
)

func renderDecisionsDoor(ds []projection.DecisionView, ids map[string]Identity, sel, width int) string {
	lines := []string{"DECISIONS · worst first · enter open · esc back"}
	if len(ds) == 0 {
		return boundedLines(append(lines, themeDim.Render("—")), width)
	}
	sel = min(max(sel, 0), len(ds)-1)
	for i, d := range ds {
		mark := "  "
		if i == sel {
			mark = "▸ "
		}
		identity := ids[d.IssueID]
		line := fmt.Sprintf("%s[%d] %s %s · %s", mark, d.ID, identity.Tag, d.Stage, d.Question)
		lines = append(lines, lipglossIdentity(identity, line))
	}
	return boundedLines(lines, width)
}

func lipglossIdentity(identity Identity, text string) string {
	if identity.Color == "" {
		return text
	}
	return identityStyle(identity).Render(text)
}

func identityStyle(identity Identity) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(identity.Color))
}

func renderProposalsDoor(ps []store.ProposalRow, sel, width int) string {
	lines := []string{"TRAY · enter accept · r reject · esc back"}
	if len(ps) == 0 {
		return boundedLines(append(lines, themeDim.Render("—")), width)
	}
	sel = min(max(sel, 0), len(ps)-1)
	for i, proposal := range ps {
		mark := "  "
		if i == sel {
			mark = "▸ "
		}
		lines = append(lines, fmt.Sprintf("%s%s", mark, proposal.Title))
		for _, line := range wrapProposal(proposal.Body, max(1, width-4)) {
			lines = append(lines, "  "+line)
		}
	}
	return boundedLines(lines, width)
}

func wrapProposal(text string, width int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []string
	for _, paragraph := range strings.Split(text, "\n") {
		words := strings.Fields(paragraph)
		line := ""
		for _, word := range words {
			if line != "" && len([]rune(line))+1+len([]rune(word)) > width {
				out = append(out, line)
				line = ""
			}
			if line != "" {
				line += " "
			}
			line += word
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func humanizeEvents(evs []core.Event, issueID string) []string {
	var lines []string
	for _, ev := range evs {
		if ev.IssueID != issueID {
			continue
		}
		stamp := ev.At.Format("15:04")
		var line string
		var payload map[string]any
		_ = json.Unmarshal(ev.Payload, &payload)
		text := func(key string) string {
			value, _ := payload[key].(string)
			return value
		}
		switch ev.Type {
		case core.EvIssueMerged:
			line = "merged to main"
		case core.EvStageStarted:
			line = text("stage") + " started"
			if agents, ok := payload["agents"].(float64); ok && agents > 0 {
				line += fmt.Sprintf(" (%d agents)", int(agents))
			}
		case core.EvStageCompleted:
			line = text("stage") + " completed"
		case core.EvStageFailed:
			line = text("stage") + " failed"
		case core.EvStageKilled:
			line = text("stage") + " paused after stop"
		case core.EvDecisionRequired:
			line = "question raised: " + text("question")
		case core.EvDecisionAnswered:
			line = "question answered"
		case core.EvIssuePaused:
			line = "issue paused"
		case core.EvIssueResumed:
			line = "issue resumed"
		case core.EvArtifactProduced:
			line = "artifact ready: " + text("artifact")
		case core.EvMergeSequenced:
			line = "merge sequenced"
		default:
			continue
		}
		lines = append(lines, stamp+" "+line)
	}
	return lines
}

func renderTextDoor(title string, lines []string, width int) string {
	out := []string{title + " · esc back"}
	if len(lines) == 0 {
		out = append(out, themeDim.Render("—"))
	} else {
		out = append(out, lines...)
	}
	return boundedLines(out, width)
}

func decisionViews(st *projection.State) []projection.DecisionView {
	if st == nil {
		return nil
	}
	ids := make([]int64, 0, len(st.Decisions))
	for id := range st.Decisions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	views := make([]projection.DecisionView, 0, len(ids))
	for _, id := range ids {
		views = append(views, st.Decisions[id])
	}
	return views
}
