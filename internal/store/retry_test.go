package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/retry"
)

func retryTestDigest(value string) string {
	return "sha256:" + strings.Repeat(value, 64)[:64]
}

func retryTestVector(tree string) retry.StateVector {
	return retry.StateVector{
		TreeDigest: retryTestDigest(tree), ConfigDigest: retryTestDigest("b"),
		EnvironmentDigest: retryTestDigest("c"), DecisionDigest: retryTestDigest("d"),
	}
}

func appendRetryFailure(t *testing.T, s *Store, fingerprint string, vector retry.StateVector) {
	t.Helper()
	_, err := s.AppendFailure(context.Background(), failure.RecordInput{
		IssueID: "GH-65", Stage: "execute", StageAttempt: 1,
		FailureSite: failure.SiteVerification, FailureClass: failure.ClassExecution,
		RetryDisposition: failure.RetryNow, RequiredStateChange: failure.StateVerification,
		Fingerprint: fingerprint, StateVector: &vector,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func retryStorePolicy(limit int) retry.Policy {
	return retry.Policy{
		ID: "test", Version: "1", TransientLimit: limit, DeterministicLimit: 1,
		ModelResampleLimit: 1, ModelResampleClasses: []failure.Class{failure.ClassExecution},
	}
}

func TestRetryContextPersistsAuthorizationAcrossReopen(t *testing.T) {
	database := filepath.Join(t.TempDir(), "retry.db")
	s, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := retryTestDigest("f")
	prior := retryTestVector("a")
	appendRetryFailure(t, s, fingerprint, prior)
	gate, err := retry.NewGate(s, retryStorePolicy(1))
	if err != nil {
		t.Fatal(err)
	}
	current := prior
	current.TreeDigest = retryTestDigest("e")
	result, err := gate.Authorize(context.Background(), retry.Request{
		IssueID: "GH-65", Stage: "execute", Kind: retry.KindAutomatic, Current: current,
	})
	if err != nil || !result.Authorized {
		t.Fatalf("authorization = %+v, err=%v", result, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stored, err := s.LoadRetryContext(context.Background(), "GH-65", "execute")
	if err != nil || stored.SharedUsed != 1 || stored.Version != 2 || stored.State != current {
		t.Fatalf("reopened context = %+v, err=%v", stored, err)
	}
	result, err = retry.MustNewGate(s, retryStorePolicy(1)).Authorize(context.Background(), retry.Request{
		IssueID: "GH-65", Stage: "execute", Kind: retry.KindExplicit, Current: current,
	})
	if err != nil || result.Rejection == nil || result.Rejection.Reason != retry.ReasonCapExhausted {
		t.Fatalf("reopened authorization = %+v, err=%v", result, err)
	}
}

func TestRetryContextPreservesDuplicateOccurrencesInOneAggregate(t *testing.T) {
	s, err := Open("file:retry-duplicates?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fingerprint := retryTestDigest("f")
	vector := retryTestVector("a")
	appendRetryFailure(t, s, fingerprint, vector)
	appendRetryFailure(t, s, fingerprint, vector)
	records, err := s.FailureHistory(context.Background(), "GH-65")
	if err != nil || len(records) != 2 {
		t.Fatalf("failure history = %+v, err=%v", records, err)
	}
	stored, err := s.LoadRetryContext(context.Background(), "GH-65", "execute")
	if err != nil || stored.FailureFingerprint != fingerprint || stored.SharedUsed != 0 || stored.LatestRecordID != records[1].RecordID {
		t.Fatalf("aggregate context = %+v, err=%v", stored, err)
	}
}

func TestAuthorizeRetryConcurrentRequestsCannotExceedCap(t *testing.T) {
	s, err := Open("file:retry-concurrent?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	prior := retryTestVector("a")
	appendRetryFailure(t, s, retryTestDigest("f"), prior)
	gate := retry.MustNewGate(s, retryStorePolicy(2))
	current := prior
	current.TreeDigest = retryTestDigest("e")

	const callers = 12
	var wg sync.WaitGroup
	results := make(chan retry.Decision, callers)
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := gate.Authorize(context.Background(), retry.Request{
				IssueID: "GH-65", Stage: "execute", Kind: retry.KindAutomatic, Current: current,
			})
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	authorized := 0
	for result := range results {
		if result.Authorized {
			authorized++
		}
	}
	if authorized != 2 {
		t.Fatalf("authorized requests = %d, want 2", authorized)
	}
	stored, err := s.LoadRetryContext(context.Background(), "GH-65", "execute")
	if err != nil || stored.SharedUsed != 2 {
		t.Fatalf("stored usage = %+v, err=%v", stored, err)
	}
}

func TestAuthorizeRetryStoreConflictReloadsOnce(t *testing.T) {
	s, err := Open("file:retry-conflict?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	prior := retryTestVector("a")
	appendRetryFailure(t, s, retryTestDigest("f"), prior)
	current := prior
	current.TreeDigest = retryTestDigest("e")
	gate := retry.MustNewGate(s, retryStorePolicy(1))

	s.FailNextRetryAuthorizationsForTest(1)
	result, err := gate.Authorize(context.Background(), retry.Request{
		IssueID: "GH-65", Stage: "execute", Kind: retry.KindAutomatic, Current: current,
	})
	if err != nil || !result.Authorized {
		t.Fatalf("one-conflict result = %+v, err=%v", result, err)
	}

	appendRetryFailure(t, s, retryTestDigest("1"), prior)
	s.FailNextRetryAuthorizationsForTest(2)
	result, err = gate.Authorize(context.Background(), retry.Request{
		IssueID: "GH-65", Stage: "execute", Kind: retry.KindAutomatic, Current: current,
	})
	if err != nil || result.Rejection == nil || result.Rejection.Reason != retry.ReasonPersistenceConflict {
		t.Fatalf("two-conflict result = %+v, err=%v", result, err)
	}
}
