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
	lines := []string{fmt.Sprintf("ARTIFACTS %s %s · enter open · esc back", id.Tag, p.Title)}
	if len(p.Files) == 0 {
		lines = append(lines, themeDim.Render("no artifacts"))
	} else {
		for i, file := range p.Files {
			mark := "  "
			if i == p.Sel {
				mark = "▶ "
			}
			lines = append(lines, mark+file)
		}
	}
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	return boundedLines(lines, width)
}

func renderPager(p pagerState, width, height int) string {
	windowHeight := max(1, height-2)
	start := min(max(p.Top, 0), max(0, len(p.Lines)-windowHeight))
	end := min(start+windowHeight, len(p.Lines))
	lines := []string{fmt.Sprintf("%s · %d/%d · esc back", p.Title, start, len(p.Lines))}
	lines = append(lines, p.Lines[start:end]...)
	return boundedLines(lines, width)
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
