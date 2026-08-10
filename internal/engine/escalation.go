package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/review"
)

func defaultDecisionEscalationPolicy() review.EscalationPolicy {
	return review.EscalationPolicy{
		ID: review.ManualPolicyID, Version: review.ManualPolicyVersion, Valid: true,
		DefaultFloor: review.FloorNone,
		StageFloors:  map[string]review.ApprovalFloor{}, OperationFloors: map[string]review.ApprovalFloor{},
		DestructiveFloor: review.FloorOperator, PublicationFloor: review.FloorOperator,
	}
}

type decisionHashEnvelope struct {
	Stage     string          `json:"stage"`
	Operation string          `json:"operation"`
	Paths     []string        `json:"paths"`
	Decision  levers.Decision `json:"decision"`
}

func (e *Engine) decisionEscalationContext(stage string, d levers.Decision) (review.EscalationContext, review.ItemBinding, error) {
	paths := append([]string(nil), d.Paths...)
	if len(paths) == 0 {
		paths = []string{"decision.json"}
	}
	operation := "decision"
	envelope, err := json.Marshal(decisionHashEnvelope{Stage: stage, Operation: operation, Paths: paths, Decision: d})
	if err != nil {
		return review.EscalationContext{}, review.ItemBinding{}, fmt.Errorf("hash decision envelope: %w", err)
	}
	digest := sha256.Sum256(envelope)
	item := review.ItemBinding{
		Kind: review.ItemDecision, Hash: hex.EncodeToString(digest[:]), Path: paths[0], Operation: operation,
	}
	model := review.ModelMetadata{Importance: &d.Importance, Options: append([]string(nil), d.Options...), Rationale: d.Why}
	ctx := review.EscalationContext{
		Stage: stage, Operation: operation, Paths: paths, RiskFactsValid: true, Item: item,
		Policy: e.cfg.DecisionPolicy, Model: model,
	}
	return ctx, item, nil
}

func (e *Engine) evaluateDecisionEscalation(stage string, d levers.Decision) (review.EscalationContext, review.Evaluation, review.Binding, error) {
	ctx, item, err := e.decisionEscalationContext(stage, d)
	if err != nil {
		return review.EscalationContext{}, review.Evaluation{}, review.Binding{}, err
	}
	evaluation := review.Evaluate(ctx)
	if evaluation.Err != nil {
		return ctx, evaluation, review.Binding{}, evaluation.Err
	}
	binding := review.Binding{
		Item: item, RequiredFloor: evaluation.RequiredFloor, EffectiveFloor: evaluation.EffectiveFloor,
		PolicyID: evaluation.PolicyID, PolicyVersion: evaluation.PolicyVersion,
		Evidence:     append([]review.Evidence(nil), evaluation.Evidence...),
		Dependencies: append([]review.DependencyBinding(nil), evaluation.Dependencies...), Model: evaluation.Model,
	}
	return ctx, evaluation, binding, nil
}

func (e *Engine) evaluateReviewTarget(target review.Target, publicationRisk bool) (review.Evaluation, []review.Binding, []review.EscalationContext, error) {
	canonical, err := target.Canonical()
	if err != nil {
		return review.Evaluation{}, nil, nil, err
	}
	bindings, err := canonical.Bindings()
	if err != nil {
		return review.Evaluation{}, nil, nil, err
	}
	if len(bindings) == 0 {
		return review.Evaluation{}, nil, nil, fmt.Errorf("review target contains no exact items")
	}
	contexts := make([]review.EscalationContext, 0, len(bindings))
	var aggregate review.Evaluation
	for index := range bindings {
		item := bindings[index].Item
		ctx := review.EscalationContext{
			Stage:           canonical.Stage,
			Operation:       item.Operation,
			Paths:           []string{item.Path},
			RiskFactsValid:  true,
			PublicationRisk: publicationRisk,
			Policy:          e.cfg.DecisionPolicy,
			Item:            item,
		}
		evaluation := review.Evaluate(ctx)
		if evaluation.Err != nil {
			return review.Evaluation{}, nil, nil, evaluation.Err
		}
		bindings[index] = review.Binding{
			Item:           item,
			RequiredFloor:  evaluation.RequiredFloor,
			EffectiveFloor: evaluation.EffectiveFloor,
			PolicyID:       evaluation.PolicyID,
			PolicyVersion:  evaluation.PolicyVersion,
			Evidence:       append([]review.Evidence(nil), evaluation.Evidence...),
			Dependencies:   append([]review.DependencyBinding(nil), evaluation.Dependencies...),
			Model:          evaluation.Model,
		}
		if index == 0 {
			aggregate = evaluation
		} else {
			if evaluation.RequiredFloor > aggregate.RequiredFloor {
				aggregate.RequiredFloor = evaluation.RequiredFloor
			}
			if evaluation.EffectiveFloor > aggregate.EffectiveFloor {
				aggregate.EffectiveFloor = evaluation.EffectiveFloor
			}
			aggregate.Evidence = append(aggregate.Evidence, evaluation.Evidence...)
		}
		contexts = append(contexts, ctx)
	}
	if aggregate.EffectiveFloor == review.FloorNone {
		aggregate.Outcome = review.OutcomeApproved
	} else {
		aggregate.Outcome = review.OutcomeRequiresApproval
	}
	return aggregate, bindings, contexts, nil
}

func checkReviewTarget(bindings []review.Binding, contexts []review.EscalationContext, approval *review.ApprovalProvenance) review.Result {
	if len(bindings) == 0 || len(bindings) != len(contexts) {
		return review.Result{Outcome: review.OutcomeInvalidContext, Err: fmt.Errorf("review approval binding count is incomplete")}
	}
	for index := range bindings {
		result := review.CheckApproval(bindings[index], contexts[index], approval)
		if result.Outcome != review.OutcomeApproved {
			return result
		}
	}
	return review.Result{Outcome: review.OutcomeApproved}
}

func hasReviewFloor(bindings []review.Binding, minimum review.ApprovalFloor) bool {
	for _, binding := range bindings {
		if binding.EffectiveFloor >= minimum {
			return true
		}
	}
	return false
}
