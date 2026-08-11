package retry_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/retry"
)

func testDigest(value string) string {
	sum := strings.Repeat(value, 64)
	return "sha256:" + sum[:64]
}

func testVector(tree, config, environment, decision string) retry.StateVector {
	return retry.StateVector{
		TreeDigest: testDigest(tree), ConfigDigest: testDigest(config),
		EnvironmentDigest: testDigest(environment), DecisionDigest: testDigest(decision),
	}
}

func testPolicy(shared int) retry.Policy {
	return retry.Policy{
		ID: "test", Version: "1", TransientLimit: shared, DeterministicLimit: shared,
		ModelResampleLimit: 1, ModelResampleClasses: []failure.Class{failure.ClassExecution},
	}
}

type memoryContextStore struct {
	mu        sync.Mutex
	context   retry.Context
	conflicts int
}

func (s *memoryContextStore) LoadRetryContext(context.Context, string, string) (retry.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.context.ContextKey == "" {
		return retry.Context{}, retry.ErrContextNotFound
	}
	return s.context, nil
}

func (s *memoryContextStore) AuthorizeRetry(_ context.Context, update retry.AuthorizationUpdate) (retry.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conflicts > 0 {
		s.conflicts--
		return retry.Context{}, retry.ErrAuthorizationConflict
	}
	if s.context.ContextKey != update.ContextKey || s.context.Version != update.ExpectedVersion {
		return retry.Context{}, retry.ErrAuthorizationConflict
	}
	if s.context.SharedUsed >= update.SharedCap ||
		(update.Kind == retry.KindModelResample && s.context.ModelResampleUsed >= update.ModelResampleCap) {
		return retry.Context{}, retry.ErrAuthorizationConflict
	}
	s.context.SharedUsed++
	if update.Kind == retry.KindModelResample {
		s.context.ModelResampleUsed++
	}
	s.context.SharedCap = update.SharedCap
	s.context.ModelResampleCap = update.ModelResampleCap
	s.context.State = update.Current
	s.context.Policy = update.Policy
	s.context.Version++
	return s.context, nil
}

func activeContext(disposition failure.RetryDisposition, state retry.StateVector) retry.Context {
	return retry.Context{
		ContextKey: "context-1", SchemaVersion: retry.ContextSchemaVersion,
		IssueID: "GH-65", Stage: "execute", FailureSite: failure.SiteVerification,
		FailureClass: failure.ClassExecution, RetryDisposition: disposition,
		FailureFingerprint: testDigest("f"), State: state,
		Lifecycle: retry.ContextActive, Version: 1,
	}
}

func TestAuthorizeRetryUsesDurableContextAndCompareAndUpdate(t *testing.T) {
	prior := testVector("a", "b", "c", "d")
	store := &memoryContextStore{context: activeContext(failure.RetryNow, prior)}
	gate, err := retry.NewGate(store, testPolicy(1))
	if err != nil {
		t.Fatal(err)
	}
	current := prior
	current.TreeDigest = testDigest("e")

	first, err := gate.Authorize(context.Background(), retry.Request{
		IssueID: "GH-65", Stage: "execute", Kind: retry.KindAutomatic, Current: current,
	})
	if err != nil || !first.Authorized || first.Authorization == nil ||
		first.Authorization.Attempt != 1 || first.Authorization.SharedUsed != 1 {
		t.Fatalf("first authorization = %+v, err=%v", first, err)
	}
	second, err := gate.Authorize(context.Background(), retry.Request{
		IssueID: "GH-65", Stage: "execute", Kind: retry.KindExplicit, Current: current,
	})
	if err != nil || second.Authorized || second.Rejection == nil || second.Rejection.Reason != retry.ReasonCapExhausted {
		t.Fatalf("second authorization = %+v, err=%v", second, err)
	}
}

