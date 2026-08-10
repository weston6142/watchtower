package engine

import (
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

func TestRehydrateProjectsBranchOnlyLegacyMerges(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	parents := []string{"GH-61", "GH-62", "GH-63", "GH-64"}
	landed := make(map[string]string, len(parents))
	for _, parent := range parents {
		landed[parent] = createCanonicalLegacyMerge(t, repo, parent)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "guildhall.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, parent := range parents {
		if err := s.UpsertIssue(store.IssueRow{ID: parent, State: "done", Flow: "default"}); err != nil {
			t.Fatal(err)
		}
		child := parent + "-child"
		if err := s.UpsertIssue(store.IssueRow{ID: child, State: "backlog", Flow: "default"}); err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceDependencies(child, []string{parent}); err != nil {
			t.Fatal(err)
		}
		appendLegacyEvents(t, s, parent,
			legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{
				"branch": "issue/" + parent,
			}},
			legacyEventSpec{typ: core.EvIssueCompleted, payload: nil},
		)
	}

	e := New(Config{
		Store: s, Runner: &runner.FakeRunner{}, Pool: slots.NewPool(2),
		Flows: map[string]flow.Flow{"default": testFlow()}, DataDir: t.TempDir(),
		Train: &marshal.Train{Repo: repo}, DecisionIdentities: testDecisionIdentities(),
	})
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	for _, parent := range parents {
		integration, ok, err := s.IssueIntegration(parent)
		if err != nil || !ok || integration.State != store.IntegrationMerged ||
			integration.BaseBranch != "main" || integration.LandedSHA != landed[parent] {
			t.Fatalf("integration %s = %+v ok=%v err=%v", parent, integration, ok, err)
		}
		assertLegacyClaimBlockers(t, e, parent+"-child", nil)
	}

	eventCount := s.LatestEventSeq()
	childRunsBefore := make(map[string]int, len(parents))
	for _, parent := range parents {
		runs, err := s.StageRuns(parent + "-child")
		if err != nil {
			t.Fatal(err)
		}
		childRunsBefore[parent] = len(runs)
	}
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if s.LatestEventSeq() != eventCount {
		t.Fatalf("event count changed after repeated branch-only rehydrate: %d -> %d", eventCount, s.LatestEventSeq())
	}
	for _, parent := range parents {
		runs, err := s.StageRuns(parent + "-child")
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != childRunsBefore[parent] {
			t.Fatalf("child stage runs for %s changed after repeat: %d -> %d", parent, childRunsBefore[parent], len(runs))
		}
	}
}

func TestRehydratePrefersLandedSHAOverBranchOnlyMergeEvidence(t *testing.T) {
	e, s, _, landedSHA := newLegacyFailureEngine(t, []legacyFailureCandidate{
		{id: "GH-61", state: "done"},
	})
	appendLegacyEvents(t, s, "GH-61",
		legacyEventSpec{typ: core.EvPublishSucceeded, payload: map[string]any{
			"branch": "main", "commit": landedSHA,
		}},
		legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{
			"branch": "issue/GH-61",
		}},
		legacyEventSpec{typ: core.EvIssueCompleted, payload: nil},
	)

	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	integration, ok, err := s.IssueIntegration("GH-61")
	if err != nil || !ok || integration.State != store.IntegrationMerged ||
		integration.BaseBranch != "main" || integration.LandedSHA != landedSHA {
		t.Fatalf("integration = %+v ok=%v err=%v", integration, ok, err)
	}
	assertLegacyClaimBlockers(t, e, "GH-61-child", nil)
}

