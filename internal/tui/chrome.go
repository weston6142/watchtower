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

// Spacing — renderers use these, never literal spacing.
const (
	padV   = 1 // blank lines inside a panel
	padH   = 2 // cells of horizontal panel padding
	gutter = 2 // cells between adjacent panels
)

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
	parts := make([]string, 0, len(bindings))
	for _, b := range bindings {
		parts = append(parts, keyChip(b[0])+label.Render(" "+b[1]))
	}
	return chromeBar(width, strings.Join(parts, "  "), right)
}

// errText styles a transient error for the keybar's right slot.
func errText(s string) string {
	if s == "" {
		return ""
	}
	return lipgloss.NewStyle().Foreground(activeTheme.Err).Render(glyphFailed + " " + s)
}

// chromeBar lays left and right on one Bg1 row spanning width.
func chromeBar(width int, left, right string) string {
	t := activeTheme
	pad := width - lipgloss.Width(left) - lipgloss.Width(right) - 2*padH
	if pad < 1 {
		pad = 1
	}
	row := strings.Repeat(" ", padH) + left + strings.Repeat(" ", pad) + right + strings.Repeat(" ", padH)
	return lipgloss.NewStyle().Background(t.Bg1).MaxWidth(max(1, width)).Render(row)
}

func keyChip(key string) string {
	t := activeTheme
	return lipgloss.NewStyle().Foreground(t.Bright).Background(t.Bg3).Padding(0, 1).Render(key)
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
