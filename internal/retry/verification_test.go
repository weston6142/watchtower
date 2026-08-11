package retry_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/retry"
)

type verificationStore struct {
	mu        sync.Mutex
	stored    retry.Context
	conflicts int
}

func (s *verificationStore) LoadRetryContext(_ context.Context, issueID, stage string) (retry.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stored.ContextKey == "" || s.stored.IssueID != issueID || s.stored.Stage != stage {
		return retry.Context{}, retry.ErrContextNotFound
	}
	return s.stored, nil
}

func (s *verificationStore) AuthorizeRetry(_ context.Context, update retry.AuthorizationUpdate) (retry.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conflicts > 0 {
		s.conflicts--
		return retry.Context{}, retry.ErrAuthorizationConflict
	}
	if update.ContextKey != s.stored.ContextKey || update.ExpectedVersion != s.stored.Version ||
		s.stored.SharedUsed >= update.SharedCap ||
		(update.Kind == retry.KindModelResample && s.stored.ModelResampleUsed >= update.ModelResampleCap) {
		return retry.Context{}, retry.ErrAuthorizationConflict
	}
	s.stored.SharedUsed++
	if update.Kind == retry.KindModelResample {
		s.stored.ModelResampleUsed++
		s.stored.DecisionIdentity = update.DecisionIdentity
	}
	s.stored.SharedCap = update.SharedCap
	s.stored.ModelResampleCap = update.ModelResampleCap
	s.stored.State = update.Current
	s.stored.Policy = update.Policy
	s.stored.Version++
	return s.stored, nil
}