func TestAuthorizeRetryRequiresMeaningfulDeterministicChange(t *testing.T) {
	prior := testVector("a", "b", "c", "d")
	for _, dimension := range []string{"tree", "config", "environment", "decision"} {
		t.Run(dimension, func(t *testing.T) {
			store := &memoryContextStore{context: activeContext(failure.RetryAfterStateChange, prior)}
			gate, err := retry.NewGate(store, testPolicy(1))
			if err != nil {
				t.Fatal(err)
			}
			unchanged, err := gate.Authorize(context.Background(), retry.Request{
				IssueID: "GH-65", Stage: "execute", Kind: retry.KindExplicit, Current: prior,
			})
			if err != nil || unchanged.Rejection == nil || unchanged.Rejection.Reason != retry.ReasonStateUnchanged ||
				store.context.SharedUsed != 0 || len(unchanged.Rejection.UnchangedDimensions) != 4 {
				t.Fatalf("unchanged result = %+v, context=%+v, err=%v", unchanged, store.context, err)
			}

			current := prior
			switch dimension {
			case "tree":
				current.TreeDigest = testDigest("e")
			case "config":
				current.ConfigDigest = testDigest("e")
			case "environment":
				current.EnvironmentDigest = testDigest("e")
			case "decision":
				current.DecisionDigest = testDigest("e")
			}
			changed, err := gate.Authorize(context.Background(), retry.Request{
				IssueID: "GH-65", Stage: "execute", Kind: retry.KindExplicit, Current: current,
			})
			if err != nil || !changed.Authorized || changed.Authorization == nil ||
				len(changed.Authorization.ChangedDimensions) != 1 || changed.Authorization.ChangedDimensions[0] != dimension {
				t.Fatalf("changed result = %+v, err=%v", changed, err)
			}
		})
	}
}

func TestAuthorizeRetryFailsClosedAndReloadsOnlyOnce(t *testing.T) {
	prior := testVector("a", "b", "c", "d")
	current := prior
	current.TreeDigest = testDigest("e")

	unavailableStore := &memoryContextStore{context: activeContext(failure.RetryNow, prior)}
	unavailableGate, _ := retry.NewGate(unavailableStore, testPolicy(1))
	unavailable := current
	unavailable.EnvironmentDigest = failure.Unavailable
	result, err := unavailableGate.Authorize(context.Background(), retry.Request{
		IssueID: "GH-65", Stage: "execute", Kind: retry.KindAutomatic, Current: unavailable,
	})
	if err != nil || result.Rejection == nil || result.Rejection.Reason != retry.ReasonFingerprintUnavailable || unavailableStore.context.SharedUsed != 0 {
		t.Fatalf("unavailable result = %+v, context=%+v, err=%v", result, unavailableStore.context, err)
	}

	oneConflict := &memoryContextStore{context: activeContext(failure.RetryNow, prior), conflicts: 1}
	oneGate, _ := retry.NewGate(oneConflict, testPolicy(1))
	result, err = oneGate.Authorize(context.Background(), retry.Request{
		IssueID: "GH-65", Stage: "execute", Kind: retry.KindAutomatic, Current: current,
	})
	if err != nil || !result.Authorized {
		t.Fatalf("one-conflict result = %+v, err=%v", result, err)
	}

	twoConflicts := &memoryContextStore{context: activeContext(failure.RetryNow, prior), conflicts: 2}
	twoGate, _ := retry.NewGate(twoConflicts, testPolicy(1))
	result, err = twoGate.Authorize(context.Background(), retry.Request{
		IssueID: "GH-65", Stage: "execute", Kind: retry.KindAutomatic, Current: current,
	})
	if err != nil || result.Rejection == nil || result.Rejection.Reason != retry.ReasonPersistenceConflict || twoConflicts.context.SharedUsed != 0 {
		t.Fatalf("two-conflict result = %+v, context=%+v, err=%v", result, twoConflicts.context, err)
	}
}

func TestFingerprintBuildsIndependentCanonicalDimensions(t *testing.T) {
	inputs := failure.FingerprintInputs{
		IssueID: "GH-65", Stage: "execute", FailureSite: failure.SiteVerification,
		ConfigurationIdentity: testDigest("a"), EnvironmentIdentity: testDigest("b"),
		WatchtowerIdentity: testDigest("1"),
		Git:                failure.GitIdentity{Repository: testDigest("c"), BaseCommit: testDigest("d"), BranchCommit: testDigest("e"), TreeIdentity: testDigest("f")},
		Decisions:          []failure.ContentIdentity{{Identity: "2", ContentHash: testDigest("a")}, {Identity: "1", ContentHash: testDigest("b")}},
		Verification:       failure.VerificationIdentity{CommandIdentity: testDigest("c"), CacheIdentity: testDigest("d")},
	}
	first := retry.BuildStateVector(inputs)
	inputs.Decisions[0], inputs.Decisions[1] = inputs.Decisions[1], inputs.Decisions[0]
	if second := retry.BuildStateVector(inputs); first != second {
		t.Fatalf("canonical vector changed after reordering: %+v != %+v", first, second)
	}
	changedInput := inputs
	changedInput.EnvironmentIdentity = testDigest("e")
	changed := retry.BuildStateVector(changedInput)
	dimensions, unchanged := retry.ChangedDimensions(first, changed)
	if len(dimensions) != 1 || dimensions[0] != "environment" || len(unchanged) != 3 {
		t.Fatalf("dimensions = changed=%v unchanged=%v", dimensions, unchanged)
	}
}

