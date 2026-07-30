package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/projection"
	"github.com/weston6142/watchtower/internal/store"
)

func renderDecisionsDoor(ds []projection.DecisionView, ids map[string]Identity, sel, width int) string {
	t := activeTheme
	lines := []string{panelTitle("Decisions", fmt.Sprintf("worst first · %d open", len(ds))), ""}
	if len(ds) == 0 {
		lines = append(lines, emptyDoorRow())
		return boundedLines(lines, width)
	}
	sel = min(max(sel, 0), len(ds)-1)
	for i, d := range ds {
		num := lipgloss.NewStyle().Foreground(t.Warn).Render(fmt.Sprintf("[%d]", d.ID))
		// Identity color stays on the tag only; the row body reads in Text.
		tag := lipglossIdentity(ids[d.IssueID], ids[d.IssueID].Tag)
		body := lipgloss.NewStyle().Foreground(t.Text).Render(d.Stage + " · " + d.Question)
		row := num + " " + tag + " " + body
		lines = append(lines, cursorRow(i == sel, truncate(row, max(1, width-4)), width))
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
	t := activeTheme
	lines := []string{panelTitle("Tray", fmt.Sprintf("%d proposal%s", len(ps), pluralSuffix(len(ps)))), ""}
	if len(ps) == 0 {
		lines = append(lines, emptyDoorRow())
		return boundedLines(lines, width)
	}
	sel = min(max(sel, 0), len(ps)-1)
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	for i, proposal := range ps {
		head := lipgloss.NewStyle().Foreground(t.Text)
		if i == sel {
			head = head.Foreground(t.Bright)
		}
		lines = append(lines, cursorRow(i == sel, head.Render(proposal.Title), width))
		for _, line := range wrapProposal(proposal.Body, max(1, width-6)) {
			lines = append(lines, "    "+dim.Render(line))
		}
		if len(proposal.DependsOn) > 0 {
			lines = append(lines, "    "+dim.Render("depends on "+strings.Join(proposal.DependsOn, ", ")))
		}
		if i < len(ps)-1 {
			lines = append(lines, "")
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

// renderTextDoor is a reading surface (timeline, transcript): bright title,
// dim count, body in plain text — no other decoration.
func renderTextDoor(title string, lines []string, width int) string {
	head := panelTitle(capitalizeDoor(title), fmt.Sprintf("%d line%s", len(lines), pluralSuffix(len(lines))))
	out := []string{head, ""}
	if len(lines) == 0 {
		out = append(out, emptyDoorRow())
	} else {
		out = append(out, lines...)
	}
	return boundedLines(out, width)
}

// Stream lines arrive as "<stage> │ <text>"; tool calls carry a "↳ " prefix.
// Both markers are written by internal/claude/stream.go.
const (
	streamGutterSep  = " │ "
	streamToolPrefix = "↳ "
)

// renderStreamDoor is the live agent view. Unlike the timeline it is watched
// while a stage runs, so it wears the same box chrome as the help overlay and
// gives its content typography: dim stage gutter, prose in Text, turn markers
// promoted from a line of prose into a rule.
func renderStreamDoor(subtitle string, lines []string, width int) string {
	t := activeTheme
	gutter := lipgloss.NewStyle().Foreground(t.Dimmer)
	prose := lipgloss.NewStyle().Foreground(t.Text)
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	inner := max(20, width-8) // border, padding, and the gutter's own width

	var body []string
	if len(lines) == 0 {
		body = append(body, gutter.Render("nothing here yet — either the stage just started or the transcript was lost to a daemon restart"))
	}
	for _, line := range lines {
		stage, text, found := strings.Cut(line, streamGutterSep)
		if !found {
			stage, text = "", line
		}
		// The turn marker is punctuation, not something to read.
		if strings.HasPrefix(strings.TrimSpace(text), "— turn complete") {
			body = append(body, gutter.Render(strings.Repeat("─", inner)))
			continue
		}
		lead := ""
		if stage != "" {
			lead = stage + streamGutterSep
		}
		if rest, ok := strings.CutPrefix(text, streamToolPrefix); ok {
			name, args, _ := strings.Cut(rest, "(")
			tool := lipgloss.NewStyle().Foreground(t.Structure).Render(name)
			if args != "" {
				tool += prose.Render("(" + args)
			}
			body = append(body, gutter.Render(lead)+dim.Render(streamToolPrefix)+tool)
			continue
		}
		wrapWidth := max(1, inner-lipgloss.Width(lead))
		pad := strings.Repeat(" ", lipgloss.Width(lead))
		for i, wrapped := range strings.Split(ansi.Wrap(text, wrapWidth, ""), "\n") {
			marker := pad
			if i == 0 {
				marker = lead
			}
			body = append(body, gutter.Render(marker)+prose.Render(wrapped))
		}
	}
	foot := keyChip("esc") + dim.Render(" close  ") + keyChip("q") + dim.Render(" quit")
	return renderBox("stream", subtitle, " esc close ", strings.Join(append(body, "", foot), "\n"))
}

// capitalizeDoor turns legacy ALL-CAPS door names into title case.
func capitalizeDoor(s string) string {
	if s == "" {
		return s
	}
	lower := strings.ToLower(s)
	return strings.ToUpper(lower[:1]) + lower[1:]
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