func TestRehydrateProjectsLegacyMergesAfterRestart(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	landedSHA := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "main"))

	s, err := store.Open(filepath.Join(t.TempDir(), "guildhall.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	parents := []string{"GH-61", "GH-62", "GH-63", "GH-64"}
	for _, parent := range parents {
		if err := s.UpsertIssue(store.IssueRow{ID: parent, State: "done", Flow: "default"}); err != nil {
			t.Fatal(err)
		}
		child := parent + "-child"
		if err := s.UpsertIssue(store.IssueRow{ID: child, State: "backlog", Flow: "default"}); err != nil {
			t.Fatal(err)
		}
		if err := s.ReplaceDependencies(child, []string{parent}); err != nil {
			t.Fatal(err)
		}
	}

	appendLegacyEvents(t, s, "GH-61",
		legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{
			"branch": "main", "commit": landedSHA,
		}},
		legacyEventSpec{typ: core.EvIssueCompleted, payload: nil},
	)
	appendLegacyEvents(t, s, "GH-62",
		legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{
			"branch": "issue/GH-62", "landed_sha": landedSHA,
		}},
		legacyEventSpec{typ: core.EvPublishSucceeded, payload: map[string]any{
			"branch": "main", "commit": landedSHA,
		}},
		legacyEventSpec{typ: core.EvIssueCompleted, payload: nil},
	)
	appendLegacyEvents(t, s, "GH-63",
		legacyEventSpec{typ: core.EvMergeConflict, payload: map[string]any{
			"base_branch": "main",
		}},
		legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{
			"base_branch": "main", "commit": landedSHA,
		}},
		legacyEventSpec{typ: core.EvIssueCompleted, payload: nil},
	)
	appendLegacyEvents(t, s, "GH-64",
		legacyEventSpec{typ: core.EvPublishSucceeded, payload: map[string]any{
			"branch": "main", "commit": landedSHA,
		}},
		legacyEventSpec{typ: core.EvIssueMerged, payload: map[string]any{
			"branch": "main", "commit": landedSHA,
		}},
		legacyEventSpec{typ: core.EvIssueCompleted, payload: nil},
	)

	newEngine := func() *Engine {
		return New(Config{
			Store: s, Runner: &runner.FakeRunner{}, Pool: slots.NewPool(2),
			Flows: map[string]flow.Flow{"default": testFlow()}, DataDir: t.TempDir(),
			Train: &marshal.Train{Repo: repo}, DecisionIdentities: testDecisionIdentities(),
		})
	}

	restarted := newEngine()
	if err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	for _, parent := range parents {
		integration, ok, err := s.IssueIntegration(parent)
		if err != nil || !ok || integration.State != store.IntegrationMerged ||
			integration.BaseBranch != "main" || integration.LandedSHA != landedSHA {
			t.Fatalf("integration %s = %+v ok=%v err=%v", parent, integration, ok, err)
		}
		blockers, err := restarted.ClaimBlockers(parent + "-child")
		if err != nil || len(blockers) != 0 {
			t.Fatalf("ClaimBlockers(%s) = %v, err=%v", parent+"-child", blockers, err)
		}
	}
	eventCount := s.LatestEventSeq()
	childRunsBefore, err := s.StageRuns(parents[0] + "-child")
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if s.LatestEventSeq() != eventCount {
		t.Fatalf("event count changed after repeated rehydrate: %d -> %d", eventCount, s.LatestEventSeq())
	}
	childRunsAfter, err := s.StageRuns(parents[0] + "-child")
	if err != nil {
		t.Fatal(err)
	}
	if len(childRunsAfter) != len(childRunsBefore) {
		t.Fatalf("child stage runs changed after repeated rehydrate: %d -> %d", len(childRunsBefore), len(childRunsAfter))
	}
}

func createCanonicalLegacyMerge(t *testing.T, repo, issueID string) string {
	t.Helper()
	return createCanonicalLegacyMergeWithSubject(t, repo, issueID, canonicalLegacyMergeSubject(issueID, "main"), "main")
}

func appendLegacyEvents(t *testing.T, s *store.Store, issueID string, specs ...legacyEventSpec) {
	t.Helper()
	for _, spec := range specs {
		var event core.Event
		if spec.raw != nil {
			event = core.Event{Type: spec.typ, IssueID: issueID, Payload: spec.raw}
		} else {
			var err error
			event, err = core.NewEvent(spec.typ, issueID, spec.payload)
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Append(event); err != nil {
			t.Fatal(err)
		}
	}
}
