// Package retry defines durable retry policy and authorization evidence.
package retry

import (
	"fmt"
	"slices"
	"strings"

	"github.com/weston6142/watchtower/internal/failure"
)

type Kind string
type Behavior string
type Reason string

const (
	KindAutomatic     Kind = "automatic"
	KindExplicit      Kind = "explicit"
	KindModelResample Kind = "model-resample"

	BehaviorTransient     Behavior = "transient"
	BehaviorDeterministic Behavior = "deterministic"
	BehaviorModelResample Behavior = "model-resample"
	BehaviorNonRetryable  Behavior = "non-retryable"

	ReasonInvalidContext         Reason = "invalid_retry_context"
	ReasonFingerprintUnavailable Reason = "fingerprint_unavailable"
	ReasonStateUnchanged         Reason = "state_unchanged"
	ReasonKindNotAllowed         Reason = "retry_kind_not_allowed"
	ReasonCapExhausted           Reason = "retry_cap_exhausted"
	ReasonModelResampleExhausted Reason = "model_resample_cap_exhausted"
	ReasonPersistenceUnavailable Reason = "retry_persistence_unavailable"
	ReasonPersistenceConflict    Reason = "retry_persistence_conflict"
	ReasonNonRetryable           Reason = "failure_non_retryable"
)

type StateVector = failure.StateVector

type Policy struct {
	ID                   string
	Version              string
	TransientLimit       int
	DeterministicLimit   int
	ModelResampleLimit   int
	ModelResampleClasses []failure.Class
}

type PolicyEvidence struct {
	PolicyID              string          `json:"policy_id"`
	PolicyVersion         string          `json:"policy_version"`
	Behavior              Behavior        `json:"behavior"`
	SharedCap             int             `json:"shared_cap"`
	ModelResampleCap      int             `json:"model_resample_cap,omitempty"`
	ModelResampleEligible bool            `json:"model_resample_eligible"`
	EligibleClasses       []failure.Class `json:"model_resample_classes,omitempty"`
}

type Rule struct {
	Behavior              Behavior
	SharedCap             int
	RequiresStateChange   bool
	ModelResampleEligible bool
	ModelResampleCap      int
	Evidence              PolicyEvidence
}

func DefaultPolicy() Policy {
	return Policy{
		ID: "retry-v1", Version: "1", TransientLimit: 2, DeterministicLimit: 1,
		ModelResampleLimit:   1,
		ModelResampleClasses: []failure.Class{failure.ClassExecution, failure.ClassTransport, failure.ClassProtocol},
	}
}

type Request struct {
	IssueID          string      `json:"issue_id"`
	Stage            string      `json:"stage"`
	Kind             Kind        `json:"retry_kind"`
	Current          StateVector `json:"current"`
	DecisionIdentity string      `json:"decision_identity,omitempty"`
}

type Authorization struct {
	ContextKey         string         `json:"context_key"`
	IssueID            string         `json:"issue_id"`
	Stage              string         `json:"stage"`
	Kind               Kind           `json:"retry_kind"`
	FailureSite        failure.Site   `json:"failure_site"`
	FailureClass       failure.Class  `json:"failure_class"`
	FailureFingerprint string         `json:"failure_fingerprint"`
	Attempt            int            `json:"attempt"`
	SharedUsed         int            `json:"shared_used"`
	SharedCap          int            `json:"shared_cap"`
	ModelResampleUsed  int            `json:"model_resample_used,omitempty"`
	ModelResampleCap   int            `json:"model_resample_cap,omitempty"`
	DecisionIdentity   string         `json:"decision_identity,omitempty"`
	Current            StateVector    `json:"current"`
	ChangedDimensions  []string       `json:"changed_dimensions,omitempty"`
	Policy             PolicyEvidence `json:"policy"`
}

type Rejection struct {
	Reason                Reason         `json:"reason"`
	IssueID               string         `json:"issue_id"`
	Stage                 string         `json:"stage"`
	Kind                  Kind           `json:"retry_kind"`
	FailureClass          failure.Class  `json:"failure_class"`
	FailureFingerprint    string         `json:"failure_fingerprint"`
	SharedUsed            int            `json:"shared_used"`
	SharedCap             int            `json:"shared_cap"`
	ModelResampleUsed     int            `json:"model_resample_used,omitempty"`
	ModelResampleCap      int            `json:"model_resample_cap,omitempty"`
	ChangedDimensions     []string       `json:"changed_dimensions,omitempty"`
	UnchangedDimensions   []string       `json:"unchanged_dimensions,omitempty"`
	UnavailableDimensions []string       `json:"unavailable_dimensions,omitempty"`
	Policy                PolicyEvidence `json:"policy"`
	NextAction            string         `json:"next_action"`
}

func (p Policy) Validate() error {
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("retry policy ID is required")
	}
	if strings.TrimSpace(p.Version) == "" {
		return fmt.Errorf("retry policy version is required")
	}
	for name, limit := range map[string]int{
		"transient":      p.TransientLimit,
		"deterministic":  p.DeterministicLimit,
		"model-resample": p.ModelResampleLimit,
	} {
		if limit <= 0 {
			return fmt.Errorf("retry policy %s limit must be finite and positive", name)
		}
	}
	if len(p.ModelResampleClasses) == 0 {
		return fmt.Errorf("retry policy model-resample classes are required")
	}
	seen := make(map[failure.Class]struct{}, len(p.ModelResampleClasses))
	for _, class := range p.ModelResampleClasses {
		if !supportedClass(class) {
			return fmt.Errorf("unsupported model-resample class %q", class)
		}
		if _, ok := seen[class]; ok {
			return fmt.Errorf("duplicate model-resample class %q", class)
		}
		seen[class] = struct{}{}
	}
	return nil
}

func supportedClass(class failure.Class) bool {
	return class != failure.ClassUnknown && class != failure.ClassOther &&
		failure.NormalizeClass(class) == class
}

func (p Policy) Rule(class failure.Class, disposition failure.RetryDisposition) (Rule, error) {
	if err := p.Validate(); err != nil {
		return Rule{}, err
	}
	if !supportedClass(class) {
		return Rule{}, fmt.Errorf("unsupported durable failure class %q", class)
	}

	rule := Rule{}
	switch disposition {
	case failure.RetryNow:
		rule.Behavior = BehaviorTransient
		rule.SharedCap = p.TransientLimit
	case failure.RetryAfterStateChange:
		rule.Behavior = BehaviorDeterministic
		rule.SharedCap = p.DeterministicLimit
		rule.RequiresStateChange = true
	case failure.DoNotRetry:
		rule.Behavior = BehaviorNonRetryable
	default:
		return Rule{}, fmt.Errorf("unsupported durable retry disposition %q", disposition)
	}
	if rule.Behavior != BehaviorNonRetryable {
		rule.ModelResampleEligible = slices.Contains(p.ModelResampleClasses, class)
		rule.ModelResampleCap = p.ModelResampleLimit
	}
	rule.Evidence = PolicyEvidence{
		PolicyID: p.ID, PolicyVersion: p.Version, Behavior: rule.Behavior,
		SharedCap: rule.SharedCap, ModelResampleCap: rule.ModelResampleCap,
		ModelResampleEligible: rule.ModelResampleEligible,
		EligibleClasses:       append([]failure.Class(nil), p.ModelResampleClasses...),
	}
	return rule, nil
}
