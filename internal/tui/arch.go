package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/wbushyeager/guildhall/internal/archmap"
	"github.com/wbushyeager/guildhall/internal/touchset"
)

func moduleMatches(module string, glob string) bool {
	prefix := touchset.PrefixOf(glob)
	if prefix == "" {
		return true
	}
	return module == prefix || strings.HasPrefix(module, prefix+"/") || strings.HasPrefix(prefix, module+"/")
}

func renderArch(am *archmap.Map, ids map[string]Identity, width, height int) string {
	if am == nil || len(am.Modules) == 0 {
		return boundedLines([]string{"ARCHITECTURE MAP", "no repo — arch map available with --repo"}, width)
	}
	lines := []string{"ARCHITECTURE MAP · a/esc back"}
	for _, module := range am.Modules {
		ghosts := make([]string, 0)
		for _, overlay := range am.Overlays {
			matched := false
			for _, glob := range overlay.Globs {
				if moduleMatches(module.Name, glob) {
					matched = true
					break
				}
			}
			if matched {
				tag := overlay.IssueID
				identity, ok := ids[overlay.IssueID]
				if ok {
					tag = identity.Tag
					ghosts = append(ghosts, lipgloss.NewStyle().Foreground(lipgloss.Color(identity.Color)).Render("◈"+tag))
				} else {
					ghosts = append(ghosts, "◈"+tag)
				}
			}
		}
		line := fmt.Sprintf("▣ %s (%d)", module.Name, module.Files)
		if len(ghosts) > 0 {
			line += " " + strings.Join(ghosts, " ")
		}
		lines = append(lines, line)
	}
	if len(am.Overlays) > 0 {
		lines = append(lines, "", "GHOSTS")
		for _, overlay := range am.Overlays {
			identity := ids[overlay.IssueID]
			label := overlay.IssueID
			if identity.Tag != "" {
				label = identity.Tag
			}
			style := lipgloss.NewStyle().Foreground(lipgloss.Color(identity.Color))
			lines = append(lines, style.Render("◈ "+label+" "+strings.Join(overlay.Globs, ", ")))
		}
	}
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	return boundedLines(lines, width)
}
