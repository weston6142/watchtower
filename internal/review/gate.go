package review

import (
	"errors"
	"fmt"
	"sort"
)

// Binding is an approval claim for one exact item under one policy evaluation.
// It is intentionally item-scoped so unrelated artifacts never share a stale
// boundary.
type Binding struct {
	Item           ItemBinding         `json:"item"`
	RequiredFloor  ApprovalFloor       `json:"required_floor"`
	EffectiveFloor ApprovalFloor       `json:"effective_floor"`
	PolicyID       string              `json:"policy_id"`
	PolicyVersion  string              `json:"policy_version"`
	Evidence       []Evidence          `json:"evidence,omitempty"`
	Dependencies   []DependencyBinding `json:"dependencies,omitempty"`
	Model          ModelMetadata       `json:"model,omitempty"`
}

type Result struct {
	Outcome Outcome `json:"outcome"`
	Err     error   `json:"-"`
}

// CheckApproval is the centralized pure comparison used by the engine gate.
// It re-evaluates current policy before trusting a recorded approval and fails
// closed for malformed context, policy, or binding data.
func CheckApproval(record Binding, current EscalationContext, approval *ApprovalProvenance) Result {
	if err := ValidateContext(current); err != nil {
		return Result{Outcome: OutcomeInvalidContext, Err: err}
	}
	evaluation := Evaluate(current)
	if evaluation.Outcome == OutcomeInvalidContext || evaluation.Outcome == OutcomePolicyError {
		return Result{Outcome: evaluation.Outcome, Err: evaluation.Err}
	}
	if err := validateBinding(record); err != nil {
		return Result{Outcome: OutcomeInvalidContext, Err: err}
	}
	if !sameItem(record.Item, current.Item) ||
		record.PolicyID != current.Policy.ID || record.PolicyVersion != current.Policy.Version ||
		record.RequiredFloor != evaluation.RequiredFloor || record.EffectiveFloor != evaluation.EffectiveFloor ||
		!sameDependencies(record.Dependencies, current.Dependencies) {
		return Result{Outcome: OutcomeStale}
	}
	if record.EffectiveFloor == FloorNone {
		return Result{Outcome: OutcomeApproved}
	}
	if approval == nil {
		return Result{Outcome: OutcomeRequiresApproval}
	}
	if err := validateGateApproval(approval); err != nil {
		return Result{Outcome: OutcomeRequiresApproval, Err: err}
	}
	switch record.EffectiveFloor {
	case FloorPolicy:
		if approval.Kind == ApprovalHuman ||
			(approval.Kind == ApprovalPolicy && approval.PolicyID == record.PolicyID && approval.PolicyVersion == record.PolicyVersion) {
			return Result{Outcome: OutcomeApproved}
		}
	case FloorOperator:
		if approval.Kind == ApprovalHuman {
			return Result{Outcome: OutcomeApproved}
		}
	default:
		return Result{Outcome: OutcomeInvalidContext, Err: errors.New("binding contains unknown approval floor")}
	}
	return Result{Outcome: OutcomeRequiresApproval}
}

func validateBinding(binding Binding) error {
	if err := validateItem(binding.Item); err != nil {
		return err
	}
	if !binding.RequiredFloor.valid() || !binding.EffectiveFloor.valid() || binding.RequiredFloor > binding.EffectiveFloor {
		return errors.New("binding contains an invalid approval floor")
	}
	if binding.PolicyID == "" || binding.PolicyVersion == "" {
		return errors.New("binding policy identity is incomplete")
	}
	seen := make(map[string]struct{}, len(binding.Dependencies))
	for _, dependency := range binding.Dependencies {
		if dependency.Kind == "" || dependency.ID == "" {
			return errors.New("binding dependency identity is incomplete")
		}
		if err := validateHash(dependency.Hash); err != nil {
			return err
		}
		key := dependency.Kind + "\x00" + dependency.ID
		if _, exists := seen[key]; exists {
			return fmt.Errorf("binding contains duplicate dependency %s/%s", dependency.Kind, dependency.ID)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateGateApproval(approval *ApprovalProvenance) error {
	switch approval.Kind {
	case ApprovalHuman:
		if approval.ActorID == "" {
			return errors.New("human approval actor is empty")
		}
	case ApprovalPolicy:
		if approval.PolicyID == "" || approval.PolicyVersion == "" {
			return errors.New("policy approval identity is incomplete")
		}
	default:
		return fmt.Errorf("unknown approval kind %q", approval.Kind)
	}
	return nil
}

func sameItem(left, right ItemBinding) bool {
	return left.Kind == right.Kind && left.Hash == right.Hash && left.Path == right.Path && left.Operation == right.Operation
}

func sameDependencies(left, right []DependencyBinding) bool {
	left = append([]DependencyBinding(nil), left...)
	right = append([]DependencyBinding(nil), right...)
	sort.Slice(left, func(i, j int) bool { return dependencyKey(left[i]) < dependencyKey(left[j]) })
	sort.Slice(right, func(i, j int) bool { return dependencyKey(right[i]) < dependencyKey(right[j]) })
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func dependencyKey(dependency DependencyBinding) string {
	return dependency.Kind + "\x00" + dependency.ID + "\x00" + dependency.Hash
}
