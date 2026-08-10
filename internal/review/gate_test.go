package review_test

import (
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/review"
)

func TestExactItemBindingsAndApprovalGateOutcomes(t *testing.T) {
	bindings, err := (review.Target{
		IssueID: "GH-64", Stage: "plan", CheckpointID: 4, NextStage: "execute",
		Artifacts: artifacts(
			"touchset.json", strings.Repeat("b", 64),
			"plan.md", strings.Repeat("a", 64),
		),
	}).Bindings()
	if err != nil || len(bindings) != 2 {
		t.Fatalf("bindings = %+v, err = %v", bindings, err)
	}
	if bindings[0].Item.Kind != review.ItemArtifact || bindings[0].Item.Hash != strings.Repeat("a", 64) ||
		bindings[0].Item.Path != "plan.md" || bindings[0].Item.Operation != "approve-artifact" {
		t.Fatalf("first binding = %+v", bindings[0])
	}
	if bindings[1].Item.Path != "touchset.json" || bindings[1].Item.Hash != strings.Repeat("b", 64) {
		t.Fatalf("second binding = %+v", bindings[1])
	}

	current := gateContext(bindings[0].Item, nil)
	record := bindings[0]
	record.RequiredFloor, record.EffectiveFloor = review.FloorOperator, review.FloorOperator
	record.PolicyID, record.PolicyVersion = current.Policy.ID, current.Policy.Version
	approval := &review.ApprovalProvenance{Kind: review.ApprovalHuman, ActorID: "operator"}
	if result := review.CheckApproval(record, current, approval); result.Outcome != review.OutcomeApproved {
		t.Fatalf("approval result = %+v, want approved", result)
	}

	mutations := []struct {
		name   string
		mutate func(*review.EscalationContext)
	}{
		{name: "hash", mutate: func(ctx *review.EscalationContext) { ctx.Item.Hash = strings.Repeat("c", 64) }},
		{name: "path", mutate: func(ctx *review.EscalationContext) {
			ctx.Item.Path = "touchset.json"
			ctx.Paths = []string{"touchset.json"}
		}},
		{name: "operation", mutate: func(ctx *review.EscalationContext) { ctx.Operation = "publish"; ctx.Item.Operation = "publish" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			ctx := current
			mutation.mutate(&ctx)
			if result := review.CheckApproval(record, ctx, approval); result.Outcome != review.OutcomeStale {
				t.Fatalf("mutation result = %+v, want stale", result)
			}
		})
	}
}

func TestDependencyInvalidationDoesNotAffectUnrelatedBinding(t *testing.T) {
	item := review.ItemBinding{Kind: review.ItemRepair, Hash: strings.Repeat("a", 64), Path: "payments/charge.go", Operation: "repair"}
	dependencyA := review.DependencyBinding{Kind: "artifact", ID: "a", Hash: strings.Repeat("b", 64)}
	dependencyB := review.DependencyBinding{Kind: "artifact", ID: "b", Hash: strings.Repeat("c", 64)}
	dependencyC := review.DependencyBinding{Kind: "artifact", ID: "c", Hash: strings.Repeat("d", 64)}
	current := gateContext(item, []review.DependencyBinding{dependencyA, dependencyB})
	record := review.Binding{Item: item, RequiredFloor: review.FloorOperator, EffectiveFloor: review.FloorOperator,
		PolicyID: current.Policy.ID, PolicyVersion: current.Policy.Version,
		Dependencies: []review.DependencyBinding{dependencyA, dependencyB}}
	approval := &review.ApprovalProvenance{Kind: review.ApprovalHuman, ActorID: "operator"}
	changed := current
	changed.Dependencies = []review.DependencyBinding{dependencyA, dependencyC}
	if result := review.CheckApproval(record, changed, approval); result.Outcome != review.OutcomeStale {
		t.Fatalf("dependent mutation result = %+v, want stale", result)
	}

	unrelated := record
	unrelated.Dependencies = []review.DependencyBinding{dependencyC}
	independent := current
	independent.Dependencies = []review.DependencyBinding{dependencyC}
	if result := review.CheckApproval(unrelated, independent, approval); result.Outcome != review.OutcomeApproved {
		t.Fatalf("unrelated result = %+v, want approved", result)
	}
}

func TestApprovalGateFailsClosedForInvalidPolicyContextAndCurrentFloor(t *testing.T) {
	item := review.ItemBinding{Kind: review.ItemDecision, Hash: strings.Repeat("a", 64), Path: "decision.json", Operation: "decide"}
	current := gateContext(item, nil)
	record := review.Binding{Item: item, RequiredFloor: review.FloorOperator, EffectiveFloor: review.FloorOperator,
		PolicyID: current.Policy.ID, PolicyVersion: current.Policy.Version}
	approval := &review.ApprovalProvenance{Kind: review.ApprovalHuman, ActorID: "operator"}

	invalid := current
	invalid.Stage = ""
	if result := review.CheckApproval(record, invalid, approval); result.Outcome != review.OutcomeInvalidContext {
		t.Fatalf("invalid context result = %+v", result)
	}
	unevaluable := current
	unevaluable.Policy.Valid = false
	if result := review.CheckApproval(record, unevaluable, approval); result.Outcome != review.OutcomePolicyError {
		t.Fatalf("policy result = %+v", result)
	}
	identityChanged := current
	identityChanged.Policy.ID = "other-policy"
	if result := review.CheckApproval(record, identityChanged, approval); result.Outcome != review.OutcomeStale {
		t.Fatalf("policy identity result = %+v", result)
	}
	floorChanged := current
	floorChanged.Policy.StageFloors["execute"] = review.FloorPolicy
	if result := review.CheckApproval(record, floorChanged, approval); result.Outcome != review.OutcomeStale {
		t.Fatalf("current floor result = %+v", result)
	}
}

func gateContext(item review.ItemBinding, dependencies []review.DependencyBinding) review.EscalationContext {
	return review.EscalationContext{
		Stage: "execute", Operation: item.Operation, Paths: []string{item.Path}, Item: item, Dependencies: dependencies,
		Policy: review.Policy{
			ID: "team-safety", Version: "7", Valid: true,
			StageFloors:     map[string]review.ApprovalFloor{"execute": review.FloorOperator},
			OperationFloors: map[string]review.ApprovalFloor{},
			DefaultFloor:    review.FloorNone, DestructiveFloor: review.FloorNone, PublicationFloor: review.FloorNone,
		},
	}
}

func artifacts(firstName, firstHash, secondName, secondHash string) []contextpack.Artifact {
	return []contextpack.Artifact{{Name: firstName, SHA256: firstHash}, {Name: secondName, SHA256: secondHash}}
}
