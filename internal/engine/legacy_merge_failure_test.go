package engine

import (
	"encoding/json"
	"os"
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

type branchOnlyHistoryCase struct {
	name    string
	issueID string
	setup   func(*testing.T, string)
	events  func(*testing.T, *store.Store, string, string)
}

func TestRehydrateRejectsBranchOnlyHistoryAndLifecycleEvidence(t *testing.T) {
	cases := []branchOnlyHistoryCase{
		{
			name:    "zero-canonical-candidates",
			issueID: "GH-61",
		},
		{
			name:    "multiple-canonical-candidates",
			issueID: "GH-61",
			setup: func(t *testing.T, repo string) {
				createCanonicalLegacyMerge(t, repo, "GH-61")
				gitOutput(t, repo, "checkout", "-q", "issue/GH-61")
				if err := os.WriteFile(filepath.Join(repo, "GH-61-second"), []byte("second\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitOutput(t, repo, "add", "GH-61-second")
				gitOutput(t, repo, "commit", "-qm", "second GH-61 change")
				gitOutput(t, repo, "checkout", "-q", "main")
				gitOutput(t, repo, "merge", "--no-ff", "-m", canonicalLegacyMergeSubject("GH-61", "main"), "issue/GH-61")
			},
		},
		{
			name:    "malformed-json",
			issueID: "GH-61",
			events: func(t *testing.T, s *store.Store, issueID, _ string) {
				appendLegacyEvents(t, s, issueID,
					legacyEventSpec{typ: core.EvIssueMerged, raw: json.RawMessage(`{"branch":`)},
					legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}},
				)
			},
		},
		{
			name:    "completion-before-merge",
			issueID: "GH-61",
			events: func(t *testing.T, s *store.Store, issueID, _ string) {
				appendLegacyEvents(t, s, issueID,
					legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}},
					legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{"branch": "issue/" + issueID}},
				)
			},
		},
		{
			name:    "duplicate-conflicting-merge-events",
			issueID: "GH-61",
			events: func(t *testing.T, s *store.Store, issueID, _ string) {
				appendLegacyEvents(t, s, issueID,
					legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{"branch": "issue/GH-61"}},
					legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{"branch": "issue/GH-62"}},
					legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}},
				)
			},
		},
		{
			name:    "left-unmerged",
			issueID: "GH-61",
			events: func(t *testing.T, s *store.Store, issueID, _ string) {
				appendLegacyEvents(t, s, issueID,
					legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{"branch": "issue/" + issueID}},
					legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{"merge": "left-unmerged"}},
				)
			},
		},
		{
			name:    "explicit-none",
			issueID: "GH-61",
			events: func(t *testing.T, s *store.Store, issueID, _ string) {
				appendLegacyEvents(t, s, issueID,
					legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{"branch": "issue/" + issueID}},
					legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{"merge": "none"}},
				)
			},
		},
		{
			name:    "branch-mismatch",
			issueID: "GH-62",
			events: func(t *testing.T, s *store.Store, issueID, _ string) {
				appendLegacyEvents(t, s, issueID,
					legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{"branch": "issue/GH-61"}},
					legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}},
				)
			},
		},
		{
			name:    "wrong-base-subject",
			issueID: "GH-61",
			setup: func(t *testing.T, repo string) {
				createCanonicalLegacyMergeWithSubject(t, repo, "GH-61", canonicalLegacyMergeSubject("GH-61", "develop"), "main")
			},
		},
		{
			name:    "spoofed-extra-subject-text",
			issueID: "GH-61",
			setup: func(t *testing.T, repo string) {
				createCanonicalLegacyMergeWithSubject(t, repo, "GH-61", canonicalLegacyMergeSubject("GH-61", "main")+" (recovered)", "main")
			},
		},
		{
			name:    "ordinary-one-parent",
			issueID: "GH-61",
			setup: func(t *testing.T, repo string) {
				createOneParentLegacySubject(t, repo, "GH-61", canonicalLegacyMergeSubject("GH-61", "main"))
			},
		},
		{
			name:    "squash-one-parent",
			issueID: "GH-61",
			setup: func(t *testing.T, repo string) {
				createSquashLegacySubject(t, repo, "GH-61", canonicalLegacyMergeSubject("GH-61", "main"))
			},
		},
		{
			name:    "rebase-shaped-one-parent",
			issueID: "GH-61",
			setup: func(t *testing.T, repo string) {
				createFastForwardLegacySubject(t, repo, "GH-61", canonicalLegacyMergeSubject("GH-61", "main"))
			},
		},
		{
			name:    "three-parent-commit",
			issueID: "GH-61",
			setup: func(t *testing.T, repo string) {
				createThreeParentLegacySubject(t, repo, "GH-61", canonicalLegacyMergeSubject("GH-61", "main"))
			},
		},
		{
			name:    "unreachable-canonical-merge",
			issueID: "GH-61",
			setup: func(t *testing.T, repo string) {
				createCanonicalLegacyMergeWithSubject(t, repo, "GH-61", canonicalLegacyMergeSubject("GH-61", "main"), "legacy-merge")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, s, repo, _ := newLegacyFailureEngine(t, []legacyFailureCandidate{
				{id: tc.issueID, state: "done"},
				{id: "GH-60", state: "done"},
			})
			if tc.setup != nil {
				tc.setup(t, repo)
			}
			if tc.events != nil {
				tc.events(t, s, tc.issueID, repo)
			} else {
				appendBranchOnlyLegacyEvents(t, s, tc.issueID)
			}
			independentSHA := createCanonicalLegacyMerge(t, repo, "GH-60")
			appendBranchOnlyLegacyEvents(t, s, "GH-60")

			if err := e.Rehydrate(); err != nil {
				t.Fatal(err)
			}
			if integration, ok, err := s.IssueIntegration(tc.issueID); err != nil || ok {
				t.Fatalf("rejected integration %s = %+v ok=%v err=%v", tc.issueID, integration, ok, err)
			}
			assertLegacyClaimBlockers(t, e, tc.issueID+"-child", []string{tc.issueID})
			integration, ok, err := s.IssueIntegration("GH-60")
			if err != nil || !ok || integration.LandedSHA != independentSHA {
				t.Fatalf("independent integration = %+v ok=%v err=%v", integration, ok, err)
			}
			assertLegacyClaimBlockers(t, e, "GH-60-child", nil)
		})
	}
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
	appendLegacyEvents(t, s, valid,
		legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main", "commit": landedSHA}},
		legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}},
	)
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
		appendLegacyEvents(t, s, id, validLegacyFailureEvents(landedSHA)...)
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

