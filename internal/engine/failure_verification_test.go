package engine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/store"
)

func TestFailureSiteMatrixProducesCanonicalRecords(t *testing.T) {
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
			s, err := store.Open("file:matrix-" + tc.name + "?mode=memory&cache=shared")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			e := New(Config{Store: s})
			primary := errors.New(tc.name + " primary failure with raw path /private/secret")
			if err := e.recordBoundaryFailure(context.Background(), "GH-63", "execute", 1,
				tc.site, tc.class, failure.RetryAfterStateChange, tc.state, primary); !errors.Is(err, primary) {
				t.Fatalf("returned error = %v", err)
			}
			records, err := s.FailureHistory(context.Background(), "GH-63")
			if err != nil || len(records) != 1 {
				t.Fatalf("records = %+v, err = %v", records, err)
			}
			record := records[0]
			if record.FailureSite != tc.site || record.FailureClass != tc.class || record.RequiredStateChange != tc.state ||
				record.RecordID <= 0 || record.SchemaVersion != failure.SchemaVersion {
				t.Fatalf("canonical record = %+v", record)
			}
		})
	}
}

func TestFailureFingerprintDomainsChangeIndependently(t *testing.T) {
	base := failure.FingerprintInputs{
		IssueID: "GH-63", Stage: "execute", FailureSite: failure.SiteRunner,
		StageInputs:        []failure.InputIdentity{{Identity: "spec.md", SHA256: "spec"}},
		WatchtowerIdentity: "watchtower", ConfigurationIdentity: "config",
		Git:          failure.GitIdentity{Repository: "repo", BaseCommit: "base", BranchCommit: "branch", TreeIdentity: "tree"},
		Artifacts:    []failure.ContentIdentity{{Identity: "artifact", SHA256: "artifact"}},
		Decisions:    []failure.ContentIdentity{{Identity: "decision", SHA256: "decision"}},
		Verification: failure.VerificationIdentity{CommandIdentity: "command", CacheIdentity: "cache"},
	}
	wantDifferent := []struct {
		name   string
		mutate func(*failure.FingerprintInputs)
	}{
		{"issue", func(v *failure.FingerprintInputs) { v.IssueID = "GH-64" }},
		{"stage", func(v *failure.FingerprintInputs) { v.Stage = "verify" }},
		{"site", func(v *failure.FingerprintInputs) { v.FailureSite = failure.SiteGit }},
		{"stage-input", func(v *failure.FingerprintInputs) { v.StageInputs[0].SHA256 = "other" }},
		{"watchtower", func(v *failure.FingerprintInputs) { v.WatchtowerIdentity = "other" }},
		{"configuration", func(v *failure.FingerprintInputs) { v.ConfigurationIdentity = "other" }},
		{"git", func(v *failure.FingerprintInputs) { v.Git.TreeIdentity = "other" }},
		{"artifact", func(v *failure.FingerprintInputs) { v.Artifacts[0].SHA256 = "other" }},
		{"decision", func(v *failure.FingerprintInputs) { v.Decisions[0].SHA256 = "other" }},
		{"verification", func(v *failure.FingerprintInputs) { v.Verification.CacheIdentity = "other" }},
	}
	original := failure.BuildFingerprint(base)
	for _, tc := range wantDifferent {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			changed.StageInputs = append([]failure.InputIdentity(nil), base.StageInputs...)
			changed.Artifacts = append([]failure.ContentIdentity(nil), base.Artifacts...)
			changed.Decisions = append([]failure.ContentIdentity(nil), base.Decisions...)
			tc.mutate(&changed)
			if got := failure.BuildFingerprint(changed); got == original {
				t.Fatalf("changed %s did not change fingerprint %q", tc.name, got)
			}
		})
	}
	withAttempt := base
	if first, second := failure.BuildFingerprint(base), failure.BuildFingerprint(withAttempt); first != second {
		t.Fatalf("attempt-independent fingerprint changed: %q != %q", first, second)
	}
}

func TestFailurePrivacyAcrossSurfaces(t *testing.T) {
	s, err := store.Open("file:privacy-surface?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := New(Config{Store: s})
	primary := errors.New("raw error /private/repo command --secret configuration body artifact bytes decision text")
	if err := e.recordBoundaryFailure(context.Background(), "GH-63", "execute", 1,
		failure.SiteVerification, failure.ClassValidation, failure.RetryNow, failure.StateVerification, primary); !errors.Is(err, primary) {
		t.Fatalf("returned error = %v", err)
	}
	records, err := s.FailureHistory(context.Background(), "GH-63")
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v, err = %v", records, err)
	}
	events, err := s.EventsSince(0)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v, err = %v", events, err)
	}
	encodedRecord, _ := json.Marshal(records[0])
	for _, raw := range []string{primary.Error(), "/private/repo", "configuration body", "artifact bytes", "decision text"} {
		if strings.Contains(string(encodedRecord), raw) || strings.Contains(string(events[0].Payload), raw) {
			t.Fatalf("raw failure value %q leaked", raw)
		}
	}
	var eventRecord failure.FailureRecord
	if err := json.Unmarshal(events[0].Payload, &eventRecord); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(eventRecord, records[0]) {
		t.Fatalf("event record = %+v, durable record = %+v", eventRecord, records[0])
	}
}

