package proto

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/engine"
	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/store"
)

func TestDaemonPlannerAuthorityRetryRestoresDurablePairBeforeIssuingCapability(t *testing.T) {
	coordinator, err := store.Open(filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	worktree := t.TempDir()
	binding := plannerartifact.Binding{IssueID: "GH-77", Stage: "plan", Attempt: 1, Worktree: worktree}
	authority, err := plannerartifact.CreateOrLoad(coordinator, binding)
	if err != nil {
		t.Fatal(err)
	}
	engineInstance := engine.New(engine.Config{Store: coordinator, DataDir: t.TempDir()})
	engineInstance.RegisterPlannerAuthority(authority)
	server := NewServer(engineInstance, coordinator)

	issued := server.exec(Command{Op: "planner_authority_issue", Worktree: worktree})
	if !issued.OK || issued.PlannerHandle == "" {
		t.Fatalf("issue response = %+v", issued)
	}
	manifest := plannerRouteManifest()
	goalSecret := "daemon-request-markdown-must-stay-private"
	goal := plannerartifact.WriteRequest{Manifest: manifest, Key: "goal", Markdown: goalSecret, Globs: manifest.Sections[0].Globs}
	accepted := server.exec(Command{Op: "apply_planner_artifact", Worktree: worktree, PlannerHandle: issued.PlannerHandle, PlannerRequest: &goal})
	if !accepted.OK {
		t.Fatalf("accept goal = %+v", accepted)
	}
	wantPlan, err := os.ReadFile(filepath.Join(worktree, "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	wantTouchset, err := os.ReadFile(filepath.Join(worktree, "touchset.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "plan.md"), []byte("tampered-daemon-plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(worktree, "touchset.json")); err != nil {
		t.Fatal(err)
	}

	rotated := server.exec(Command{Op: "planner_authority_retry", Worktree: worktree})
	if !rotated.OK || rotated.PlannerHandle == "" || rotated.PlannerHandle == issued.PlannerHandle {
		t.Fatalf("retry response = %+v", rotated)
	}
	gotPlan, err := os.ReadFile(filepath.Join(worktree, "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	gotTouchset, err := os.ReadFile(filepath.Join(worktree, "touchset.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPlan, wantPlan) || !bytes.Equal(gotTouchset, wantTouchset) {
		t.Fatal("daemon retry issued a capability before exact pair restoration")
	}

	architectureSecret := "daemon-architecture-markdown-must-stay-private"
	architecture := plannerartifact.WriteRequest{Manifest: manifest, Key: "architecture", Markdown: architectureSecret, Globs: manifest.Sections[1].Globs}
	stale := server.exec(Command{Op: "apply_planner_artifact", Worktree: worktree, PlannerHandle: issued.PlannerHandle, PlannerRequest: &architecture})
	if stale.OK || stale.ErrorClass != string(plannerartifact.ErrorStaleCapability) {
		t.Fatalf("old handle response = %+v", stale)
	}
	appended := server.exec(Command{Op: "apply_planner_artifact", Worktree: worktree, PlannerHandle: rotated.PlannerHandle, PlannerRequest: &architecture})
	if !appended.OK || appended.SectionKey != "architecture" {
		t.Fatalf("new handle append = %+v", appended)
	}
	for name, response := range map[string]Response{"goal": accepted, "stale": stale, "architecture": appended} {
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{goalSecret, architectureSecret, issued.PlannerHandle, rotated.PlannerHandle, "WATCHTOWER_PLANNER_SESSION", worktree} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("%s response disclosed private planner data: %s", name, encoded)
			}
		}
	}

	canonical := authority.Binding()
	status, _, storedManifest, storedSections, found, err := coordinator.LoadPlannerArtifact(canonical.IssueID, canonical.Stage, canonical.Attempt, canonical.Worktree)
	if err != nil || !found {
		t.Fatalf("load recovered authority: found=%v err=%v", found, err)
	}
	if err := coordinator.UpdatePlannerArtifact(canonical.IssueID, canonical.Stage, canonical.Attempt, canonical.Worktree, status, []byte("short"), storedManifest, storedSections); err != nil {
		t.Fatal(err)
	}
	corrupt := server.exec(Command{Op: "planner_authority_retry", Worktree: worktree})
	if corrupt.OK || corrupt.PlannerHandle != "" || corrupt.ErrorClass != string(plannerartifact.ErrorAuthorityState) || corrupt.CorrelationID == "" {
		t.Fatalf("corrupt digest response = %+v", corrupt)
	}
	encoded, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{goalSecret, architectureSecret, issued.PlannerHandle, rotated.PlannerHandle, "short", "WATCHTOWER_PLANNER_SESSION", worktree} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("corrupt response disclosed private planner data: %s", encoded)
		}
	}
}