func TestRehydrateBranchOnlyPersistenceFailureIsFatalAndRetryable(t *testing.T) {
	ids := []string{"GH-80", "GH-81"}
	e, s, repo, _ := newLegacyFailureEngine(t, []legacyFailureCandidate{
		{id: ids[0], state: "done"},
		{id: ids[1], state: "done"},
	})
	landed := make(map[string]string, len(ids))
	for _, id := range ids {
		landed[id] = createCanonicalLegacyMerge(t, repo, id)
		appendBranchOnlyLegacyEvents(t, s, id)
	}
	beforeEvents := s.LatestEventSeq()
	s.FailIssueIntegrationWriteAfterForTest(1)
	if err := e.Rehydrate(); err == nil || !strings.Contains(err.Error(), "integration persistence") {
		t.Fatalf("Rehydrate branch-only persistence failure = %v", err)
	}
	first, ok, err := s.IssueIntegration(ids[0])
	if err != nil || !ok || first.State != store.IntegrationMerged || first.LandedSHA != landed[ids[0]] {
		t.Fatalf("first branch-only candidate after failed batch = %+v ok=%v err=%v", first, ok, err)
	}
	if second, ok, err := s.IssueIntegration(ids[1]); err != nil || ok {
		t.Fatalf("second branch-only candidate unexpectedly persisted after failed batch: %+v ok=%v err=%v", second, ok, err)
	}
	assertLegacyClaimBlockers(t, e, ids[1]+"-child", []string{ids[1]})
	if s.LatestEventSeq() != beforeEvents {
		t.Fatalf("branch-only persistence failure appended lifecycle evidence: before=%d after=%d", beforeEvents, s.LatestEventSeq())
	}
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	second, ok, err := s.IssueIntegration(ids[1])
	if err != nil || !ok || second.State != store.IntegrationMerged || second.LandedSHA != landed[ids[1]] {
		t.Fatalf("second branch-only candidate after retry = %+v ok=%v err=%v", second, ok, err)
	}
	assertLegacyClaimBlockers(t, e, ids[1]+"-child", nil)
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
		appendLegacyEvents(t, s, candidate.id, candidate.events...)
	}
	e := New(Config{
		Store: s, Runner: &runner.FakeRunner{}, Pool: slots.NewPool(2),
		Flows: map[string]flow.Flow{"default": testFlow()}, DataDir: t.TempDir(),
		Train: &marshal.Train{Repo: repo}, DecisionIdentities: testDecisionIdentities(),
	})
	return e, s, repo, landedSHA
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

