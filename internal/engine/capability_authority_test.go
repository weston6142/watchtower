package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/store"
)

func TestCapabilityAuthorityUsesArchivedApprovedTouchset(t *testing.T) {
	fixture := newCapabilityAuthorityFixture(t, []byte(`{"globs":["allowed/**"]}`))
	if err := os.WriteFile(filepath.Join(fixture.worktree, "touchset.json"), []byte(`{"globs":["outside/**"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	authority, err := fixture.engine.resolveCapabilityAuthority(fixture.issue, flow.Stage{
		Name: "execute", CapabilityProfile: flow.ProfileImplementation,
	}, store.BeginAttempt(fixture.issue.id, "execute", "checkpoint-2"), []string{"plan.md", "touchset.json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(authority.AgentWritablePaths, []string{"allowed/**"}) || authority.ApprovedTouchsetDigest != fixture.touchsetDigest {
		t.Fatalf("authority = %+v", authority)
	}
	if authority.ApprovalBinding.DecisionID == "" || authority.ApprovalBinding.ArtifactID == "" {
		t.Fatalf("approval binding = %+v", authority.ApprovalBinding)
	}
}

func TestCapabilityAuthorityRejectsMutableOrStaleTouchset(t *testing.T) {
	t.Run("digest mismatch", func(t *testing.T) {
		fixture := newCapabilityAuthorityFixture(t, []byte(`{"globs":["allowed/**"]}`))
		if err := os.WriteFile(fixture.archivedTouchset, []byte(`{"globs":["outside/**"]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := fixture.engine.resolveCapabilityAuthority(fixture.issue, flow.Stage{Name: "execute", CapabilityProfile: flow.ProfileImplementation},
			store.BeginAttempt(fixture.issue.id, "execute", "checkpoint-2"), nil, nil)
		if err == nil || !strings.Contains(err.Error(), "digest") {
			t.Fatalf("resolve error = %v", err)
		}
	})
	t.Run("missing archive", func(t *testing.T) {
		fixture := newCapabilityAuthorityFixture(t, []byte(`{"globs":["allowed/**"]}`))
		if err := os.Remove(fixture.archivedTouchset); err != nil {
			t.Fatal(err)
		}
		_, err := fixture.engine.resolveCapabilityAuthority(fixture.issue, flow.Stage{Name: "execute", CapabilityProfile: flow.ProfileImplementation},
			store.BeginAttempt(fixture.issue.id, "execute", "checkpoint-2"), nil, nil)
		if err == nil {
			t.Fatal("missing archived touchset was accepted")
		}
	})
}

func TestCapabilityAuthorityMaterializesArtifactPaths(t *testing.T) {
	fixture := newCapabilityAuthorityFixture(t, []byte(`{"globs":["allowed/**"]}`))
	stage := flow.Stage{Name: "plan", CapabilityProfile: flow.ProfileArtifact, Artifacts: []string{"plan.md", "touchset.json"}}
	authority, err := fixture.engine.resolveCapabilityAuthority(fixture.issue, stage,
		store.BeginAttempt(fixture.issue.id, stage.Name, "checkpoint-2"),
		[]string{"attachments/design.txt", "spec.md"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantReadable := []string{"ISSUE.md", "STAGE.md", "attachments/design.txt", "decisions.md", "plan.md", "spec.md", "touchset.json"}
	if !reflect.DeepEqual(authority.MaterializedReadablePaths, wantReadable) {
		t.Fatalf("materialized reads = %v, want %v", authority.MaterializedReadablePaths, wantReadable)
	}
	if !reflect.DeepEqual(authority.AgentWritablePaths, []string{"plan.md", "touchset.json"}) {
		t.Fatalf("artifact writes = %v", authority.AgentWritablePaths)
	}
	if strings.HasPrefix(authority.ScratchRoot, authority.WorkspaceRoot) || authority.ScratchRoot == "" {
		t.Fatalf("scratch is not external: %q", authority.ScratchRoot)
	}
}

func TestCapabilityAuthorityIntersectsLibrarianAndConflictScopes(t *testing.T) {
	fixture := newCapabilityAuthorityFixture(t, []byte(`{"globs":["docs/guildhall/**","internal/**"]}`))
	librarian, err := fixture.engine.resolveCapabilityAuthority(fixture.issue, flow.Stage{
		Name: "librarian", CapabilityProfile: flow.ProfileLibrarian, DocumentationPaths: []string{"docs/**"},
	}, store.BeginAttempt(fixture.issue.id, "librarian", "checkpoint-2"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(librarian.AgentWritablePaths, []string{"docs/guildhall/**"}) {
		t.Fatalf("librarian writes = %v", librarian.AgentWritablePaths)
	}
	conflict, err := fixture.engine.resolveCapabilityAuthority(fixture.issue, flow.Stage{
		Name: "conflict", CapabilityProfile: flow.ProfileConflictResolution,
	}, store.BeginAttempt(fixture.issue.id, "conflict", "checkpoint-2"), nil, []string{"internal/engine.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(conflict.AgentWritablePaths, []string{"internal/engine.go"}) {
		t.Fatalf("conflict writes = %v", conflict.AgentWritablePaths)
	}
	if _, err := fixture.engine.resolveCapabilityAuthority(fixture.issue, flow.Stage{
		Name: "conflict", CapabilityProfile: flow.ProfileConflictResolution,
	}, store.BeginAttempt(fixture.issue.id, "conflict", "checkpoint-3"), nil, []string{"outside.go"}); err == nil {
		t.Fatal("out-of-touchset conflict was accepted")
	}
}

type capabilityAuthorityFixture struct {
	engine           *Engine
	issue            *issueState
	worktree         string
	archivedTouchset string
	touchsetDigest   string
}

func newCapabilityAuthorityFixture(t *testing.T, touchsetBytes []byte) capabilityAuthorityFixture {
	t.Helper()
	dataDir := t.TempDir()
	worktree := observerRepoForEngine(t)
	if err := os.MkdirAll(filepath.Join(worktree, "allowed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "allowed", "file.go"), []byte("package allowed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engineGit(t, worktree, "add", ".")
	engineGit(t, worktree, "commit", "-m", "baseline")

	s, err := store.Open(filepath.Join(t.TempDir(), "authority.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	e := New(Config{Store: s, DataDir: dataDir})
	policy := review.ManualPlanReviewPolicy(flow.LeverRegular)
	issue := &issueState{id: "GH-68", wsPath: worktree, branch: "issue/GH-68", baseRef: engineGit(t, worktree, "rev-parse", "HEAD"), planReview: policy}

	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "touchset.json"), touchsetBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "plan.md"), []byte("# plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkpointID, err := s.InsertStageCheckpoint(store.StageCheckpoint{IssueID: issue.id, Stage: "plan", Status: "running"})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := "checkpoint-" + strconv.FormatInt(checkpointID, 10)
	result, err := contextpack.MaterializeAttemptResult(source, e.issueDir(issue.id), attemptID,
		[]string{"plan.md", "touchset.json"}, contextpack.AttemptResult{IssueID: issue.id, Stage: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	refs, err := contextpack.PublishAttemptArchive(e.issueDir(issue.id), result, attemptID+":artifacts_archived")
	if err != nil {
		t.Fatal(err)
	}
	stageAttempt := store.BeginAttempt(issue.id, "plan", attemptID)
	stageAttempt.CapabilitySchemaVersion = 0
	stageAttempt.LegacyCheckpointID = checkpointID
	if err := s.CreateStageLifecycleAttempt(stageAttempt); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStageArchiveManifest(stageAttempt, attemptID+":artifacts_archived", refs); err != nil {
		t.Fatal(err)
	}
	artifacts := make([]contextpack.Artifact, 0, len(refs))
	var touchsetRef contextpack.AttemptArtifact
	for _, ref := range refs {
		artifacts = append(artifacts, contextpack.Artifact{Name: ref.Name, SHA256: ref.SHA256})
		if ref.Name == "touchset.json" {
			touchsetRef = ref
		}
	}
	target, err := (review.Target{IssueID: issue.id, Stage: "plan", CheckpointID: checkpointID, Artifacts: artifacts, NextStage: "execute"}).Canonical()
	if err != nil {
		t.Fatal(err)
	}
	_, bindings, _, err := e.evaluateReviewTarget(target, false)
	if err != nil {
		t.Fatal(err)
	}
	decisionID, err := s.RequestArtifactReview(target, store.DecisionRow{
		IssueID: issue.id, Stage: "plan", Question: "Approve?", Options: []string{"Approve", "Revise"}, Recommended: 0,
		ReviewPolicy: &policy, Bindings: bindings,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveArtifactReview(decisionID, target, levers.ChoiceResponse(0), &review.ApprovalProvenance{Kind: review.ApprovalHuman, ActorID: review.DefaultActorID}); err != nil {
		t.Fatal(err)
	}
	event, err := core.NewEvent(core.EvPlanReviewHumanApproved, issue.id, map[string]any{"decision_id": decisionID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(event); err != nil {
		t.Fatal(err)
	}
	return capabilityAuthorityFixture{
		engine: e, issue: issue, worktree: worktree,
		archivedTouchset: filepath.Join(e.issueDir(issue.id), filepath.FromSlash(touchsetRef.Path)),
		touchsetDigest:   touchsetRef.SHA256,
	}
}

func observerRepoForEngine(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	engineGit(t, repo, "init", "-b", "issue/GH-68")
	engineGit(t, repo, "config", "user.name", "Authority Test")
	engineGit(t, repo, "config", "user.email", "authority@example.test")
	return repo
}

func engineGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
