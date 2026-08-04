package tui

import (
	"reflect"
	"testing"
)

func TestMainKeybindingRegistryMetadata(t *testing.T) {
	expectedIDs := []string{
		"navigation-floors",
		"navigation-cards",
		"navigation-focus-issue",
		"navigation-attention",
		"navigation-war-room",
		"navigation-open-artifacts",
		"navigation-setup-inspector",
		"navigation-back",
		"navigation-rows-layout",
		"control-pause-resume",
		"control-kill-stage",
		"control-abandon-lane",
		"control-retry",
		"control-levers",
		"control-new-issue",
		"control-investigation",
		"control-save-draft",
		"control-backlog",
		"control-launch-draft",
		"control-retire-shipped-lane",
		"control-shipped-shelf",
		"doors-decisions",
		"doors-triage",
		"doors-timeline",
		"doors-stream",
		"doors-reject-tray",
		"doors-architecture",
		"decisions-accept",
		"decisions-choose",
		"decisions-evidence",
		"decisions-choose-by-number",
		"chrome-help",
		"chrome-quit",
	}

	if len(mainKeybindingRegistry) != len(expectedIDs) {
		t.Fatalf("registry has %d entries, want %d", len(mainKeybindingRegistry), len(expectedIDs))
	}
	wantIDs := make(map[string]bool, len(expectedIDs))
	for _, id := range expectedIDs {
		wantIDs[id] = true
	}
	seenIDs := make(map[string]bool, len(mainKeybindingRegistry))
	seenFooterOrders := map[int]string{}
	var investigation mainKeybinding
	for _, binding := range mainKeybindingRegistry {
		if binding.ID == "" || binding.Key == "" || binding.Description == "" {
			t.Fatalf("registry entry has empty required metadata: %+v", binding)
		}
		if !wantIDs[binding.ID] {
			t.Fatalf("unexpected registry ID %q", binding.ID)
		}
		if seenIDs[binding.ID] {
			t.Fatalf("duplicate registry ID %q", binding.ID)
		}
		seenIDs[binding.ID] = true
		switch binding.Group {
		case mainKeybindingGroupNavigation, mainKeybindingGroupControl,
			mainKeybindingGroupDoors, mainKeybindingGroupDecisions:
		default:
			t.Fatalf("unrecognized help group %q for %q", binding.Group, binding.ID)
		}
		switch binding.Column {
		case mainKeybindingColumnLeft, mainKeybindingColumnRight:
		default:
			t.Fatalf("unrecognized help column %q for %q", binding.Column, binding.ID)
		}
		switch binding.Placement {
		case mainKeybindingPlacementGroup, mainKeybindingPlacementChrome:
		default:
			t.Fatalf("unrecognized help placement %q for %q", binding.Placement, binding.ID)
		}
		if binding.FooterVisible {
			if binding.FooterOrder <= 0 {
				t.Fatalf("footer-visible entry %q has no positive order", binding.ID)
			}
			if previous, ok := seenFooterOrders[binding.FooterOrder]; ok {
				t.Fatalf("footer order %d used by both %q and %q", binding.FooterOrder, previous, binding.ID)
			}
			seenFooterOrders[binding.FooterOrder] = binding.ID
		}
		if binding.Key == "i" {
			investigation = binding
		}
	}
	for _, id := range expectedIDs {
		if !seenIDs[id] {
			t.Fatalf("registry is missing approved entry %q", id)
		}
	}
	if investigation.ID != "control-investigation" || investigation.Group != mainKeybindingGroupControl || investigation.Description != "investigation" {
		t.Fatalf("investigation registration = %+v", investigation)
	}
}

func TestMainKeybindingHelpProjectionCoversRegistry(t *testing.T) {
	projection := projectMainKeybindingHelp()
	seen := map[string]int{}
	for _, column := range projection.Columns {
		for _, group := range column {
			for _, row := range group.Rows {
				seen[row.ID]++
			}
		}
	}
	seen[projection.Close.ID]++
	seen[projection.Quit.ID]++
	for _, binding := range mainKeybindingRegistry {
		if seen[binding.ID] != 1 {
			t.Fatalf("registry entry %q appears %d times in help projection", binding.ID, seen[binding.ID])
		}
	}
}

func TestMainKeybindingFooterProjectionPreservesContract(t *testing.T) {
	want := [][2]string{
		{"j/k", "floors"},
		{"tab", "next"},
		{"p", "pause/resume"},
		{"x", "kill"},
		{"R", "retry"},
		{"T", "stream"},
		{"L", "levers"},
		{"?", "help"},
		{"q", "quit"},
	}
	got := projectMainKeybindingFooter()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("footer projection = %#v, want %#v", got, want)
	}
	for _, row := range got {
		if row[0] == "i" || row[1] == "investigation" {
			t.Fatalf("investigation must remain out of compact footer: %#v", got)
		}
	}
}