func appendBranchOnlyLegacyEvents(t *testing.T, s *store.Store, issueID string) {
	t.Helper()
	appendLegacyEvents(t, s, issueID,
		legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{"branch": "issue/" + issueID}},
		legacyEventSpec{typ: core.EvIssueCompleted, payload: map[string]any{}},
	)
}

func canonicalLegacyMergeSubject(issueID, baseBranch string) string {
	return "Merge branch 'issue/" + issueID + "' into " + baseBranch
}

func createCanonicalLegacyMergeWithSubject(t *testing.T, repo, issueID, subject, target string) string {
	t.Helper()
	branch := "issue/" + issueID
	gitOutput(t, repo, "checkout", "-q", "-b", branch, "main")
	if err := os.WriteFile(filepath.Join(repo, issueID), []byte(issueID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "add", issueID)
	gitOutput(t, repo, "commit", "-qm", "change "+issueID)
	if target == "main" {
		gitOutput(t, repo, "checkout", "-q", "main")
	} else {
		gitOutput(t, repo, "checkout", "-q", "-b", target, "main")
	}
	gitOutput(t, repo, "merge", "--no-ff", "-m", subject, branch)
	landedSHA := strings.TrimSpace(gitOutput(t, repo, "rev-parse", target))
	if target != "main" {
		gitOutput(t, repo, "checkout", "-q", "main")
	}
	return landedSHA
}

func createOneParentLegacySubject(t *testing.T, repo, issueID, subject string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, issueID), []byte(issueID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "add", issueID)
	gitOutput(t, repo, "commit", "-qm", subject)
	return strings.TrimSpace(gitOutput(t, repo, "rev-parse", "main"))
}

func createSquashLegacySubject(t *testing.T, repo, issueID, subject string) string {
	t.Helper()
	branch := "issue/" + issueID
	gitOutput(t, repo, "checkout", "-q", "-b", branch, "main")
	if err := os.WriteFile(filepath.Join(repo, issueID), []byte(issueID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "add", issueID)
	gitOutput(t, repo, "commit", "-qm", "change "+issueID)
	gitOutput(t, repo, "checkout", "-q", "main")
	gitOutput(t, repo, "merge", "--squash", branch)
	gitOutput(t, repo, "commit", "-qm", subject)
	return strings.TrimSpace(gitOutput(t, repo, "rev-parse", "main"))
}

func createFastForwardLegacySubject(t *testing.T, repo, issueID, subject string) string {
	t.Helper()
	branch := "issue/" + issueID
	gitOutput(t, repo, "checkout", "-q", "-b", branch, "main")
	if err := os.WriteFile(filepath.Join(repo, issueID), []byte(issueID+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "add", issueID)
	gitOutput(t, repo, "commit", "-qm", subject)
	gitOutput(t, repo, "checkout", "-q", "main")
	gitOutput(t, repo, "merge", "--ff-only", branch)
	return strings.TrimSpace(gitOutput(t, repo, "rev-parse", "main"))
}

func createThreeParentLegacySubject(t *testing.T, repo, issueID, subject string) string {
	t.Helper()
	base := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "main"))
	gitOutput(t, repo, "checkout", "-q", "-b", "support-a", "main")
	if err := os.WriteFile(filepath.Join(repo, "support-a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "add", "support-a")
	gitOutput(t, repo, "commit", "-qm", "support a")
	parentA := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "support-a"))
	gitOutput(t, repo, "checkout", "-q", "main")
	gitOutput(t, repo, "checkout", "-q", "-b", "support-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "support-b"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "add", "support-b")
	gitOutput(t, repo, "commit", "-qm", "support b")
	parentB := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "support-b"))
	gitOutput(t, repo, "checkout", "-q", "main")
	tree := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "main^{tree}"))
	candidate := strings.TrimSpace(gitOutput(t, repo, "commit-tree", tree, "-p", base, "-p", parentA, "-p", parentB, "-m", subject))
	gitOutput(t, repo, "update-ref", "refs/heads/main", candidate)
	return candidate
}
