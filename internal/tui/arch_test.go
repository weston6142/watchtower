package tui

import (
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/projection"
)

func TestBuildersAndContention(t *testing.T) {
	issues := map[string]*projection.IssueView{
		"GH-1": {ID: "GH-1", AreaWeights: map[string]int{"payments": 300, "api": 40}},
		"GH-2": {ID: "GH-2", AreaWeights: map[string]int{"api": 200}},
	}
	b := buildersByArea(issues)
	if !b["payments"]["GH-1"] || b["api"]["GH-1"] {
		t.Fatalf("builders wrong: %v", b)
	}
	if !b["api"]["GH-2"] {
		t.Fatalf("GH-2 should build api: %v", b)
	}
	if mapInsight(issues) == "" {
		t.Fatal("no insight")
	}
	issues["GH-1"].AreaWeights["api"] = 200
	if !strings.Contains(mapInsight(issues), "api") {
		t.Fatalf("contention not surfaced: %q", mapInsight(issues))
	}
}
