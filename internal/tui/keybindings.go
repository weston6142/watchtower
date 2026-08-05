package tui

import "sort"

type mainKeybindingGroup string

const (
	mainKeybindingGroupNavigation mainKeybindingGroup = "NAVIGATION"
	mainKeybindingGroupControl    mainKeybindingGroup = "CONTROL"
	mainKeybindingGroupDoors      mainKeybindingGroup = "DOORS"
	mainKeybindingGroupDecisions  mainKeybindingGroup = "DECISIONS"
)

type mainKeybindingColumn string

const (
	mainKeybindingColumnLeft  mainKeybindingColumn = "left"
	mainKeybindingColumnRight mainKeybindingColumn = "right"
)

type mainKeybindingPlacement string

const (
	mainKeybindingPlacementGroup  mainKeybindingPlacement = "group"
	mainKeybindingPlacementChrome mainKeybindingPlacement = "chrome"
)

type mainKeybinding struct {
	ID                string
	Key               string
	Description       string
	Group             mainKeybindingGroup
	Column            mainKeybindingColumn
	Placement         mainKeybindingPlacement
	FooterVisible     bool
	FooterOrder       int
	FooterKey         string
	FooterDescription string
}

type mainKeybindingHelpRow struct {
	ID          string
	Key         string
	Description string
}

type mainKeybindingHelpGroup struct {
	Name string
	Rows []mainKeybindingHelpRow
}

type mainKeybindingHelpProjection struct {
	Columns [2][]mainKeybindingHelpGroup
	Close   mainKeybindingHelpRow
	Quit    mainKeybindingHelpRow
}

var mainKeybindingRegistry = []mainKeybinding{
	{ID: "navigation-floors", Key: "j / k", Description: "floors", Group: mainKeybindingGroupNavigation, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup, FooterVisible: true, FooterOrder: 1, FooterKey: "j/k"},
	{ID: "navigation-cards", Key: "h / l", Description: "cards", Group: mainKeybindingGroupNavigation, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "navigation-focus-issue", Key: "1..9", Description: "focus issue", Group: mainKeybindingGroupNavigation, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "navigation-attention", Key: "tab", Description: "attention / next field", Group: mainKeybindingGroupNavigation, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup, FooterVisible: true, FooterOrder: 2, FooterDescription: "next"},
	{ID: "navigation-war-room", Key: "g", Description: "war room", Group: mainKeybindingGroupNavigation, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "navigation-open-artifacts", Key: "enter", Description: "open artifacts", Group: mainKeybindingGroupNavigation, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "navigation-open-decision-page", Key: "w", Description: "open briefing", Group: mainKeybindingGroupNavigation, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup, FooterVisible: true, FooterOrder: 10, FooterDescription: "briefing"},
	{ID: "navigation-setup-inspector", Key: "f", Description: "setup inspector", Group: mainKeybindingGroupNavigation, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "navigation-back", Key: "esc", Description: "back", Group: mainKeybindingGroupNavigation, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "navigation-rows-layout", Key: "z", Description: "rows / tower layout", Group: mainKeybindingGroupNavigation, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "control-pause-resume", Key: "p", Description: "pause / resume", Group: mainKeybindingGroupControl, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup, FooterVisible: true, FooterOrder: 3, FooterDescription: "pause/resume"},
	{ID: "control-kill-stage", Key: "x", Description: "kill stage", Group: mainKeybindingGroupControl, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup, FooterVisible: true, FooterOrder: 4, FooterDescription: "kill"},
	{ID: "control-abandon-lane", Key: "X", Description: "abandon lane", Group: mainKeybindingGroupControl, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "control-retry", Key: "R", Description: "retry failed stage", Group: mainKeybindingGroupControl, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup, FooterVisible: true, FooterOrder: 5, FooterDescription: "retry"},
	{ID: "control-levers", Key: "L", Description: "lever editor", Group: mainKeybindingGroupControl, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup, FooterVisible: true, FooterOrder: 7, FooterDescription: "levers"},
	{ID: "control-new-issue", Key: "n", Description: "new issue", Group: mainKeybindingGroupControl, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "control-investigation", Key: "i", Description: "investigation", Group: mainKeybindingGroupControl, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "control-save-draft", Key: "ctrl+s", Description: "save draft", Group: mainKeybindingGroupControl, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "control-backlog", Key: "b", Description: "backlog", Group: mainKeybindingGroupControl, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "control-launch-draft", Key: "l", Description: "launch draft (in backlog)", Group: mainKeybindingGroupControl, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "control-retire-shipped-lane", Key: "c", Description: "retire shipped lane", Group: mainKeybindingGroupControl, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "control-shipped-shelf", Key: "u", Description: "shipped shelf", Group: mainKeybindingGroupControl, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementGroup},
	{ID: "doors-decisions", Key: "d", Description: "decisions", Group: mainKeybindingGroupDoors, Column: mainKeybindingColumnRight, Placement: mainKeybindingPlacementGroup},
	{ID: "doors-triage", Key: "t", Description: "triage", Group: mainKeybindingGroupDoors, Column: mainKeybindingColumnRight, Placement: mainKeybindingPlacementGroup},
	{ID: "doors-timeline", Key: "e", Description: "timeline", Group: mainKeybindingGroupDoors, Column: mainKeybindingColumnRight, Placement: mainKeybindingPlacementGroup},
	{ID: "doors-stream", Key: "T", Description: "stream", Group: mainKeybindingGroupDoors, Column: mainKeybindingColumnRight, Placement: mainKeybindingPlacementGroup, FooterVisible: true, FooterOrder: 6},
	{ID: "doors-reject-tray", Key: "r", Description: "reject tray item", Group: mainKeybindingGroupDoors, Column: mainKeybindingColumnRight, Placement: mainKeybindingPlacementGroup},
	{ID: "doors-architecture", Key: "a / A", Description: "architecture pane / map", Group: mainKeybindingGroupDoors, Column: mainKeybindingColumnRight, Placement: mainKeybindingPlacementGroup},
	{ID: "decisions-accept", Key: "y", Description: "accept recommendation", Group: mainKeybindingGroupDecisions, Column: mainKeybindingColumnRight, Placement: mainKeybindingPlacementGroup},
	{ID: "decisions-choose", Key: "n", Description: "choose option", Group: mainKeybindingGroupDecisions, Column: mainKeybindingColumnRight, Placement: mainKeybindingPlacementGroup},
	{ID: "decisions-evidence", Key: "o", Description: "show evidence", Group: mainKeybindingGroupDecisions, Column: mainKeybindingColumnRight, Placement: mainKeybindingPlacementGroup},
	{ID: "decisions-choose-by-number", Key: "1..9", Description: "choose option by number", Group: mainKeybindingGroupDecisions, Column: mainKeybindingColumnRight, Placement: mainKeybindingPlacementGroup},
	{ID: "chrome-help", Key: "? / esc", Description: "close", Group: mainKeybindingGroupNavigation, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementChrome, FooterVisible: true, FooterOrder: 8, FooterKey: "?", FooterDescription: "help"},
	{ID: "chrome-quit", Key: "q / ctrl+c", Description: "quit", Group: mainKeybindingGroupNavigation, Column: mainKeybindingColumnLeft, Placement: mainKeybindingPlacementChrome, FooterVisible: true, FooterOrder: 9, FooterKey: "q", FooterDescription: "quit"},
}

