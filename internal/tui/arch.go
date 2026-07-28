package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/weston6142/watchtower/internal/archmap"
	"github.com/weston6142/watchtower/internal/projection"
	"github.com/weston6142/watchtower/internal/touchset"
)

func moduleMatches(module string, glob string) bool {
	prefix := touchset.PrefixOf(glob)
	if prefix == "" {
		return true
	}
	return module == prefix || strings.HasPrefix(module, prefix+"/") || strings.HasPrefix(prefix, module+"/")
}

func renderArch(am *archmap.Map, ids map[string]Identity, width, height int) string {
	return renderArchWithState(am, nil, ids, width, height, 0, "")
}

const archNameWidth = 28

func renderArchWithState(am *archmap.Map, st *projection.State, ids map[string]Identity, width, height, selected int, filter string) string {
	t := activeTheme
	if am == nil || len(am.Modules) == 0 {
		return boundedLines([]string{panelTitle("Architecture map", ""), "", lipgloss.NewStyle().Foreground(t.Dim).Render("no repo — arch map available with --repo")}, width)
	}
	insight := ""
	if st != nil {
		insight = mapInsight(st.Issues)
	}
	lines := []string{panelTitle("Architecture map", insight), ""}
	builders := map[string]map[string]bool{}
	if st != nil {
		builders = buildersByArea(st.Issues)
	}
	active := 0
	quietNames := []string{}
	structure := lipgloss.NewStyle().Foreground(t.Structure)
	for _, module := range am.Modules {
		marks := areaMarks(module.Name, builders, am.Overlays, ids, filter)
		if len(marks) == 0 {
			quietNames = append(quietNames, module.Name)
			continue
		}
		nameStyle := lipgloss.NewStyle().Foreground(t.Text)
		if active == selected {
			nameStyle = nameStyle.Foreground(t.Bright)
		}
		row := structure.Render("▣") + " " + nameStyle.Render(padCell(module.Name, archNameWidth)) + " " + strings.Join(marks, " ")
		lines = append(lines, cursorRow(active == selected, row, width))
		active++
	}
	if n := len(quietNames); n > 0 {
		// Quiet areas collapse into one dim row with a short name sample.
		sample := strings.Join(quietNames[:min(3, n)], " · ")
		if n > 3 {
			sample += " · …"
		}
		dim := lipgloss.NewStyle().Foreground(t.Dim)
		lines = append(lines, "  "+lipgloss.NewStyle().Foreground(t.Dimmer).Render("▢")+" "+dim.Render(fmt.Sprintf("%d quiet area%s", n, pluralSuffix(n))+"   "+sample))
	}
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	return boundedLines(lines, width)
}

// archFooterDetail names the selected active module for the keybar's right
// slot; empty when the map has no active modules.
func archFooterDetail(am *archmap.Map, st *projection.State, ids map[string]Identity, selected int, filter string) string {
	if am == nil || st == nil {
		return ""
	}
	builders := buildersByArea(st.Issues)
	active := 0
	for _, module := range am.Modules {
		marks := areaMarks(module.Name, builders, am.Overlays, ids, filter)
		if len(marks) == 0 {
			continue
		}
		if active == selected {
			return lipgloss.NewStyle().Foreground(activeTheme.Dim).
				Render(fmt.Sprintf("%s · %d active", module.Name, len(marks)))
		}
		active++
	}
	return ""
}

func areaMarks(area string, builders map[string]map[string]bool, overlays []archmap.Overlay, ids map[string]Identity, filter string) []string {
	byIssue := builders[area]
	if len(byIssue) == 0 {
		for _, overlay := range overlays {
			if filter != "" && overlay.IssueID != filter {
				continue
			}
			for _, glob := range overlay.Globs {
				if moduleMatches(area, glob) {
					byIssue = map[string]bool{overlay.IssueID: true}
					break
				}
			}
		}
	}
	issueIDs := make([]string, 0, len(byIssue))
	for issueID := range byIssue {
		if filter == "" || filter == issueID {
			issueIDs = append(issueIDs, issueID)
		}
	}
	sort.Strings(issueIDs)
	marks := make([]string, 0, len(issueIDs))
	for _, issueID := range issueIDs {
		mark := "◌"
		if byIssue[issueID] {
			mark = "◈"
		}
		identity := ids[issueID]
		if identity.Color != "" {
			marks = append(marks, identityStyle(identity).Render(mark+identity.Tag))
		} else {
			marks = append(marks, mark+issueID)
		}
	}
	return marks
}

func buildersByArea(issues map[string]*projection.IssueView) map[string]map[string]bool {
	builders := map[string]map[string]bool{}
	for issueID, issue := range issues {
		if issue == nil {
			continue
		}
		total := 0
		for _, weight := range issue.AreaWeights {
			if weight > 0 {
				total += weight
			}
		}
		if total == 0 {
			continue
		}
		for area, weight := range issue.AreaWeights {
			if weight <= 0 {
				continue
			}
			if builders[area] == nil {
				builders[area] = map[string]bool{}
			}
			// An issue "builds" an area when the area holds at least a
			// quarter of its total change weight; lighter touches are
			// rendered as brushers.
			builders[area][issueID] = weight*4 >= total
		}
	}
	return builders
}

func mapInsight(issues map[string]*projection.IssueView) string {
	builders := buildersByArea(issues)
	areas := make([]string, 0, len(builders))
	for area, byIssue := range builders {
		count := 0
		for _, isBuilder := range byIssue {
			if isBuilder {
				count++
			}
		}
		if count >= 2 {
			areas = append(areas, area)
		}
	}
	if len(areas) > 0 {
		sort.Strings(areas)
		area := areas[0]
		count := 0
		for _, isBuilder := range builders[area] {
			if isBuilder {
				count++
			}
		}
		return fmt.Sprintf("%s/ has %d builders (sequenced)", area, count)
	}
	issueIDs := make([]string, 0, len(issues))
	for issueID := range issues {
		issueIDs = append(issueIDs, issueID)
	}
	sort.Strings(issueIDs)
	bestID := ""
	bestAreas := 0
	for _, issueID := range issueIDs {
		issue := issues[issueID]
		if issue == nil || len(issue.AreaWeights) <= bestAreas {
			continue
		}
		bestID, bestAreas = issueID, len(issue.AreaWeights)
	}
	if bestAreas > 0 {
		return fmt.Sprintf("%s spans %d areas", bestID, bestAreas)
	}
	return "quiet"
}
