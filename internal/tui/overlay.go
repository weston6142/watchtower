package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// overlayCenter composites modal over base, centered in width x height. The
// base is flattened to faint (existing colors are stripped) so the modal
// reads as the only saturated element — the herdr overlay look.
func overlayCenter(base, modal string, width, height int) string {
	modalLines := strings.Split(modal, "\n")
	mw := lipgloss.Width(modal)
	mh := len(modalLines)
	if mw >= width || mh >= height {
		return lipgloss.Place(max(1, width), max(1, height), lipgloss.Center, lipgloss.Center, modal)
	}
	baseLines := strings.Split(base, "\n")
	if len(baseLines) > height {
		baseLines = baseLines[:height]
	}
	for len(baseLines) < height {
		baseLines = append(baseLines, "")
	}
	for i := range baseLines {
		baseLines[i] = themeDim.Render(ansi.Strip(baseLines[i]))
	}
	x := (width - mw) / 2
	y := (height - mh) / 2
	for i, ml := range modalLines {
		row := y + i
		bl := baseLines[row]
		left := ansi.Truncate(bl, x, "")
		if pad := x - lipgloss.Width(left); pad > 0 {
			left += strings.Repeat(" ", pad)
		}
		// pad short modal lines so the right slice of the base lines up
		if pad := mw - lipgloss.Width(ml); pad > 0 {
			ml += strings.Repeat(" ", pad)
		}
		baseLines[row] = left + ml + ansi.TruncateLeft(bl, x+mw, "")
	}
	return strings.Join(baseLines, "\n")
}
