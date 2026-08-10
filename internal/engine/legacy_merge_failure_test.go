package engine

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/store"
)

type legacyFailureCandidate struct {
	id     string
	state  string
	events []legacyEventSpec
}

func TestRehydrateRejectsInvalidLegacyEvidenceIndependently(t *testing.T) {
	valid := "GH-70"
	candidates := []legacyFailureCandidate{
		{id: valid, state: "done"},
		{id: "GH-71", state: "done", events: []legacyEventSpec{
			{typ: core.EvIssueCompleted, payload: map[string]any{}},
		}},
		{id: "GH-72", state: "done", events: []legacyEventSpec{
			{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main", "commit": "missing-completion"}},
		}},
		{id: "GH-73", state: "done", events: []legacyEventSpec{
			{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main"}},
			{typ: core.EvIssueCompleted, payload: map[string]any{}},
		}},
		{id: "GH-74", state: "done", events: []legacyEventSpec{
			{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main", "commit": "not-a-commit"}},
			{typ: core.EvIssueCompleted, payload: map[string]any{}},
		}},
		{id: "GH-80", state: "done", events: []legacyEventSpec{
			{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main", "commit": "main"}},
			{typ: core.EvIssueCompleted, payload: map[string]any{}},
		}},
		{id: "GH-75", state: "done", events: []legacyEventSpec{
			{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main", "commit": "left-unmerged"}},
			{typ: core.EvIssueCompleted, payload: map[string]any{"merge": "left-unmerged"}},
		}},
		{id: "GH-76", state: "done", events: []legacyEventSpec{
			{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main", "commit": "commit-a"}},
			{typ: core.EvPublishSucceeded, payload: map[string]any{"branch": "main", "commit": "commit-b"}},
			{typ: core.EvIssueCompleted, payload: map[string]any{}},
		}},
		{id: "GH-77", state: "done", events: []legacyEventSpec{
			{typ: core.EvIssueMerged, raw: json.RawMessage(`{"commit":`)},
			{typ: core.EvIssueCompleted, payload: map[string]any{}},
		}},
		{id: "GH-78", state: "done", events: []legacyEventSpec{
			{typ: core.EvIssueMerged, payload: map[string]any{"base_branch": "main", "commit": "commit-a"}},
			{typ: core.EvPublishSucceeded, payload: map[string]any{"branch": "develop", "commit": "commit-a"}},
			{typ: core.EvIssueCompleted, payload: map[string]any{}},
		}},
		{id: "GH-79", state: "done (unmerged)", events: []legacyEventSpec{
			{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main", "commit": "commit-a"}},
			{typ: core.EvIssueCompleted, payload: map[string]any{}},
		}},
	}
	e, s, _, landedSHA := newLegacyFailureEngine(t, candidates)
	if err := appendLegacyFailureEvents(t, s, valid, []legacyEventSpec{
		{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main", "commit": landedSHA}},
		{typ: core.EvIssueCompleted, payload: map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}
	beforeEvents := s.LatestEventSeq()
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}

	integration, ok, err := s.IssueIntegration(valid)
	if err != nil || !ok || integration.State != store.IntegrationMerged || integration.LandedSHA != landedSHA {
		t.Fatalf("valid integration = %+v ok=%v err=%v", integration, ok, err)
	}
	for _, candidate := range candidates {
		if candidate.id == valid {
			continue
		}
		if integration, ok, err := s.IssueIntegration(candidate.id); err != nil || ok {
			t.Fatalf("invalid candidate %s integration = %+v ok=%v err=%v", candidate.id, integration, ok, err)
		}
		blockers, err := e.ClaimBlockers(candidate.id + "-child")
		if err != nil || len(blockers) != 1 || blockers[0] != candidate.id {
			t.Fatalf("invalid child %s blockers = %v err=%v", candidate.id, blockers, err)
		}
	}
	if s.LatestEventSeq() != beforeEvents {
		t.Fatalf("reconciliation appended lifecycle evidence: before=%d after=%d", beforeEvents, s.LatestEventSeq())
	}
}

func TestRehydratePersistenceFailureIsFatalAndRetryable(t *testing.T) {
	e, s, _, landedSHA := newLegacyFailureEngine(t, []legacyFailureCandidate{
		{id: "GH-80", state: "done"},
		{id: "GH-81", state: "done"},
	})
	// The temporary repository's real main commit is the only reachable landing.
	for _, id := range []string{"GH-80", "GH-81"} {
		if err := appendLegacyFailureEvents(t, s, id, validLegacyFailureEvents(landedSHA)); err != nil {
			t.Fatal(err)
		}
	}
	s.FailIssueIntegrationWriteAfterForTest(1)
	if err := e.Rehydrate(); err == nil || !strings.Contains(err.Error(), "integration persistence") {
		t.Fatalf("Rehydrate persistence failure = %v", err)
	}
	if integration, ok, err := s.IssueIntegration("GH-80"); err != nil || !ok || integration.State != store.IntegrationMerged {
		t.Fatalf("first candidate after failed batch = %+v ok=%v err=%v", integration, ok, err)
	}
	if _, ok, err := s.IssueIntegration("GH-81"); err != nil || ok {
		t.Fatalf("second candidate unexpectedly persisted after failed batch: ok=%v err=%v", ok, err)
	}
	if blockers, err := e.ClaimBlockers("GH-81-child"); err != nil || len(blockers) != 1 || blockers[0] != "GH-81" {
		t.Fatalf("second candidate blockers after failed batch = %v err=%v", blockers, err)
	}
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if integration, ok, err := s.IssueIntegration("GH-81"); err != nil || !ok || integration.State != store.IntegrationMerged || integration.LandedSHA != landedSHA {
		t.Fatalf("second candidate after retry = %+v ok=%v err=%v", integration, ok, err)
	}
	if blockers, err := e.ClaimBlockers("GH-81-child"); err != nil || len(blockers) != 0 {
		t.Fatalf("second candidate blockers after retry = %v err=%v", blockers, err)
	}
}

func TestLegacyEvidenceCannotSatisfyStrictClaims(t *testing.T) {
	parents := []legacyFailureCandidate{
		{id: "GH-90", state: "done", events: []legacyEventSpec{
			{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main", "commit": "not-a-commit"}},
			{typ: core.EvIssueCompleted, payload: map[string]any{}},
		}},
		{id: "GH-91", state: "done"},
		{id: "GH-92", state: "done"},
		{id: "GH-93", state: "done"},
		{id: "GH-94", state: "done"},
	}
	e, s, _, _ := newLegacyFailureEngine(t, parents)
	child := "GH-claim-child"
	if err := s.UpsertIssue(store.IssueRow{ID: child, State: "backlog", Flow: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceDependencies(child, []string{"GH-90", "GH-91", "GH-92", "GH-93", "GH-94"}); err != nil {
		t.Fatal(err)
	}
	for id, state := range map[string]string{
		"GH-91": store.IntegrationPreserved,
		"GH-92": "unknown",
		"GH-93": store.IntegrationCleanupNeeded,
	} {
		if err := s.SetIssueIntegration(store.IssueIntegration{IssueID: id, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	wantBlocked := []string{"GH-90", "GH-91", "GH-92", "GH-94"}
	assertLegacyClaimBlockers(t, e, child, wantBlocked)
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	assertLegacyClaimBlockers(t, e, child, wantBlocked)

	for _, id := range wantBlocked {
		if err := s.SetIssueIntegration(store.IssueIntegration{
			IssueID: id, State: store.IntegrationMerged,
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertLegacyClaimBlockers(t, e, child, nil)
}

func validLegacyFailureEvents(commit string) []legacyEventSpec {
	return []legacyEventSpec{
		{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main", "commit": commit}},
		{typ: core.EvIssueCompleted, payload: map[string]any{}},
	}
}

func newLegacyFailureEngine(t *testing.T, candidates []legacyFailureCandidate) (*Engine, *store.Store, string, string) {
	t.Helper()
	repo := t.TempDir()
	initGitRepo(t, repo)
	landedSHA := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "main"))
	s, err := store.Open(filepath.Join(t.TempDir(), "guildhall.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, candidate := range candidates {
		if err := s.UpsertIssue(store.IssueRow{ID: candidate.id, State: candidate.state, Flow: "default"}); err != nil {
			t.Fatal(err)
		}
		child := candidate.id + "-child"
		if err := s.UpsertIssue(store.IssueRow{ID: child, State: "backlog", Flow: "default"}); err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceDependencies(child, []string{candidate.id}); err != nil {
			t.Fatal(err)
		}
		if err := appendLegacyFailureEvents(t, s, candidate.id, candidate.events); err != nil {
			t.Fatal(err)
		}
	}
	e := New(Config{
		Store: s, Runner: &runner.FakeRunner{}, Pool: slots.NewPool(2),
		Flows: map[string]flow.Flow{"default": testFlow()}, DataDir: t.TempDir(),
		Train: &marshal.Train{Repo: repo}, DecisionIdentities: testDecisionIdentities(),
	})
	return e, s, repo, landedSHA
}

func appendLegacyFailureEvents(t *testing.T, s *store.Store, issueID string, specs []legacyEventSpec) error {
	t.Helper()
	for _, spec := range specs {
		var event core.Event
		var err error
		if spec.raw != nil {
			event = core.Event{Type: spec.typ, IssueID: issueID, Payload: spec.raw}
		} else {
			event, err = core.NewEvent(spec.typ, issueID, spec.payload)
			if err != nil {
				return err
			}
		}
		if _, err := s.Append(event); err != nil {
			return err
		}
	}
	return nil
}

func assertLegacyClaimBlockers(t *testing.T, e *Engine, child string, want []string) {
	t.Helper()
	got, err := e.ClaimBlockers(child)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ClaimBlockers(%s) = %v, want %v", child, got, want)
	}
}
