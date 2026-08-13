package retry_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/retry"
)

func TestPolicyMapsTypedDispositionAndRejectsUnsupportedEvidence(t *testing.T) {
	policy := retry.Policy{
		ID: "test", Version: "1",
		TransientLimit: 2, DeterministicLimit: 1, ModelResampleLimit: 1,
		ModelResampleClasses: []failure.Class{failure.ClassExecution},
	}

	transient, err := policy.Rule(failure.ClassExecution, failure.RetryNow)
	if err != nil || transient.Behavior != retry.BehaviorTransient || transient.SharedCap != 2 {
		t.Fatalf("transient rule = %+v, err=%v", transient, err)
	}
	if !transient.ModelResampleEligible || transient.ModelResampleCap != 1 {
		t.Fatalf("transient model-resample evidence = %+v", transient)
	}

	deterministic, err := policy.Rule(failure.ClassValidation, failure.RetryAfterStateChange)
	if err != nil || deterministic.Behavior != retry.BehaviorDeterministic ||
		!deterministic.RequiresStateChange || deterministic.SharedCap != 1 {
		t.Fatalf("deterministic rule = %+v, err=%v", deterministic, err)
	}
	if deterministic.ModelResampleEligible {
		t.Fatalf("validation unexpectedly model-resample eligible: %+v", deterministic)
	}

	nonRetryable, err := policy.Rule(failure.ClassAuthorization, failure.DoNotRetry)
	if err != nil || nonRetryable.Behavior != retry.BehaviorNonRetryable || nonRetryable.SharedCap != 0 {
		t.Fatalf("non-retryable rule = %+v, err=%v", nonRetryable, err)
	}
	configuredNonRetryable, err := policy.Rule(failure.ClassExecution, failure.DoNotRetry)
	if err != nil || configuredNonRetryable.Behavior != retry.BehaviorNonRetryable ||
		configuredNonRetryable.ModelResampleEligible {
		t.Fatalf("configured non-retryable rule = %+v, err=%v", configuredNonRetryable, err)
	}

	for name, tc := range map[string]struct {
		class       failure.Class
		disposition failure.RetryDisposition
	}{
		"unknown class":       {failure.ClassUnknown, failure.RetryNow},
		"other class":         {failure.ClassOther, failure.RetryNow},
		"unknown disposition": {failure.ClassExecution, failure.RetryUnknown},
		"other disposition":   {failure.ClassExecution, failure.RetryOther},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := policy.Rule(tc.class, tc.disposition); err == nil {
				t.Fatal("unsupported durable evidence was accepted")
			}
		})
	}
}

func TestPolicyValidationRequiresFiniteVisibleConfiguration(t *testing.T) {
	valid := retry.Policy{
		ID: "retry-v1", Version: "1",
		TransientLimit: 2, DeterministicLimit: 1, ModelResampleLimit: 1,
		ModelResampleClasses: []failure.Class{failure.ClassExecution, failure.ClassTransport, failure.ClassProtocol},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}

	for name, mutate := range map[string]func(*retry.Policy){
		"missing ID":              func(p *retry.Policy) { p.ID = "" },
		"missing version":         func(p *retry.Policy) { p.Version = "" },
		"zero transient cap":      func(p *retry.Policy) { p.TransientLimit = 0 },
		"negative deterministic":  func(p *retry.Policy) { p.DeterministicLimit = -1 },
		"zero model-resample cap": func(p *retry.Policy) { p.ModelResampleLimit = 0 },
		"unknown eligible class":  func(p *retry.Policy) { p.ModelResampleClasses = []failure.Class{failure.ClassUnknown} },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.ModelResampleClasses = append([]failure.Class(nil), valid.ModelResampleClasses...)
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid policy was accepted")
			}
		})
	}

	equalFinite := valid
	equalFinite.TransientLimit = 1
	if err := equalFinite.Validate(); err != nil {
		t.Fatalf("equal finite limits rejected: %v", err)
	}
}

func TestPolicyEvidenceContainsOnlySanitizedPolicyMetadata(t *testing.T) {
	policy := retry.Policy{
		ID: "retry-v1", Version: "1",
		TransientLimit: 2, DeterministicLimit: 1, ModelResampleLimit: 1,
		ModelResampleClasses: []failure.Class{failure.ClassExecution},
	}
	rule, err := policy.Rule(failure.ClassExecution, failure.RetryNow)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(rule.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, raw := range []string{"raw environment", "model sample", "exception text", "command text"} {
		if strings.Contains(text, raw) {
			t.Fatalf("policy evidence leaked %q: %s", raw, text)
		}
	}
	for _, field := range []string{"policy_id", "policy_version", "behavior", "shared_cap"} {
		if !strings.Contains(text, `"`+field+`"`) {
			t.Fatalf("policy evidence omitted %q: %s", field, text)
		}
	}
}
