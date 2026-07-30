package main

import (
	"strings"
	"testing"

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
