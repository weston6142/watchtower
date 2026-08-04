package main

import (
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/decision"
	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/review"
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

func TestFormatDecisionShowsArtifactReviewIdentity(t *testing.T) {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	got := formatDecision(engine.PendingDecision{
		ID: 34, IssueID: "GH-26", Stage: "plan",
		D: levers.Decision{Question: "Approve plan artifacts?", Options: []string{"approve", "revise"}},
		Review: &review.Target{
			IssueID: "GH-26", Stage: "plan", CheckpointID: 17,
			Artifacts:       []contextpack.Artifact{{Name: "plan.md", SHA256: digest}},
			ArtifactVersion: "17|plan.md=" + digest, NextStage: "execute",
		},
	})
	for _, want := range []string{
		"artifact review", "checkpoint: 17", "artifact_version: 17|plan.md=" + digest,
		"next: execute", "plan.md: " + digest,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatDecision() missing %q:\n%s", want, got)
		}
	}
}

func TestFormatDecisionShowsPlanReviewPolicy(t *testing.T) {
	got := formatDecision(engine.PendingDecision{
		ID: 35, IssueID: "GH-35", Stage: "plan",
		D:            levers.Decision{Question: "Approve plan?", Options: []string{"approve", "reject"}},
		ReviewPolicy: &review.ResolvedPolicy{Mode: "regular", HumanRequired: true, PolicyID: "manual-default", PolicyVersion: "1", Reason: "manual_default"},
	})
	for _, want := range []string{"review policy: human approval required", "mode: regular", "policy: manual-default@1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("formatDecision() missing %q:\n%s", want, got)
		}
	}
}
