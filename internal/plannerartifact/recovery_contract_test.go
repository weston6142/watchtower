package plannerartifact_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/weston6142/watchtower/internal/plannerartifact"
	"github.com/weston6142/watchtower/internal/store"
)

func TestPlannerArtifactRecoveryContractRestoresPartialAndCompleteFileBackedState(t *testing.T) {
	t.Run("partial prefix replays exactly and advances only the next section", func(t *testing.T) {
		databasePath := filepath.Join(t.TempDir(), "coordinator.db")
		worktree := t.TempDir()
		binding := plannerartifact.Binding{IssueID: "GH-77", Stage: "plan", Attempt: 1, Worktree: worktree}
		manifest := recoveryContractManifest()
		coordinator := openRecoveryStore(t, databasePath)
		authority, err := plannerartifact.CreateOrLoad(coordinator, binding)
		if err != nil {
			t.Fatal(err)
		}
		for _, request := range recoveryContractRequests(manifest)[:2] {
			if err := authority.Apply(request); err != nil {
				t.Fatalf("seed %s: %v", request.Key, err)
			}
		}
		canonical := authority.Binding()
		wantPlan := readRecoveryContractFile(t, worktree, "plan.md")
		wantTouchset := readRecoveryContractFile(t, worktree, "touchset.json")
		_, oldDigest, wantManifest, wantSections, found, err := coordinator.LoadPlannerArtifact(canonical.IssueID, canonical.Stage, canonical.Attempt, canonical.Worktree)
		if err != nil || !found {
			t.Fatalf("load seeded record: found=%v err=%v", found, err)
		}
		if err := authority.Close(); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.Close(); err != nil {
			t.Fatal(err)
		}

		if err := os.Remove(filepath.Join(worktree, "plan.md")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, "touchset.json"), []byte(`{"globs":["tampered/**"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		coordinator = openRecoveryStore(t, databasePath)
		defer coordinator.Close()
		recovered, err := plannerartifact.CreateOrLoad(coordinator, binding, plannerartifact.RequireDurableRecovery())
		if err != nil {
			t.Fatalf("recover partial prefix: %v", err)
		}
		if !bytes.Equal(readRecoveryContractFile(t, worktree, "plan.md"), wantPlan) || !bytes.Equal(readRecoveryContractFile(t, worktree, "touchset.json"), wantTouchset) {
			t.Fatal("partial recovery did not restore exact rendered pair")
		}
		_, newDigest, gotManifest, gotSections, found, err := coordinator.LoadPlannerArtifact(canonical.IssueID, canonical.Stage, canonical.Attempt, canonical.Worktree)
		if err != nil || !found {
			t.Fatalf("load recovered record: found=%v err=%v", found, err)
		}
		if bytes.Equal(oldDigest, newDigest) || len(newDigest) == 0 {
			t.Fatal("successful exact recovery did not rotate the live digest")
		}
		if !bytes.Equal(gotManifest, wantManifest) || !bytes.Equal(gotSections, wantSections) {
			t.Fatal("recovery changed durable manifest or accepted-section byte streams")
		}

		requests := recoveryContractRequests(manifest)
		if err := recovered.Apply(requests[0]); err != nil {
			t.Fatalf("replay goal: %v", err)
		}
		if err := recovered.Apply(requests[1]); err != nil {
			t.Fatalf("replay architecture: %v", err)
		}
		if err := recovered.Apply(requests[3]); err == nil {
			t.Fatal("recovered prefix accepted an out-of-order section")
		}
		accepted, err := recovered.AcceptedSections()
		if err != nil || len(accepted) != 2 {
			t.Fatalf("accepted prefix after rejected skip = %d err=%v", len(accepted), err)
		}
		if err := recovered.Apply(requests[2]); err != nil {
			t.Fatalf("next pending section: %v", err)
		}
		accepted, err = recovered.AcceptedSections()
		if err != nil || len(accepted) != 3 || accepted[2].Key != "technology-stack" {
			t.Fatalf("advanced prefix = %#v err=%v", accepted, err)
		}
	})

	t.Run("complete prefix remains complete after restart recovery", func(t *testing.T) {
		databasePath := filepath.Join(t.TempDir(), "coordinator.db")
		worktree := t.TempDir()
		binding := plannerartifact.Binding{IssueID: "GH-77", Stage: "plan", Attempt: 1, Worktree: worktree}
		coordinator := openRecoveryStore(t, databasePath)
		authority, err := plannerartifact.CreateOrLoad(coordinator, binding)
		if err != nil {
			t.Fatal(err)
		}
		for _, request := range recoveryContractRequests(recoveryContractManifest()) {
			if err := authority.Apply(request); err != nil {
				t.Fatalf("seed %s: %v", request.Key, err)
			}
		}
		wantPlan := readRecoveryContractFile(t, worktree, "plan.md")
		wantTouchset := readRecoveryContractFile(t, worktree, "touchset.json")
		if err := authority.Close(); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(worktree, "plan.md"), []byte("tampered complete plan"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(worktree, "touchset.json")); err != nil {
			t.Fatal(err)
		}

		coordinator = openRecoveryStore(t, databasePath)
		defer coordinator.Close()
		recovered, err := plannerartifact.CreateOrLoad(coordinator, binding, plannerartifact.RequireDurableRecovery())
		if err != nil {
			t.Fatalf("recover complete prefix: %v", err)
		}
		if !bytes.Equal(readRecoveryContractFile(t, worktree, "plan.md"), wantPlan) || !bytes.Equal(readRecoveryContractFile(t, worktree, "touchset.json"), wantTouchset) {
			t.Fatal("complete recovery changed rendered bytes")
		}
		if err := recovered.ValidateComplete(); err != nil {
			t.Fatalf("recovered complete authority: %v", err)
		}
	})
}

func TestPlannerArtifactRecoveryContractRollsBackPairWhenRegistryRotationFails(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "coordinator.db")
	worktree := t.TempDir()
	binding := plannerartifact.Binding{IssueID: "GH-77", Stage: "plan", Attempt: 1, Worktree: worktree}
	coordinator := openRecoveryStore(t, databasePath)
	authority, err := plannerartifact.CreateOrLoad(coordinator, binding)
	if err != nil {
		t.Fatal(err)
	}
	request := recoveryContractRequests(recoveryContractManifest())[0]
	if err := authority.Apply(request); err != nil {
		t.Fatal(err)
	}
	canonical := authority.Binding()
	_, oldDigest, _, _, _, err := coordinator.LoadPlannerArtifact(canonical.IssueID, canonical.Stage, canonical.Attempt, canonical.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}

	planPath := filepath.Join(worktree, "plan.md")
	touchsetPath := filepath.Join(worktree, "touchset.json")
	if err := os.Remove(planPath); err != nil {
		t.Fatal(err)
	}
	tamperedTouchset := []byte("pre-recovery-touchset-bytes")
	if err := os.WriteFile(touchsetPath, tamperedTouchset, 0o600); err != nil {
		t.Fatal(err)
	}
	coordinator = openRecoveryStore(t, databasePath)
	defer coordinator.Close()
	coordinator.FailNextPlannerArtifactWriteForTest()
	recovered, err := plannerartifact.CreateOrLoad(coordinator, binding, plannerartifact.RequireDurableRecovery())
	if recovered != nil || err == nil || plannerartifact.ErrorClassOf(err) != plannerartifact.ErrorAuthorityState {
		t.Fatalf("registry failure recovery = %#v err=%v", recovered, err)
	}
	if _, err := os.Stat(planPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback did not restore absent plan: %v", err)
	}
	if !bytes.Equal(readRecoveryContractFile(t, worktree, "touchset.json"), tamperedTouchset) {
		t.Fatal("rollback did not restore prior touchset bytes")
	}
	_, gotDigest, _, _, found, err := coordinator.LoadPlannerArtifact(canonical.IssueID, canonical.Stage, canonical.Attempt, canonical.Worktree)
	if err != nil || !found || !bytes.Equal(gotDigest, oldDigest) {
		t.Fatalf("registry failure changed durable digest: found=%v err=%v", found, err)
	}
}

func openRecoveryStore(t *testing.T, path string) *store.Store {
	t.Helper()
	coordinator, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func readRecoveryContractFile(t *testing.T, worktree, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(worktree, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func recoveryContractManifest() plannerartifact.Manifest {
	return plannerartifact.Manifest{Sections: []plannerartifact.ManifestEntry{
		{Key: "goal", Globs: []string{"internal/recovery/goal/**"}},
		{Key: "architecture", Globs: []string{"internal/recovery/architecture/**"}},
		{Key: "technology-stack", Globs: []string{"internal/recovery/technology/**"}},
		{Key: "execution-contract", Globs: []string{"internal/recovery/contract/**"}},
		{Key: "file-structure", Globs: []string{"internal/recovery/files/**"}},
		{Key: "task-0001", Globs: []string{"internal/recovery/task/**"}},
		{Key: "verification", Globs: []string{"internal/recovery/verification/**"}},
	}}
}

func recoveryContractRequests(manifest plannerartifact.Manifest) []plannerartifact.WriteRequest {
	requests := make([]plannerartifact.WriteRequest, 0, len(manifest.Sections))
	for _, section := range manifest.Sections {
		requests = append(requests, plannerartifact.WriteRequest{Manifest: manifest, Key: section.Key, Markdown: "final " + section.Key + " contract", Globs: section.Globs})
	}
	return requests
}