func verificationDigest(label string) string {
	digest := sha256.Sum256([]byte(label))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func verificationState(tree, config, environment, decision string) retry.StateVector {
	return retry.StateVector{
		TreeDigest: verificationDigest(tree), ConfigDigest: verificationDigest(config),
		EnvironmentDigest: verificationDigest(environment), DecisionDigest: verificationDigest(decision),
	}
}

func verificationPolicy(shared, model int) retry.Policy {
	return retry.Policy{
		ID: "verification", Version: "1", TransientLimit: shared,
		DeterministicLimit: shared, ModelResampleLimit: model,
		ModelResampleClasses: []failure.Class{failure.ClassExecution},
	}
}

func verificationContext(disposition failure.RetryDisposition, state retry.StateVector) retry.Context {
	return retry.Context{
		ContextKey: verificationDigest("context"), SchemaVersion: retry.ContextSchemaVersion,
		LatestRecordID: 1, IssueID: "GH-65", Stage: "execute",
		FailureSite: failure.SiteRunner, FailureClass: failure.ClassExecution,
		RetryDisposition: disposition, FailureFingerprint: verificationDigest("failure"),
		State: state, Lifecycle: retry.ContextActive, Version: 1,
	}
}

func authorizeVerification(t *testing.T, gate *retry.Gate, kind retry.Kind, state retry.StateVector, decision string) retry.Decision {
	t.Helper()
	result, err := gate.Authorize(context.Background(), retry.Request{
		IssueID: "GH-65", Stage: "execute", Kind: kind, Current: state, DecisionIdentity: decision,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestVerificationSharedAutomaticExplicitAllowanceAndCap(t *testing.T) {
	state := verificationState("tree", "config", "environment", "decision")
	store := &verificationStore{stored: verificationContext(failure.RetryNow, state)}
	gate := retry.MustNewGate(store, verificationPolicy(2, 1))

	automatic := authorizeVerification(t, gate, retry.KindAutomatic, state, "")
	explicit := authorizeVerification(t, gate, retry.KindExplicit, state, "")
	exhausted := authorizeVerification(t, gate, retry.KindExplicit, state, "")
	if automatic.Authorization == nil || automatic.Authorization.SharedUsed != 1 ||
		explicit.Authorization == nil || explicit.Authorization.SharedUsed != 2 ||
		exhausted.Rejection == nil || exhausted.Rejection.Reason != retry.ReasonCapExhausted ||
		store.stored.SharedUsed != 2 {
		t.Fatalf("shared allowance: automatic=%+v explicit=%+v exhausted=%+v context=%+v",
			automatic, explicit, exhausted, store.stored)
	}
}

func TestVerificationStateChangeUnlocksWithoutReset(t *testing.T) {
	initial := verificationState("tree-1", "config-1", "environment-1", "decision-1")
	store := &verificationStore{stored: verificationContext(failure.RetryAfterStateChange, initial)}
	gate := retry.MustNewGate(store, verificationPolicy(2, 1))

	unchanged := authorizeVerification(t, gate, retry.KindExplicit, initial, "")
	treeChanged := initial
	treeChanged.TreeDigest = verificationDigest("tree-2")
	first := authorizeVerification(t, gate, retry.KindExplicit, treeChanged, "")
	configChanged := treeChanged
	configChanged.ConfigDigest = verificationDigest("config-2")
	second := authorizeVerification(t, gate, retry.KindExplicit, configChanged, "")
	environmentChanged := configChanged
	environmentChanged.EnvironmentDigest = verificationDigest("environment-2")
	exhausted := authorizeVerification(t, gate, retry.KindExplicit, environmentChanged, "")

	if unchanged.Rejection == nil || unchanged.Rejection.Reason != retry.ReasonStateUnchanged ||
		first.Authorization == nil || first.Authorization.SharedUsed != 1 ||
		second.Authorization == nil || second.Authorization.SharedUsed != 2 ||
		exhausted.Rejection == nil || exhausted.Rejection.Reason != retry.ReasonCapExhausted ||
		store.stored.SharedUsed != 2 {
		t.Fatalf("state-change accounting: unchanged=%+v first=%+v second=%+v exhausted=%+v context=%+v",
			unchanged, first, second, exhausted, store.stored)
	}
}

func TestVerificationModelResampleIsExplicitOnlyAndCapped(t *testing.T) {
	initial := verificationState("tree", "config", "environment", "decision-1")
	store := &verificationStore{stored: verificationContext(failure.RetryAfterStateChange, initial)}
	gate := retry.MustNewGate(store, verificationPolicy(3, 1))

	missingDecision := authorizeVerification(t, gate, retry.KindModelResample, initial, "")
	changed := initial
	changed.DecisionDigest = verificationDigest("decision-2")
	selected := verificationDigest("sample-2")
	accepted := authorizeVerification(t, gate, retry.KindModelResample, changed, selected)
	again := changed
	again.DecisionDigest = verificationDigest("decision-3")
	reused := authorizeVerification(t, gate, retry.KindModelResample, again, selected)
	newer := again
	newer.DecisionDigest = verificationDigest("decision-4")
	exhausted := authorizeVerification(t, gate, retry.KindModelResample, newer, verificationDigest("sample-3"))

	if missingDecision.Rejection == nil || missingDecision.Rejection.Reason != retry.ReasonKindNotAllowed ||
		accepted.Authorization == nil || accepted.Authorization.ModelResampleUsed != 1 ||
		accepted.Authorization.DecisionIdentity != selected || store.stored.DecisionIdentity != selected ||
		reused.Rejection == nil || reused.Rejection.Reason != retry.ReasonKindNotAllowed ||
		exhausted.Rejection == nil || exhausted.Rejection.Reason != retry.ReasonModelResampleExhausted ||
		store.stored.SharedUsed != 1 || store.stored.ModelResampleUsed != 1 {
		t.Fatalf("model-resample binding: missing=%+v accepted=%+v reused=%+v exhausted=%+v context=%+v",
			missingDecision, accepted, reused, exhausted, store.stored)
	}
}

func TestVerificationMissingEvidenceAndOneConflictFailClosedOrRecoverOnce(t *testing.T) {
	state := verificationState("tree", "config", "environment", "decision")
	missing := state
	missing.EnvironmentDigest = failure.Unavailable
	missingStore := &verificationStore{stored: verificationContext(failure.RetryNow, state)}
	missingGate := retry.MustNewGate(missingStore, verificationPolicy(1, 1))
	missingResult := authorizeVerification(t, missingGate, retry.KindExplicit, missing, "")
	if missingResult.Rejection == nil || missingResult.Rejection.Reason != retry.ReasonFingerprintUnavailable ||
		missingStore.stored.SharedUsed != 0 {
		t.Fatalf("missing evidence result=%+v context=%+v", missingResult, missingStore.stored)
	}

	conflictStore := &verificationStore{stored: verificationContext(failure.RetryNow, state), conflicts: 1}
	conflictGate := retry.MustNewGate(conflictStore, verificationPolicy(1, 1))
	recovered := authorizeVerification(t, conflictGate, retry.KindAutomatic, state, "")
	if recovered.Authorization == nil || recovered.Authorization.SharedUsed != 1 || conflictStore.conflicts != 0 {
		t.Fatalf("single-conflict result=%+v context=%+v conflicts=%d", recovered, conflictStore.stored, conflictStore.conflicts)
	}

	twoConflictStore := &verificationStore{stored: verificationContext(failure.RetryNow, state), conflicts: 2}
	twoConflictGate := retry.MustNewGate(twoConflictStore, verificationPolicy(1, 1))
	rejected := authorizeVerification(t, twoConflictGate, retry.KindAutomatic, state, "")
	if rejected.Rejection == nil || rejected.Rejection.Reason != retry.ReasonPersistenceConflict ||
		twoConflictStore.stored.SharedUsed != 0 {
		t.Fatalf("two-conflict result=%+v context=%+v", rejected, twoConflictStore.stored)
	}
}

func TestVerificationStructuredOutcomesContainOnlySanitizedEvidence(t *testing.T) {
	state := verificationState("tree", "config", "environment", "decision")
	store := &verificationStore{stored: verificationContext(failure.RetryNow, state)}
	gate := retry.MustNewGate(store, verificationPolicy(1, 1))
	authorized := authorizeVerification(t, gate, retry.KindAutomatic, state, "")
	rejected := authorizeVerification(t, gate, retry.KindExplicit, state, "")

	encoded, err := json.Marshal([]retry.Decision{authorized, rejected})
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, raw := range []string{"SECRET_API_TOKEN", "/private/repo", "exception text", "artifact bytes", "decision text", "model output"} {
		if strings.Contains(text, raw) {
			t.Fatalf("structured retry outcome leaked %q: %s", raw, text)
		}
	}
	if authorized.Authorization == nil || rejected.Rejection == nil ||
		!strings.Contains(text, `"failure_fingerprint":"sha256:`) ||
		!strings.Contains(text, `"reason":"retry_cap_exhausted"`) {
		t.Fatalf("structured evidence missing from %s", text)
	}
}

var _ retry.ContextStore = (*verificationStore)(nil)
