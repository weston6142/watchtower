package tui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

type pagerState struct {
	Mode  string
	Files []string
	Sel   int
	Title string
	Lines []string
	Top   int
}

func (p pagerState) scroll(key string, height int) pagerState {
	if height < 1 {
		height = 1
	}
	maxTop := max(0, len(p.Lines)-height)
	switch key {
	case "j":
		p.Top++
	case "k":
		p.Top--
	case "d":
		p.Top += height / 2
	case "u":
		p.Top -= height / 2
	case "g":
		p.Top = 0
	case "G":
		p.Top = maxTop
	}
	p.Top = min(max(p.Top, 0), maxTop)
	return p
}

func renderArtifactList(p pagerState, id Identity, width, height int) string {
	t := activeTheme
	dim := lipgloss.NewStyle().Foreground(t.Dim)
	inner := max(20, width-8)
	var body []string
	if len(p.Files) == 0 {
		body = append(body, themeDim.Render("no artifacts"))
	} else {
		for i, file := range p.Files {
			body = append(body, cursorRow(i == p.Sel, truncate(file, max(1, inner-2)), inner))
		}
	}
	if height > 4 && len(body) > height-4 {
		body = body[:height-4]
	}
	foot := keyChip("enter") + dim.Render(" open  ") + keyChip("esc") + dim.Render(" back to tower")
	sub := strings.TrimSpace(id.Tag + " " + p.Title)
	return renderBox("artifacts", sub, " esc back ", strings.Join(append(body, "", foot), "\n"))
}

// renderPager is a reading mode: diff semantics in Ok/Err/Structure, body in
// Dim — the position indicator is the ONLY accent on the surface, so nothing
// claims your action while you read.
func renderPager(p pagerState, width, height int) string {
	t := activeTheme
	windowHeight := max(1, height-2)
	start := min(max(p.Top, 0), max(0, len(p.Lines)-windowHeight))
	end := min(start+windowHeight, len(p.Lines))
	title := lipgloss.NewStyle().Foreground(t.Bright).Bold(true).Render(p.Title)
	position := lipgloss.NewStyle().Foreground(t.Accent).Render(fmt.Sprintf("%d/%d", start, len(p.Lines)))
	hint := lipgloss.NewStyle().Foreground(t.Dim).Render(" · esc back")
	lines := []string{title + "  " + position + hint, ""}
	for _, line := range p.Lines[start:end] {
		lines = append(lines, pagerLine(line))
	}
	return boundedLines(lines, width)
}

// pagerLine classifies one pager line for diff-aware coloring.
func pagerLine(line string) string {
	t := activeTheme
	switch {
	case strings.HasPrefix(line, "+"):
		return lipgloss.NewStyle().Foreground(t.Ok).Render(line)
	case strings.HasPrefix(line, "-"):
		return lipgloss.NewStyle().Foreground(t.Err).Render(line)
	case strings.HasPrefix(line, "@@"):
		return lipgloss.NewStyle().Foreground(t.Structure).Render(line)
	case strings.HasPrefix(line, "diff "):
		return lipgloss.NewStyle().Foreground(t.Bright).Bold(true).Render(line)
	default:
		return lipgloss.NewStyle().Foreground(t.Dim).Render(line)
	}
}

func readArtifact(p pagerState) (pagerState, error) {
	if p.Sel < 0 || p.Sel >= len(p.Files) {
		return p, fmt.Errorf("artifact selection out of range")
	}
	b, err := os.ReadFile(p.Files[p.Sel])
	if err != nil {
		return p, err
	}
	p.Mode = "pager"
	p.Title = p.Files[p.Sel]
	p.Lines = strings.Split(string(b), "\n")
	p.Top = 0
	return p, nil
}

func pagerStyle(id Identity) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(id.Color))
}
