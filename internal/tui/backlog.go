package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/priority"
	"github.com/weston6142/watchtower/internal/projection"
)

// The backlog is the one overlay sized to the viewport instead of to its
// content. Every other overlay is a prompt — a few fields you answer and
// dismiss — so hugging the content is right for them. The backlog is a browsing
// surface, so it takes the screenspace and spends it on two panes: the draft
// list and the selected draft's detail, which was otherwise invisible until you
// pressed enter to edit it.
//
// The frame stays strictly inside the terminal. overlayCenter falls back to
// lipgloss.Place once a box reaches the full width or height, and that drops the
// dimmed base along with the overlay look, so these margins are load-bearing.
const (
	// backlogChromeCols/Rows are the border, padding, and outer margin the frame
	// gives back to the dimmed base.
	backlogChromeCols = 8
	backlogChromeRows = 8
	// backlogMinInner is the old fixed width: id, priority, and a readable title.
	backlogMinInner = 44
	// backlogIDMin/Max bound the id column. Real ids run from GH-3 to
	// gh-importer, so the column is derived per entry set between these.
	backlogIDMin    = 8
	backlogIDMax    = 14
	backlogPrioCols = 7
	// backlogMaxInner caps the line length — past this, one row is a long walk
	// for the eye on an ultrawide terminal.
	backlogMaxInner = 132
	backlogMinRows  = 3
	// backlogFooterRows is the blank line plus the key/position row under the panes.
	backlogFooterRows = 2
	// backlogDividerCols is the " │ " between the two panes.
	backlogDividerCols = 3
	// backlogDetailInner is the narrowest useful detail pane, and
	// backlogSplitInner is where that pane earns its space: the list still needs
	// its minimum, and the divider costs its columns on top of both.
	backlogDetailInner = 40
	backlogSplitInner  = backlogMinInner + backlogDetailInner + backlogDividerCols
	// backlogFallbackRows stands in until the first WindowSizeMsg lands.
	backlogFallbackRows = 32
)

// renderBacklog lists drafts in the shared box chrome — the backlog is where
// issues wait, so it wears the issue modal's clothes — but sized to width x
// height rather than to the rows it happens to hold.
func renderBacklog(entries []*projection.IssueView, sel, width, height int) string {
	keys := backlogKeys()
	// The key hints are the one line that is the same at every size, and cutting
	// them costs the operator the way out of the overlay — so they, not the list,
	// set the frame's floor.
	inner := min(max(width-backlogChromeCols, backlogMinInner, lipgloss.Width(keys)), backlogMaxInner)
	if len(entries) == 0 {
		// An empty backlog has nothing to size to: one sentence ruled off inside a
		// 130-column frame reads worse than the small box ever did, so the empty
		// state keeps exactly its old shape.
		return renderBox("backlog", "drafts waiting to launch", " esc close ",
			lipgloss.NewStyle().Foreground(activeTheme.Dim).
				Render("backlog is empty — n then ctrl+s files a draft")+"\n\n"+keys)
	}
	if height <= 0 {
		height = backlogFallbackRows
	}
	// The call site clamps sel too, but an out-of-range sel here would page the
	// window past the end and have the footer report 37–36 of 36.
	sel = min(max(sel, 0), len(entries)-1)
	paneBudget := max(height-backlogChromeRows-backlogFooterRows, backlogMinRows)

	listWidth, detailWidth := inner, 0
	if inner >= backlogSplitInner {
		detailWidth = max(backlogDetailInner, inner/3)
		listWidth = max(backlogMinInner, inner-detailWidth-backlogDividerCols)
	}

	var detail []string
	if detailWidth > 0 && sel >= 0 && sel < len(entries) {
		detail = backlogDetail(entries[sel], detailWidth)
	}
	// Height is content-driven but capped: filling a tall terminal with blank
	// rows for two drafts would be worse than the box being small.
	paneRows := max(backlogMinRows, min(paneBudget, max(len(entries), len(detail))))
	detail = backlogClipDetail(detail, paneRows, detailWidth)

	start := backlogWindowStart(sel, len(entries), paneRows)
	list := backlogRows(entries, sel, start, min(start+paneRows, len(entries)), listWidth)
	pane := backlogPane(list, detail, paneRows, listWidth, detailWidth)
	footer := boundedLines([]string{backlogFooter(len(entries), start, paneRows, inner)}, inner)
	return renderBox("backlog", "drafts waiting to launch", " esc close ", pane+"\n\n"+footer)
}

// backlogWindowStart pages the window rather than centering it on the cursor.
// renderBacklog holds no scroll position, so it cannot scroll minimally; of the
// stateless options, a page that flips only at its boundary is far calmer under
// j/k than a window that re-centers on every keypress.
func backlogWindowStart(sel, count, rows int) int {
	if count <= rows || rows <= 0 {
		return 0
	}
	return max(sel, 0) / rows * rows
}

// backlogClipDetail trims the detail pane to the rows it has, marking the cut.
// The list says "1–10 of 40" when it clips, so a long body must not just stop
// mid-sentence and look like the whole of it.
func backlogClipDetail(detail []string, rows, width int) []string {
	if len(detail) <= rows || rows <= 0 {
		return detail
	}
	clipped := append([]string(nil), detail[:rows-1]...)
	return append(clipped, lipgloss.NewStyle().Foreground(activeTheme.Dim).
		Render(padCell("… enter to read it all", width)))
}

