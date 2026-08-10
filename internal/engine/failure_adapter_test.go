package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/store"
)

type fakeFailureRecorder struct {
	records []failure.FailureRecord
	err     error
	nextID  int64
}

func (r *fakeFailureRecorder) AppendFailure(_ context.Context, input failure.RecordInput) (failure.FailureRecord, error) {
	if r.err != nil {
		return failure.FailureRecord{}, r.err
	}
	r.nextID++
	record := failure.FailureRecord{
		RecordID: r.nextID, SchemaVersion: failure.SchemaVersion,
		IssueID: input.IssueID, Stage: input.Stage, StageAttempt: input.StageAttempt,
		FailureSite: input.FailureSite, FailureClass: input.FailureClass,
		RetryDisposition: input.RetryDisposition, RequiredStateChange: input.RequiredStateChange,
		Fingerprint: input.Fingerprint, OccurredAt: time.Now().UTC(),
	}
	r.records = append(r.records, record)
	return record, nil
}

func (r *fakeFailureRecorder) FailureHistory(_ context.Context, issueID string) ([]failure.FailureRecord, error) {
	if r.err != nil {
		return nil, r.err
	}
	result := make([]failure.FailureRecord, 0)
	for _, record := range r.records {
		if record.IssueID == issueID {
			result = append(result, record)
		}
	}
	return result, nil
}

func testFailureContext(primary error) failureContext {
	return failureContext{
		IssueID: "GH-63", Stage: "plan", StageAttempt: 1,
		Site: failure.SiteRunner, Class: failure.ClassExecution,
		Disposition: failure.RetryNow, StateChange: failure.StateNone,
		FingerprintInputs: failure.FingerprintInputs{
			IssueID: "GH-63", Stage: "plan", FailureSite: failure.SiteRunner,
		},
		Primary: primary,
	}
}

func TestRecordFailureKeepsPrimaryErrorWhenRecorderFails(t *testing.T) {
	primary := errors.New("primary runner failure")
	recorder := &fakeFailureRecorder{err: errors.New("failure storage unavailable")}
	s, err := store.Open("file:adapter-error?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := New(Config{Store: s, FailureRecorder: recorder})
	got := e.recordFailure(context.Background(), testFailureContext(primary))
	if !errors.Is(got, primary) {
		t.Fatalf("returned error = %v, want original %v", got, primary)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == core.EvFailureRecorded {
			t.Fatal("failure event claims a non-persisted record")
		}
	}
}

func TestRecordFailureEmitsOnlyAfterPersistenceAndRetainsDuplicates(t *testing.T) {
	s, err := store.Open("file:adapter-success?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recorder := &fakeFailureRecorder{}
	e := New(Config{Store: s, FailureRecorder: recorder})
	primary := errors.New("primary failure")
	first := testFailureContext(primary)
	second := testFailureContext(primary)
	if err := e.recordFailure(context.Background(), first); !errors.Is(err, primary) {
		t.Fatalf("first returned error = %v", err)
	}
	if err := e.recordFailure(context.Background(), second); !errors.Is(err, primary) {
		t.Fatalf("second returned error = %v", err)
	}
	if len(recorder.records) != 2 || recorder.records[0].RecordID == recorder.records[1].RecordID {
		t.Fatalf("records = %+v", recorder.records)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == core.EvFailureRecorded {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("failure event count = %d, want 2; events = %+v", count, events)
	}
}

func TestFailureSiteAdapters(t *testing.T) {
	cases := []struct {
		name  string
		site  failure.Site
		class failure.Class
		state failure.StateChange
	}{
		{"runner", failure.SiteRunner, failure.ClassExecution, failure.StateRunnerInput},
		{"workspace", failure.SiteWorkspace, failure.ClassUnavailable, failure.StateWorkspace},
		{"artifact", failure.SiteArtifact, failure.ClassValidation, failure.StateArtifact},
		{"planner", failure.SitePlanner, failure.ClassTransport, failure.StatePlannerInput},
		{"git", failure.SiteGit, failure.ClassIntegrity, failure.StateGit},
		{"verification", failure.SiteVerification, failure.ClassValidation, failure.StateVerification},
		{"cache", failure.SiteCache, failure.ClassUnavailable, failure.StateCache},
		{"store", failure.SiteStore, failure.ClassUnavailable, failure.StateStore},
		{"finalization", failure.SiteFinalization, failure.ClassStateMismatch, failure.StateOperator},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.Open("file:adapter-" + tc.name + "?mode=memory&cache=shared")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			recorder := &fakeFailureRecorder{}
			e := New(Config{Store: s, FailureRecorder: recorder})
			ctx := testFailureContext(errors.New(tc.name + " failure"))
			ctx.Site, ctx.Class, ctx.StateChange = tc.site, tc.class, tc.state
			ctx.FingerprintInputs.FailureSite = tc.site
			if err := e.recordFailure(context.Background(), ctx); err == nil || !strings.Contains(err.Error(), tc.name+" failure") {
				t.Fatalf("record failure returned %v", err)
			}
			if len(recorder.records) != 1 || recorder.records[0].FailureSite != tc.site ||
				recorder.records[0].FailureClass != tc.class || recorder.records[0].RequiredStateChange != tc.state {
				t.Fatalf("record = %+v", recorder.records)
			}
		})
	}
}

func TestFailureRetryCorrelation(t *testing.T) {
	recorder := &fakeFailureRecorder{}
	e := New(Config{FailureRecorder: recorder})
	primary := errors.New("retryable failure")
	ctx := testFailureContext(primary)
	ctx.Disposition = failure.RetryAfterStateChange
	ctx.StateChange = failure.StateWorkspace
	if err := e.recordFailure(context.Background(), ctx); !errors.Is(err, primary) {
		t.Fatalf("record failure returned %v", err)
	}
	fingerprint := recorder.records[0].Fingerprint
	matching, err := e.CorrelateFailure(context.Background(), "GH-63", "plan", failure.SiteRunner, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !matching.Found || !matching.FingerprintMatches || matching.PriorFingerprint != fingerprint ||
		matching.RetryDisposition != failure.RetryAfterStateChange || matching.RequiredStateChange != failure.StateWorkspace {
		t.Fatalf("matching correlation = %+v", matching)
	}
	changed, err := e.CorrelateFailure(context.Background(), "GH-63", "plan", failure.SiteRunner, "sha256:"+strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if !changed.Found || changed.FingerprintMatches {
		t.Fatalf("changed correlation = %+v", changed)
	}
	if err := e.recordFailure(context.Background(), ctx); !errors.Is(err, primary) {
		t.Fatalf("retry returned %v", err)
	}
	if len(recorder.records) != 2 {
		t.Fatalf("retry records = %d, want 2", len(recorder.records))
	}
}
