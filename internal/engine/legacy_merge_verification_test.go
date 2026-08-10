package engine

import (
	"testing"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/store"
)

func TestLegacyMergeVerificationMatrix(t *testing.T) {
	candidates := []legacyFailureCandidate{
		{id: "GH-100", state: "done"},
		{id: "GH-101", state: "done"},
		{id: "GH-102", state: "done", events: []legacyEventSpec{
			{typ: core.EvIssueMerged, payload: map[string]any{"branch": "main", "commit": "left-unmerged"}},
			{typ: core.EvIssueCompleted, payload: map[string]any{"merge": "left-unmerged"}},
		}},
		{id: "GH-103", state: "done"},
		{id: "GH-104", state: "done"},
		{id: "GH-105", state: "done"},
	}
	e, s, _, landedSHA := newLegacyFailureEngine(t, candidates)
	appendLegacyEvents(t, s, "GH-100", validLegacyFailureEvents(landedSHA)...)
	for id, state := range map[string]string{
		"GH-103": store.IntegrationPreserved,
		"GH-104": store.IntegrationCleanupNeeded,
		"GH-105": "unknown",
	} {
		if err := s.SetIssueIntegration(store.IssueIntegration{IssueID: id, State: state}); err != nil {
			t.Fatal(err)
		}
	}

	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	validIntegration, ok, err := s.IssueIntegration("GH-100")
	if err != nil || !ok || validIntegration.State != store.IntegrationMerged ||
		validIntegration.BaseBranch != "main" || validIntegration.LandedSHA != landedSHA {
		t.Fatalf("valid integration = %+v ok=%v err=%v", validIntegration, ok, err)
	}
	assertLegacyClaimBlockers(t, e, "GH-100-child", nil)
	assertLegacyClaimBlockers(t, e, "GH-101-child", []string{"GH-101"})
	assertLegacyClaimBlockers(t, e, "GH-102-child", []string{"GH-102"})
	assertLegacyClaimBlockers(t, e, "GH-103-child", []string{"GH-103"})
	assertLegacyClaimBlockers(t, e, "GH-104-child", nil)
	assertLegacyClaimBlockers(t, e, "GH-105-child", []string{"GH-105"})

	eventCount := s.LatestEventSeq()
	if err := e.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	afterIntegration, ok, err := s.IssueIntegration("GH-100")
	if err != nil || !ok || afterIntegration.State != validIntegration.State ||
		afterIntegration.BaseBranch != validIntegration.BaseBranch ||
		afterIntegration.LandedSHA != validIntegration.LandedSHA {
		t.Fatalf("valid integration after repeat = %+v ok=%v err=%v", afterIntegration, ok, err)
	}
	if s.LatestEventSeq() != eventCount {
		t.Fatalf("event count changed after repeated rehydrate: %d -> %d", eventCount, s.LatestEventSeq())
	}
}