// backlogRows renders entries[start:end] as cursor rows padded to width, so the
// selected row's band spans the whole column.
func backlogRows(entries []*projection.IssueView, sel, start, end, width int) []string {
	t := activeTheme
	// cursorRow prepends a two-column cursor or gutter, so the cells share what
	// is left of the column.
	cells := max(1, width-2)
	idWidth := backlogIDWidth(entries)
	titleWidth := max(1, cells-idWidth-backlogPrioCols)
	var rows []string
	for i := start; i < end; i++ {
		iv := entries[i]
		// Pad every cell before styling: padCell truncates, and truncating an
		// already-styled string can cut mid-escape and bleed colour into the
		// next cell. Width 7 fits urgent/normal plus a space.
		id := lipgloss.NewStyle().Foreground(t.Dim).Render(padCell(iv.ID, idWidth))
		prio := lipgloss.NewStyle().Foreground(t.Structure).Render(padCell(priority.Label(iv.Priority), backlogPrioCols))
		title := lipgloss.NewStyle().Foreground(t.Text).Render(padCell(iv.Title, titleWidth))
		rows = append(rows, cursorRow(i == sel, id+prio+title, width))
	}
	return rows
}

// backlogIDWidth sizes the id column to the widest id in the whole set rather
// than in the visible window: deriving it from the window would shift the
// column sideways every time j/k crossed a page boundary. Ids as long as
// gh-importer are real, and the old fixed 7 showed them as gh-imp….
func backlogIDWidth(entries []*projection.IssueView) int {
	widest := 0
	for _, iv := range entries {
		widest = max(widest, lipgloss.Width(iv.ID))
	}
	return min(max(widest+1, backlogIDMin), backlogIDMax)
}

// backlogDetail describes one draft in the right-hand pane. Every line is padded
// to width: renderBox sizes to its widest content line, so a short body would
// otherwise shrink the whole frame back to its content.
func backlogDetail(iv *projection.IssueView, width int) []string {
	t := activeTheme
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	cell := func(s string, style lipgloss.Style) string { return style.Render(padCell(s, width)) }
	wrapped := func(s string, style lipgloss.Style) []string {
		var out []string
		for _, line := range strings.Split(ansi.Wrap(s, width, ""), "\n") {
			out = append(out, cell(line, style))
		}
		return out
	}

	lines := wrapped(iv.Title, lipgloss.NewStyle().Foreground(t.Bright).Bold(true))
	lines = append(lines, cell("", dim))
	labelWidth := min(9, max(1, width-1))
	for _, kv := range [][2]string{
		{"id", iv.ID},
		{"priority", priority.Label(iv.Priority)},
		{"flow", orElse(iv.Flow, "default")},
		{"preset", orElse(iv.Preset, string(flow.LeverRegular))},
	} {
		label := dim.Render(padCell(strings.ToUpper(kv[0]), labelWidth))
		value := lipgloss.NewStyle().Foreground(t.Structure).
			Render(padCell(kv[1], max(1, width-labelWidth)))
		lines = append(lines, label+value)
	}
	lines = append(lines, cell("", dim), cell("BODY", dim))
	if strings.TrimSpace(iv.Body) == "" {
		return append(lines, cell("no body yet", lipgloss.NewStyle().Foreground(t.Dimmer)))
	}
	return append(lines, wrapped(iv.Body, lipgloss.NewStyle().Foreground(t.Text))...)
}

// backlogPane lays the list and detail columns side by side for rows rows,
// squaring both off so every line is exactly the frame's inner width.
func backlogPane(list, detail []string, rows, listWidth, detailWidth int) string {
	divider := lipgloss.NewStyle().Foreground(activeTheme.Dimmer).Render("│")
	out := make([]string, rows)
	for i := range out {
		left := padStyled(lineAt(list, i), listWidth)
		if detailWidth == 0 {
			out[i] = left
			continue
		}
		out[i] = left + " " + divider + " " + padStyled(lineAt(detail, i), detailWidth)
	}
	return strings.Join(out, "\n")
}

// backlogFooter puts the keys on the left and, once there are drafts, how many
// of them you are looking at on the right.
func backlogFooter(count, start, rows, width int) string {
	keys := backlogKeys()
	position := fmt.Sprintf("%d drafts", count)
	switch {
	case count == 1:
		position = "1 draft"
	case count > rows:
		// The window is clipping: say which slice of the queue is on screen so
		// drafts off-window are not mistaken for drafts that do not exist.
		position = fmt.Sprintf("%d–%d of %d", start+1, min(start+rows, count), count)
	}
	styled := lipgloss.NewStyle().Foreground(activeTheme.Structure).Render(position)
	gap := width - lipgloss.Width(keys) - lipgloss.Width(styled)
	if gap < 1 {
		// Both do not fit. The keys are how the operator acts on the list, so the
		// count is what yields: appending it anyway would push the hints past the
		// frame, and the frame truncates from the right.
		return keys
	}
	return keys + strings.Repeat(" ", gap) + styled
}

// backlogKeys is the hint line under the panes. renderBacklog measures it to
// floor the frame — cutting the keys costs the operator the way out of the
// overlay — so it lives on its own rather than inline in the footer.
func backlogKeys() string {
	dim := lipgloss.NewStyle().Foreground(activeTheme.Dim)
	return keyChip("enter") + dim.Render(" edit  ") + keyChip("l") + dim.Render(" launch  ") +
		keyChip("X") + dim.Render(" delete  ") + keyChip("j/k") + dim.Render(" move")
}

// padStyled right-pads an already-styled line to width. The padding is plain, so
// it neither extends a cursor band nor cuts into an escape sequence.
func padStyled(s string, width int) string {
	if pad := width - lipgloss.Width(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

// lineAt returns lines[i], or "" past the end: the two panes rarely hold the
// same number of rows, so the shorter one has to keep answering.
func lineAt(lines []string, i int) string {
	if i < 0 || i >= len(lines) {
		return ""
	}
	return lines[i]
}

func orElse(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
