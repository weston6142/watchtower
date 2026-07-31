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

const streamEmptyNotice = "nothing here yet — either the stage just started or the transcript was lost to a daemon restart"

// streamState is the stream door's reading position. Follow is the default —
// the newest output is what you opened the door to see. Top is only consulted
// once the operator has scrolled off the bottom.
//
// The zero value therefore means detached at row 0, not following: Follow is
// derived from the clamped Top, and g legitimately produces {Top: 0,
// Follow: false} on a clipping body, so the two cannot be told apart from the
// field values. Every construction site sets Follow: true explicitly.
type streamState struct {
	Top    int  // first visible body row, when detached
	Follow bool // pinned to newest; the refetch only runs while true
}

// scroll moves the reading position over rendered body rows. The vocabulary
// mirrors pagerState.scroll so the two reading surfaces share muscle memory.
func (s streamState) scroll(key string, rows, total int) streamState {
	if rows < 1 {
		rows = 1
	}
	maxTop := max(0, total-rows)
	if s.Follow {
		s.Top = maxTop // detaching starts from where the eye already is
	}
	switch key {
	case "j":
		s.Top++
	case "k":
		s.Top--
	case "d":
		s.Top += rows / 2
	case "u":
		s.Top -= rows / 2
	case "g":
		s.Top = 0
	case "G":
		s.Top = maxTop
	}
	s.Top = min(max(s.Top, 0), maxTop)
	// Derived, not toggled: this single line is what makes "back at the bottom
	// means live" and "a body that fits never detaches" true by construction.
	s.Follow = s.Top >= maxTop
	return s
}

// streamChromeRows is what a stream-door screen spends on chrome rather than
// body. Verified row for row against testdata/stream-wide.golden, which is 15
// rows for a 5-row body: 1 header + 1 notice row + 4 renderBox (top border,
// title band, blank, bottom border) + 2 inside the box (the door's own blank
// line and footer) + 2 under it (blank line and keybar).
const streamChromeRows = 10

// streamInner is the door's usable content width: the border, the padding and
// the gutter's own width come off the screen width.
func streamInner(width int) int { return max(20, width-8) }

// streamRows is how many body rows fit. A non-positive height means no
// WindowSizeMsg has landed yet, so it borrows backlogFallbackRows rather than
// spelling a second literal that could drift from it.
func streamRows(height int) int {
	if height <= 0 {
		height = backlogFallbackRows
	}
	return max(1, height-streamChromeRows)
}

// renderStreamDoor is the live agent view. Unlike the timeline it is watched
// while a stage runs, so it wears the same box chrome as the help overlay and
// gives its content typography: dim stage gutter, prose in Text, turn markers
// promoted from a line of prose into a rule.
func renderStreamDoor(subtitle string, lines []string, width int) string {
	dim := lipgloss.NewStyle().Foreground(activeTheme.Dim)
	body := streamBody(lines, streamInner(width))
	foot := keyChip("esc") + dim.Render(" close  ") + keyChip("q") + dim.Render(" quit")
	return renderBox("stream", subtitle, " esc close ", strings.Join(append(body, "", foot), "\n"))
}

// streamBody turns transcript lines into styled body rows. Every row it returns
// is exactly inner cells wide — renderBox sizes the frame to its widest content
// line, so an unpadded or over-long row would move the frame as the window
// scrolls over it, silently, and take the footer's gap arithmetic with it.
func streamBody(lines []string, inner int) []string {
	t := activeTheme
	gutter := lipgloss.NewStyle().Foreground(t.Dimmer)
	prose := lipgloss.NewStyle().Foreground(t.Text)
	dim := lipgloss.NewStyle().Foreground(t.Dim)

	var body []string
	if len(lines) == 0 {
		for _, row := range strings.Split(ansi.Wrap(streamEmptyNotice, inner, ""), "\n") {
			body = append(body, padStyled(gutter.Render(row), inner))
		}
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
		// "merge-verification │ " is 21 cells against an inner floor of 20, and
		// padStyled only pads — it cannot shrink an over-wide row. Cut the plain
		// lead before it is styled, leaving room for the tool-call prefix and at
		// least one cell of content.
		lead = truncate(lead, max(1, inner-lipgloss.Width(streamToolPrefix)-1))
		if rest, ok := strings.CutPrefix(text, streamToolPrefix); ok {
			// Tool calls are not wrapped, so the plain text is truncated before
			// it is split and styled: cutting an already-styled string can slice
			// an escape sequence and bleed colour into the rest of the row.
			rest = truncate(rest, max(1, inner-lipgloss.Width(lead)-lipgloss.Width(streamToolPrefix)))
			name, args, _ := strings.Cut(rest, "(")
			tool := lipgloss.NewStyle().Foreground(t.Structure).Render(name)
			if args != "" {
				tool += prose.Render("(" + args)
			}
			body = append(body, padStyled(gutter.Render(lead)+dim.Render(streamToolPrefix)+tool, inner))
			continue
		}
		wrapWidth := max(1, inner-lipgloss.Width(lead))
		pad := strings.Repeat(" ", lipgloss.Width(lead))
		for i, wrapped := range strings.Split(ansi.Wrap(text, wrapWidth, ""), "\n") {
			marker := pad
			if i == 0 {
				marker = lead
			}
			body = append(body, padStyled(gutter.Render(marker)+prose.Render(wrapped), inner))
		}
	}
	return body
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
