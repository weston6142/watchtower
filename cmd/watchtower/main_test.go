package main

import (
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/levers"
)

func TestFormatDecisionShowsFreeformRecommendation(t *testing.T) {
	got := formatDecision(engine.PendingDecision{
		ID: 7, IssueID: "GH-1", Stage: "spec",
		D: levers.Decision{
			Kind:                levers.DecisionFreeform,
			Question:            "Review spec.md",
			RecommendedResponse: "Approve spec.md as written.",
		},
	})
	if !strings.Contains(got, "recommended: Approve spec.md as written.") {
		t.Fatalf("formatDecision() = %q", got)
	}
}

func TestFormatDecisionShowsContextHeader(t *testing.T) {
	got := formatDecision(engine.PendingDecision{
		ID: 31, IssueID: "GH-31", Stage: "execute",
		D: levers.Decision{Question: "Proceed?", Options: []string{"yes", "no"}, Recommended: 0},
		Context: &decision.DecisionContext{
			TaskSummary: "Ship decision context.", AgentName: "Executor",
			AgentColor: "green", AgentSymbol: "⚙",
		},
	})
	for _, want := range []string{
		"task: Ship decision context.", "agent: Executor · green · ⚙",
		"[31] GH-31/execute: Proceed?", "* 0) yes", "  1) no",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatDecision() missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "task:") > strings.Index(got, "[31]") ||
		strings.Index(got, "agent:") > strings.Index(got, "[31]") {
		t.Fatalf("context header follows decision:\n%s", got)
	}
}

func TestFormatDecisionLegacyOmitsContextHeader(t *testing.T) {
	got := formatDecision(engine.PendingDecision{
		ID: 32, IssueID: "GH-31", Stage: "execute",
		D: levers.Decision{Question: "Legacy?", Kind: levers.DecisionFreeform},
	})
	if strings.Contains(got, "task:") || strings.Contains(got, "agent:") {
		t.Fatalf("legacy decision gained fabricated context:\n%s", got)
	}
	if !strings.Contains(got, "[32] GH-31/execute: Legacy?") {
		t.Fatalf("legacy decision lost existing rendering:\n%s", got)
	}
}