func TestFailurePersistenceAndEventDegradationPreservePrimary(t *testing.T) {
	primary := errors.New("primary failure")
	s, err := store.Open("file:failure-degradation?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := New(Config{Store: s})
	s.FailNextFailureAppendForTest()
	if err := e.recordBoundaryFailure(context.Background(), "GH-63", "execute", 1,
		failure.SiteStore, failure.ClassUnavailable, failure.RetryNow, failure.StateStore, primary); !errors.Is(err, primary) {
		t.Fatalf("persistence fault replaced primary: %v", err)
	}
	records, err := s.FailureHistory(context.Background(), "GH-63")
	if err != nil || len(records) != 0 {
		t.Fatalf("phantom record after persistence fault = %+v, err=%v", records, err)
	}
	s.FailNextAppendForTest(core.EvFailureRecorded)
	if err := e.recordBoundaryFailure(context.Background(), "GH-63", "execute", 1,
		failure.SiteVerification, failure.ClassValidation, failure.RetryNow, failure.StateVerification, primary); !errors.Is(err, primary) {
		t.Fatalf("event fault replaced primary: %v", err)
	}
	records, err = s.FailureHistory(context.Background(), "GH-63")
	if err != nil || len(records) != 1 {
		t.Fatalf("durable row lost after event fault = %+v, err=%v", records, err)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == core.EvFailureRecorded {
			t.Fatal("event was persisted despite injected event failure")
		}
	}
}

func TestFailureRetryCorrelationAcrossSurfaces(t *testing.T) {
	s, err := store.Open("file:failure-retry-correlation?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := New(Config{Store: s})
	primary := errors.New("retry failure")
	ctx := failureContext{
		IssueID: "GH-63", Stage: "execute", StageAttempt: 1, Site: failure.SiteVerification,
		Class: failure.ClassValidation, Disposition: failure.RetryAfterStateChange,
		StateChange: failure.StateVerification, FingerprintInputs: failure.FingerprintInputs{
			IssueID: "GH-63", Stage: "execute", FailureSite: failure.SiteVerification,
		}, Primary: primary,
	}
	if err := e.recordFailure(context.Background(), ctx); !errors.Is(err, primary) {
		t.Fatal(err)
	}
	records, err := s.FailureHistory(context.Background(), "GH-63")
	if err != nil || len(records) != 1 {
		t.Fatalf("initial records = %+v, err=%v", records, err)
	}
	correlation, err := e.CorrelateFailure(context.Background(), "GH-63", "execute", failure.SiteVerification, records[0].Fingerprint)
	if err != nil || !correlation.Found || !correlation.FingerprintMatches || correlation.RequiredStateChange != failure.StateVerification {
		t.Fatalf("correlation = %+v, err=%v", correlation, err)
	}
	if err := e.recordFailure(context.Background(), ctx); !errors.Is(err, primary) {
		t.Fatal(err)
	}
	records, err = s.FailureHistory(context.Background(), "GH-63")
	if err != nil || len(records) != 2 || records[0].RecordID == records[1].RecordID || records[0].Fingerprint != records[1].Fingerprint {
		t.Fatalf("retry history = %+v, err=%v", records, err)
	}
}

func TestFailureRestartDurability(t *testing.T) {
	database := filepath.Join(t.TempDir(), "failures.db")
	s, err := store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	input := failure.RecordInput{
		IssueID: "GH-63", Stage: "execute", StageAttempt: 2, FailureSite: failure.SiteGit,
		FailureClass: failure.ClassIntegrity, RetryDisposition: failure.RetryAfterStateChange,
		RequiredStateChange: failure.StateGit, Fingerprint: "sha256:" + strings.Repeat("c", 64),
	}
	want, err := s.AppendFailure(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.FailureHistory(context.Background(), "GH-63")
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("reopened failure history = %+v, want %+v, err=%v", got, want, err)
	}
}

func TestFailureCompatibility(t *testing.T) {
	s, err := store.Open("file:failure-compatibility?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := failure.RecordInput{
		IssueID: "GH-63", Stage: "execute", StageAttempt: 1, FailureSite: failure.SiteCache,
		FailureClass: failure.ClassUnavailable, RetryDisposition: failure.RetryNow,
		RequiredStateChange: failure.StateCache, Fingerprint: "sha256:" + strings.Repeat("d", 64),
	}
	const count = 8
	results := make(chan failure.FailureRecord, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record, appendErr := s.AppendFailure(context.Background(), base)
			if appendErr != nil {
				t.Errorf("append failure: %v", appendErr)
				return
			}
			results <- record
		}()
	}
	wg.Wait()
	close(results)
	ids := make([]int64, 0, count)
	for record := range results {
		ids = append(ids, record.RecordID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) != count {
		t.Fatalf("concurrent record count = %d, want %d", len(ids), count)
	}
	for i, id := range ids {
		if id <= 0 || (i > 0 && id == ids[i-1]) {
			t.Fatalf("concurrent record IDs = %v", ids)
		}
	}
	stageFailed, err := core.NewEvent(core.EvStageFailed, "GH-63", map[string]string{"error": "legacy failure"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(stageFailed); err != nil {
		t.Fatal(err)
	}
	finalizationFailed, err := core.NewEvent(core.EvFinalizationFailed, "GH-63", map[string]string{"error": "legacy finalization failure"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(finalizationFailed); err != nil {
		t.Fatal(err)
	}
	records, err := s.FailureHistory(context.Background(), "GH-63")
	if err != nil || len(records) != count {
		t.Fatalf("compatibility history = %d, err=%v", len(records), err)
	}
	if records[0].OccurredAt.After(time.Now().UTC()) {
		t.Fatalf("future failure timestamp = %v", records[0].OccurredAt)
	}
}
