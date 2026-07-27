package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wbushyeager/guildhall/internal/archmap"
	"github.com/wbushyeager/guildhall/internal/projection"
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
	return renderArchWithState(am, nil, ids, width, height, 0, "")
}

func renderArchWithState(am *archmap.Map, st *projection.State, ids map[string]Identity, width, height, selected int, filter string) string {
	if am == nil || len(am.Modules) == 0 {
		return boundedLines([]string{"ARCHITECTURE MAP", "no repo — arch map available with --repo"}, width)
	}
	lines := []string{"ARCHITECTURE MAP · a/esc back"}
	if st != nil {
		lines = append(lines, "MAP · "+mapInsight(st.Issues))
	}
	builders := map[string]map[string]bool{}
	if st != nil {
		builders = buildersByArea(st.Issues)
	}
	active := 0
	firstActive := ""
	quiet := 0
	for _, module := range am.Modules {
		marks := areaMarks(module.Name, builders, am.Overlays, ids, filter)
		if len(marks) == 0 {
			quiet++
			continue
		}
		if firstActive == "" {
			firstActive = module.Name
		}
		marker := "  "
		if active == selected {
			marker = "▸ "
		}
		lines = append(lines, marker+"▣ "+module.Name+" "+strings.Join(marks, " "))
		active++
	}
	if quiet > 0 {
		lines = append(lines, fmt.Sprintf("▸ %d quiet areas", quiet))
	}
	if firstActive != "" {
		lines = append(lines, themeDim.Render("selection: "+firstActive+" · active areas show builders and brushers"))
	}
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	return boundedLines(lines, width)
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
