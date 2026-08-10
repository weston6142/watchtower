package review_test

import (
	"math"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/review"
)

func TestEvaluateUsesStrictestIndependentFloor(t *testing.T) {
	importance := 0.0
	result := review.Evaluate(review.EscalationContext{
		Stage: "execute", Operation: "repair",
		Paths:           []string{"payments/charge.go"},
		DestructiveRisk: true, PublicationRisk: false,
		Policy: review.Policy{
			ID: "team-safety", Version: "7", Valid: true,
			DefaultFloor:     review.FloorNone,
			StageFloors:      map[string]review.ApprovalFloor{"execute": review.FloorPolicy},
			OperationFloors:  map[string]review.ApprovalFloor{"repair": review.FloorOperator},
			PathFloors:       []review.PathFloor{{Glob: "payments/**", Floor: review.FloorOperator}},
			DestructiveFloor: review.FloorPolicy,
			PublicationFloor: review.FloorOperator,
		},
		Item: review.ItemBinding{
			Kind: review.ItemRepair, Hash: strings.Repeat("a", 64),
			Path: "payments/charge.go", Operation: "repair",
		},
		Model: review.ModelMetadata{Importance: &importance},
	})
	if result.Outcome != review.OutcomeRequiresApproval ||
		result.RequiredFloor != review.FloorOperator || result.EffectiveFloor != review.FloorOperator {
		t.Fatalf("evaluation = %+v, want operator requires-approval", result)
	}
	if result.PolicyID != "team-safety" || result.PolicyVersion != "7" ||
		!hasSignal(result.Evidence, "stage") || !hasSignal(result.Evidence, "operation") ||
		!hasSignal(result.Evidence, "path") || !hasSignal(result.Evidence, "destructive") {
		t.Fatalf("evaluation lost policy evidence: %+v", result)
	}
}

func TestEvaluateRaisesForEachIndependentRiskSignal(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*review.EscalationContext)
		want   review.ApprovalFloor
	}{
		{name: "stage", mutate: func(ctx *review.EscalationContext) { ctx.Stage = "merge-verification" }, want: review.FloorPolicy},
		{name: "operation", mutate: func(ctx *review.EscalationContext) { ctx.Operation = "publish"; ctx.Item.Operation = "publish" }, want: review.FloorPolicy},
		{name: "path", mutate: func(ctx *review.EscalationContext) {
			ctx.Paths = []string{"payments/charge.go"}
			ctx.Item.Path = "payments/charge.go"
		}, want: review.FloorPolicy},
		{name: "destructive", mutate: func(ctx *review.EscalationContext) { ctx.DestructiveRisk = true }, want: review.FloorPolicy},
		{name: "publication", mutate: func(ctx *review.EscalationContext) { ctx.PublicationRisk = true }, want: review.FloorPolicy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := baseContext()
			tc.mutate(&ctx)
			got := review.Evaluate(ctx)
			if got.RequiredFloor != tc.want || !hasSignal(got.Evidence, tc.name) {
				t.Fatalf("evaluation = %+v, want floor %v with %s evidence", got, tc.want, tc.name)
			}
		})
	}
}

func TestEvaluateDoesNotTrustModelImportanceToLowerPolicyFloor(t *testing.T) {
	for _, importance := range []*float64{nil, float64Pointer(0), float64Pointer(-1), float64Pointer(0.1), float64Pointer(math.NaN())} {
		ctx := baseContext()
		ctx.Policy.StageFloors["execute"] = review.FloorOperator
		ctx.Model.Importance = importance
		got := review.Evaluate(ctx)
		if got.RequiredFloor != review.FloorOperator || got.EffectiveFloor != review.FloorOperator ||
			got.Outcome != review.OutcomeRequiresApproval {
			t.Fatalf("importance %v lowered engine floor: %+v", importance, got)
		}
	}
}

func TestEvaluatePreservesStricterModelRequestWithoutChangingRequiredFloor(t *testing.T) {
	importance := 1.0
	ctx := baseContext()
	ctx.Model = review.ModelMetadata{Importance: &importance, Options: []string{"hold"}, Rationale: "uncertain"}
	got := review.Evaluate(ctx)
	if got.RequiredFloor != review.FloorNone || got.EffectiveFloor != review.FloorOperator ||
		got.Outcome != review.OutcomeRequiresApproval || got.Model.Rationale != "uncertain" {
		t.Fatalf("evaluation = %+v, want stricter model request retained", got)
	}
}

func TestEvaluateFailsClosedForInvalidContextAndPolicy(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*review.EscalationContext)
		want   review.Outcome
	}{
		{name: "missing stage", mutate: func(ctx *review.EscalationContext) { ctx.Stage = "" }, want: review.OutcomeInvalidContext},
		{name: "unsafe operation", mutate: func(ctx *review.EscalationContext) { ctx.Operation = "../repair" }, want: review.OutcomeInvalidContext},
		{name: "unsafe path", mutate: func(ctx *review.EscalationContext) { ctx.Paths = []string{"../secret"} }, want: review.OutcomeInvalidContext},
		{name: "invalid hash", mutate: func(ctx *review.EscalationContext) { ctx.Item.Hash = "bad" }, want: review.OutcomeInvalidContext},
		{name: "duplicate dependency", mutate: func(ctx *review.EscalationContext) {
			ctx.Dependencies = []review.DependencyBinding{{Kind: "artifact", ID: "a", Hash: strings.Repeat("b", 64)}, {Kind: "artifact", ID: "a", Hash: strings.Repeat("c", 64)}}
		}, want: review.OutcomeInvalidContext},
		{name: "invalid policy", mutate: func(ctx *review.EscalationContext) { ctx.Policy.Valid = false }, want: review.OutcomePolicyError},
		{name: "unknown floor", mutate: func(ctx *review.EscalationContext) { ctx.Policy.DefaultFloor = review.ApprovalFloor(99) }, want: review.OutcomePolicyError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := baseContext()
			tc.mutate(&ctx)
			if got := review.Evaluate(ctx); got.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", got.Outcome, tc.want)
			}
		})
	}
}

func baseContext() review.EscalationContext {
	return review.EscalationContext{
		Stage: "execute", Operation: "review", Paths: []string{"internal/review/escalation.go"},
		Policy: review.Policy{
			ID: "team-safety", Version: "7", Valid: true, DefaultFloor: review.FloorNone,
			StageFloors:      map[string]review.ApprovalFloor{"merge-verification": review.FloorPolicy},
			OperationFloors:  map[string]review.ApprovalFloor{"publish": review.FloorPolicy},
			PathFloors:       []review.PathFloor{{Glob: "payments/**", Floor: review.FloorPolicy}},
			DestructiveFloor: review.FloorPolicy, PublicationFloor: review.FloorPolicy,
		},
		Item: review.ItemBinding{Kind: review.ItemArtifact, Hash: strings.Repeat("a", 64), Path: "internal/review/escalation.go", Operation: "review"},
	}
}

func float64Pointer(value float64) *float64 { return &value }

func hasSignal(evidence []review.Evidence, want string) bool {
	for _, item := range evidence {
		if item.Signal == want {
			return true
		}
	}
	return false
}