func projectMainKeybindingHelp() mainKeybindingHelpProjection {
	projection := mainKeybindingHelpProjection{}
	groups := []struct {
		group  mainKeybindingGroup
		column mainKeybindingColumn
	}{
		{group: mainKeybindingGroupNavigation, column: mainKeybindingColumnLeft},
		{group: mainKeybindingGroupControl, column: mainKeybindingColumnLeft},
		{group: mainKeybindingGroupDoors, column: mainKeybindingColumnRight},
		{group: mainKeybindingGroupDecisions, column: mainKeybindingColumnRight},
	}
	for _, spec := range groups {
		group := mainKeybindingHelpGroup{Name: string(spec.group)}
		for _, binding := range mainKeybindingRegistry {
			if binding.Placement != mainKeybindingPlacementGroup || binding.Group != spec.group || binding.Column != spec.column {
				continue
			}
			group.Rows = append(group.Rows, mainKeybindingHelpRow{
				ID:          binding.ID,
				Key:         binding.Key,
				Description: binding.Description,
			})
		}
		column := 0
		if spec.column == mainKeybindingColumnRight {
			column = 1
		}
		projection.Columns[column] = append(projection.Columns[column], group)
	}
	for _, binding := range mainKeybindingRegistry {
		if binding.Placement != mainKeybindingPlacementChrome {
			continue
		}
		row := mainKeybindingHelpRow{ID: binding.ID, Key: binding.Key, Description: binding.Description}
		switch binding.ID {
		case "chrome-help":
			projection.Close = row
		case "chrome-quit":
			projection.Quit = row
		}
	}
	return projection
}

func projectMainKeybindingFooter() [][2]string {
	bindings := make([]mainKeybinding, 0, len(mainKeybindingRegistry))
	for _, binding := range mainKeybindingRegistry {
		if binding.FooterVisible {
			bindings = append(bindings, binding)
		}
	}
	sort.SliceStable(bindings, func(i, j int) bool {
		return bindings[i].FooterOrder < bindings[j].FooterOrder
	})
	rows := make([][2]string, 0, len(bindings))
	for _, binding := range bindings {
		key := binding.FooterKey
		if key == "" {
			key = binding.Key
		}
		description := binding.FooterDescription
		if description == "" {
			description = binding.Description
		}
		rows = append(rows, [2]string{key, description})
	}
	return rows
}