func TestFingerprintMarksIncompleteDimensionInputsUnavailable(t *testing.T) {
	complete := failure.FingerprintInputs{
		IssueID: "GH-65", Stage: "execute", FailureSite: failure.SiteVerification,
		ConfigurationIdentity: testDigest("a"), EnvironmentIdentity: testDigest("b"),
		WatchtowerIdentity: testDigest("c"),
		Git: failure.GitIdentity{
			Repository: testDigest("d"), BaseCommit: testDigest("e"),
			BranchCommit: testDigest("f"), TreeIdentity: testDigest("1"),
		},
		Decisions: []failure.ContentIdentity{{Identity: "1", ContentHash: testDigest("2")}},
		Verification: failure.VerificationIdentity{
			CommandIdentity: testDigest("3"), CacheIdentity: testDigest("4"),
		},
	}

	for name, tc := range map[string]struct {
		dimension string
		mutate    func(*failure.FingerprintInputs)
	}{
		"tree repository":     {"tree", func(inputs *failure.FingerprintInputs) { inputs.Git.Repository = "" }},
		"config verification": {"config", func(inputs *failure.FingerprintInputs) { inputs.Verification.CommandIdentity = "" }},
		"environment runtime": {"environment", func(inputs *failure.FingerprintInputs) { inputs.WatchtowerIdentity = "" }},
		"decision digest":     {"decision", func(inputs *failure.FingerprintInputs) { inputs.Decisions[0].ContentHash = "" }},
	} {
		t.Run(name, func(t *testing.T) {
			inputs := complete
			inputs.Decisions = append([]failure.ContentIdentity(nil), complete.Decisions...)
			tc.mutate(&inputs)
			vector := retry.BuildStateVector(inputs)
			unavailable := retry.UnavailableDimensions(vector)
			if len(unavailable) != 1 || unavailable[0] != tc.dimension {
				t.Fatalf("unavailable dimensions = %v, want [%s]; vector=%+v", unavailable, tc.dimension, vector)
			}
		})
	}

	withoutDecisions := complete
	withoutDecisions.Decisions = nil
	if vector := retry.BuildStateVector(withoutDecisions); vector.DecisionDigest == failure.Unavailable {
		t.Fatalf("empty durable decision set was marked unavailable: %+v", vector)
	}
}

func TestRetryEventEvidenceIsStructuredAndSanitized(t *testing.T) {
	authorization := retry.Authorization{
		IssueID: "GH-65", Stage: "execute", Kind: retry.KindAutomatic,
		FailureClass: failure.ClassExecution, FailureFingerprint: testDigest("f"),
		Attempt: 1, SharedUsed: 1, SharedCap: 2, Current: testVector("a", "b", "c", "d"),
		Policy: retry.PolicyEvidence{PolicyID: "test", PolicyVersion: "1", Behavior: retry.BehaviorTransient, SharedCap: 2},
	}
	event, err := core.NewEvent(core.EvRetryAuthorized, "GH-65", core.RetryAuthorizedPayload(authorization))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"issue_id", "stage", "retry_kind", "failure_class", "failure_fingerprint", "shared_used", "shared_cap", "policy"} {
		if _, ok := payload[field]; !ok {
			t.Fatalf("retry payload omitted %q: %s", field, event.Payload)
		}
	}
	for _, raw := range []string{"SECRET_API_TOKEN=value", "raw model sample", "primary exception", "run dangerous command", "artifact bytes", "decision text"} {
		if strings.Contains(string(event.Payload), raw) {
			t.Fatalf("retry event leaked %q: %s", raw, event.Payload)
		}
	}
	if !errors.Is(retry.ErrAuthorizationConflict, retry.ErrAuthorizationConflict) {
		t.Fatal("typed retry conflict is not comparable")
	}
}