func TestDaemonPlannerAuthorityReportsUninitializedWithoutClientFallback(t *testing.T) {
	coordinator, err := store.Open(filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()

	worktree := t.TempDir()
	response := NewServer(nil, coordinator).exec(Command{
		Op:       "planner_authority_issue",
		Worktree: worktree,
	})
	if response.OK || response.ErrorClass != string(plannerartifact.ErrorAuthorityUninitialized) {
		t.Fatalf("uninitialized planner authority response = %+v", response)
	}
	if response.PlannerHandle != "" || response.Error == "" {
		t.Fatalf("uninitialized response exposed authority or omitted safe error: %+v", response)
	}
}

func TestDaemonPlannerAuthorityAppliesThroughEngineAndRetainsSafeResponse(t *testing.T) {
	coordinator, err := store.Open(filepath.Join(t.TempDir(), "coordinator.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.Close()
	worktree := t.TempDir()
	binding := plannerartifact.Binding{IssueID: "GH-72", Stage: "plan", Attempt: 1, Worktree: worktree}
	authority, err := plannerartifact.CreateOrLoad(coordinator, binding)
	if err != nil {
		t.Fatal(err)
	}
	engineInstance := engine.New(engine.Config{Store: coordinator, DataDir: t.TempDir()})
	engineInstance.RegisterPlannerAuthority(authority)
	server := NewServer(engineInstance, coordinator)

	issued := server.exec(Command{Op: "planner_authority_issue", Worktree: worktree})
	if !issued.OK || issued.PlannerHandle == "" || issued.CorrelationID == "" {
		t.Fatalf("issue response = %+v", issued)
	}
	manifest := plannerRouteManifest()
	request := plannerartifact.WriteRequest{Manifest: manifest, Key: "goal", Markdown: "final daemon-routed goal", Globs: manifest.Sections[0].Globs}
	missingHandle := server.exec(Command{
		Op: "apply_planner_artifact", Worktree: worktree, PlannerRequest: &request,
	})
	if missingHandle.OK || missingHandle.ErrorClass != string(plannerartifact.ErrorStaleCapability) {
		t.Fatalf("missing capability response = %+v", missingHandle)
	}
	result := server.exec(Command{
		Op: "apply_planner_artifact", Worktree: worktree, PlannerHandle: issued.PlannerHandle,
		PlannerRequest: &request,
	})
	if !result.OK || result.SectionKey != "goal" {
		t.Fatalf("apply response = %+v", result)
	}
	plan, err := os.ReadFile(filepath.Join(worktree, "plan.md"))
	if err != nil || !strings.Contains(string(plan), request.Markdown) {
		t.Fatalf("daemon route did not publish pair: err=%v plan=%q", err, plan)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), issued.PlannerHandle) || strings.Contains(string(encoded), request.Markdown) {
		t.Fatalf("safe response disclosed capability or request body: %s", encoded)
	}

	mismatch := server.exec(Command{Op: "apply_planner_artifact", IssueID: "GH-71", Worktree: worktree,
		PlannerHandle: issued.PlannerHandle, PlannerRequest: &request})
	if mismatch.OK || mismatch.ErrorClass != string(plannerartifact.ErrorScopeMismatch) {
		t.Fatalf("scope mismatch response = %+v", mismatch)
	}
	rotated := server.exec(Command{Op: "planner_authority_retry", Worktree: worktree})
	if !rotated.OK || rotated.PlannerHandle == issued.PlannerHandle {
		t.Fatalf("retry did not rotate capability: %+v", rotated)
	}
	stale := server.exec(Command{Op: "apply_planner_artifact", Worktree: worktree,
		PlannerHandle: issued.PlannerHandle, PlannerRequest: &request})
	if stale.OK || stale.ErrorClass != string(plannerartifact.ErrorStaleCapability) {
		t.Fatalf("stale capability response = %+v", stale)
	}
}

func plannerRouteManifest() plannerartifact.Manifest {
	return plannerartifact.Manifest{Sections: []plannerartifact.ManifestEntry{
		{Key: "goal", Globs: []string{"internal/gh72/goal/**"}},
		{Key: "architecture", Globs: []string{"internal/gh72/architecture/**"}},
		{Key: "technology-stack", Globs: []string{"internal/gh72/technology/**"}},
		{Key: "execution-contract", Globs: []string{"internal/gh72/contract/**"}},
		{Key: "file-structure", Globs: []string{"internal/gh72/files/**"}},
		{Key: "task-0001", Globs: []string{"internal/gh72/task/**"}},
		{Key: "verification", Globs: []string{"internal/gh72/verification/**"}},
	}}
}
