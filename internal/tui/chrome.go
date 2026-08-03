package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// State glyphs — the ONLY state vocabulary any surface may use.
const (
	glyphDone    = "●"
	glyphNeedYou = "◔"
	glyphWorking = "◐"
	glyphWaiting = "○"
	glyphFailed  = "✕"
	glyphCursor  = "▸"
	glyphShipped = "⇡"
	glyphParked  = "⏸"
)

// padH is the horizontal padding, in cells, inside chrome bars.
const padH = 2

type badgeKind int

const (
	badgeWarn badgeKind = iota
	badgeErr
	badgeOk
)

type badge struct {
	Text string
	Kind badgeKind
}

func badgeColor(k badgeKind) lipgloss.Color {
	switch k {
	case badgeErr:
		return activeTheme.Err
	case badgeOk:
		return activeTheme.Ok
	default:
		return activeTheme.Warn
	}
}

func renderBadge(b badge) string {
	return lipgloss.NewStyle().Foreground(badgeColor(b.Kind)).Bold(true).Render("● " + b.Text)
}

// renderChromeHeader is the full-width Bg1 bar: badges left, dim detail right.
func renderChromeHeader(width int, badges []badge, right string) string {
	t := activeTheme
	parts := make([]string, 0, len(badges))
	for _, b := range badges {
		parts = append(parts, renderBadge(b))
	}
	left := strings.Join(parts, "  ")
	return chromeBar(width, left, lipgloss.NewStyle().Foreground(t.Dim).Render(right))
}

// renderKeybar is the full-width Bg1 footer: key chips left, a pre-styled
// slot right (errText for errors, or any dim detail text).
func renderKeybar(width int, bindings [][2]string, right string) string {
	t := activeTheme
	label := lipgloss.NewStyle().Foreground(t.Dim)
	items := make([]string, 0, len(bindings))
	for _, b := range bindings {
		items = append(items, keyChip(b[0])+label.Render(" "+b[1]))
	}

	available := max(1, width-2*padH)
	rows := make([]string, 0, len(items)+1)
	current := ""
	for _, item := range items {
		candidate := item
		if current != "" {
			candidate = current + "  " + item
		}
		if current != "" && lipgloss.Width(candidate) > available {
			rows = append(rows, current)
			current = item
			continue
		}
		current = candidate
	}
	if current != "" {
		rows = append(rows, current)
	}
	if right != "" {
		last := len(rows) - 1
		if last >= 0 && lipgloss.Width(rows[last])+1+lipgloss.Width(right) <= available {
			rows[last] += " " + right
		} else {
			rows = append(rows, right)
		}
	}
	return chromeRows(width, rows)
}

// errText styles a transient error for the keybar's right slot.
//
// The newlines are collapsed for the same reason renderNoticeRow collapses
// them: this is a reserved single row that every screen's height budget counts
// on, and m.Err carries err.Error() straight from the daemon, where an error
// wrapping a command's CombinedOutput is routinely multi-line. chromeBar caps
// the width but cannot cap the height, so an uncollapsed error adds a screen
// row per newline and bubbletea silently drops the header off the top.
func errText(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " ")
	return lipgloss.NewStyle().Foreground(activeTheme.Err).Render(glyphFailed + " " + s)
}

// chromeBar lays left and right on one Bg1 row spanning width.
func chromeBar(width int, left, right string) string {
	width = max(1, width)
	available := max(1, width-2*padH)
	left = truncate(left, available)
	right = truncate(right, max(1, available-lipgloss.Width(left)-1))
	gap := max(1, available-lipgloss.Width(left)-lipgloss.Width(right))
	return chromeRows(width, []string{left + strings.Repeat(" ", gap) + right})
}

func chromeRows(width int, rows []string) string {
	width = max(1, width)
	available := max(1, width-2*padH)
	leftPad := min(padH, width/2)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		row = truncate(row, available)
		rowWidth := lipgloss.Width(row)
		if rowWidth > width-leftPad {
			row = truncate(row, max(1, width-leftPad))
			rowWidth = lipgloss.Width(row)
		}
		out = append(out, lipgloss.NewStyle().Background(activeTheme.Bg1).Render(
			strings.Repeat(" ", leftPad)+row+strings.Repeat(" ", max(0, width-leftPad-rowWidth)),
		))
	}
	return strings.Join(out, "\n")
}

func keyChip(key string) string {
	t := activeTheme
	return lipgloss.NewStyle().Foreground(t.Bright).Background(t.Bg3).Padding(0, 1).Render(key)
}

// panelTitle renders the shared door heading: bright bold title plus an
// optional dim subtitle.
func panelTitle(title, subtitle string) string {
	t := activeTheme
	out := lipgloss.NewStyle().Foreground(t.Bright).Bold(true).Render(title)
	if subtitle != "" {
		out += lipgloss.NewStyle().Foreground(t.Dim).Render("  " + subtitle)
	}
	return out
}

// emptyDoorRow is the dim placeholder shown when a door has no content.
func emptyDoorRow() string {
	return lipgloss.NewStyle().Foreground(activeTheme.Dimmer).Render("  —")
}

// cursorRow renders a selectable list row: accent ▸ + Bg2 ground when
// selected, a 2-cell gutter otherwise. Every list uses this.
func cursorRow(selected bool, content string, width int) string {
	t := activeTheme
	if !selected {
		return "  " + content
	}
	row := lipgloss.NewStyle().Foreground(t.Accent).Render(glyphCursor) + " " + content
	return lipgloss.NewStyle().Background(t.Bg2).MaxWidth(max(1, width)).Render(row)
}
