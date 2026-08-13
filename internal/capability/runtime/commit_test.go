package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/flow"
)

func TestGatewayCommitRejectsOutOfScopeIndexContent(t *testing.T) {
	repo := runtimeGitRepo(t)
	start := runtimeGit(t, repo, "rev-parse", "HEAD")
	branch := runtimeGit(t, repo, "symbolic-ref", "--short", "HEAD")
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "execute", AttemptID: "checkpoint-1", Profile: flow.ProfileImplementation,
		WorkspaceRoot: repo, ReadableRepositoryPaths: []string{"README.md"},
		Repository:       capability.RepositoryIdentity{IssueID: "GH-68", Canonical: repo, Branch: branch, BaseCommit: start, StartCommit: start},
		Approval:         capability.ApprovalBinding{Approved: true, TouchsetDigest: strings.Repeat("a", 64), DecisionID: "1", ArtifactID: "touchset-v1"},
		ApprovedTouchset: []string{"src/**"},
	})
	session := startRuntimeSession(t, repo, contract)
	defer session.Close()
	if err := os.WriteFile(filepath.Join(repo, "outside.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Commit(context.Background(), "unbounded change"); err == nil {
		t.Fatal("out-of-scope commit succeeded")
	}
	if head := runtimeGit(t, repo, "rev-parse", "HEAD"); head != start {
		t.Fatalf("rejected commit advanced HEAD to %s", head)
	}
}

func TestGatewayCommitExcludesDeclaredOutputs(t *testing.T) {
	repo := runtimeGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "src", "file.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtimeGit(t, repo, "add", "src/file.txt")
	runtimeGit(t, repo, "commit", "-m", "source")
	start := runtimeGit(t, repo, "rev-parse", "HEAD")
	branch := runtimeGit(t, repo, "symbolic-ref", "--short", "HEAD")
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "review", AttemptID: "checkpoint-output", Profile: flow.ProfileReview,
		WorkspaceRoot: repo, ReadableRepositoryPaths: []string{"src/file.txt"},
		Repository:       capability.RepositoryIdentity{IssueID: "GH-68", Canonical: repo, Branch: branch, BaseCommit: start, StartCommit: start},
		Approval:         capability.ApprovalBinding{Approved: true, TouchsetDigest: strings.Repeat("a", 64), DecisionID: "1", ArtifactID: "touchset-v1"},
		ApprovedTouchset: []string{"src/**"},
		Outputs:          []capability.RequiredOutput{{Path: "src/review-report.md", Owner: capability.OwnerAgent}},
	})
	session := startRuntimeSession(t, repo, contract)
	defer session.Close()
	if err := session.WriteFile("src/file.txt", []byte("after\n"), capability.MutationModify); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteFile("src/review-report.md", []byte("report\n"), capability.MutationCreate); err != nil {
		t.Fatal(err)
	}
	commit, err := session.Commit(context.Background(), "bounded repair")
	if err != nil {
		t.Fatal(err)
	}
	if changed := runtimeGit(t, repo, "diff", "--name-only", start+".."+commit); changed != "src/file.txt" {
		t.Fatalf("committed paths=%q", changed)
	}
	if status := runtimeGit(t, repo, "status", "--short", "--", "src/review-report.md"); status != "?? src/review-report.md" {
		t.Fatalf("declared output status=%q", status)
	}
}
