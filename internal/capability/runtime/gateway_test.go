package runtime_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	capruntime "github.com/weston6142/watchtower/internal/capability/runtime"
	"github.com/weston6142/watchtower/internal/flow"
)

func TestGatewayEnforcesArtifactAndReadonlyPaths(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "ISSUE.md"), []byte("issue\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "secret.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "spec", AttemptID: "checkpoint-1", Profile: flow.ProfileArtifact,
		WorkspaceRoot: workdir, MaterializedInputs: []string{"ISSUE.md"},
		Outputs: []capability.RequiredOutput{{Path: "spec.md", Owner: capability.OwnerAgent}},
	})
	session := startRuntimeSession(t, workdir, contract)
	defer session.Close()
	if body, err := session.ReadFile("ISSUE.md"); err != nil || string(body) != "issue\n" {
		t.Fatalf("declared read body=%q err=%v", body, err)
	}
	if body, err := session.ReadFile("secret.txt"); err == nil || len(body) != 0 {
		t.Fatalf("undeclared read disclosed body=%q err=%v", body, err)
	}
	if err := session.WriteFile("spec.md", []byte("spec\n"), capability.MutationCreate); err != nil {
		t.Fatal(err)
	}
	if err := session.WriteFile("outside.md", []byte("outside\n"), capability.MutationCreate); err == nil {
		t.Fatal("out-of-contract write succeeded")
	}

	t.Run("readonly", func(t *testing.T) {
		repo := runtimeGitRepo(t)
		before := runtimeGit(t, repo, "status", "--porcelain=v1", "-z") + runtimeGit(t, repo, "rev-parse", "HEAD")
		readonly := compileRuntimeContract(t, capability.CompileInput{
			IssueID: "GH-68", Stage: "inspect", AttemptID: "checkpoint-2", Profile: flow.ProfileInspect,
			WorkspaceRoot: repo, Readonly: true, ReadableRepositoryPaths: []string{"README.md"},
		})
		readonlySession := startRuntimeSession(t, repo, readonly)
		defer readonlySession.Close()
		if _, err := readonlySession.ReadFile("README.md"); err != nil {
			t.Fatal(err)
		}
		if err := readonlySession.WriteFile("README.md", []byte("changed\n"), capability.MutationModify); err == nil {
			t.Fatal("readonly mutation succeeded")
		}
		after := runtimeGit(t, repo, "status", "--porcelain=v1", "-z") + runtimeGit(t, repo, "rev-parse", "HEAD")
		if after != before {
			t.Fatalf("readonly identity changed: before=%q after=%q", before, after)
		}
	})
}

func TestGatewayCommitIsLinearScopedAndHookFree(t *testing.T) {
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
	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch \""+hookMarker+"\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "execute", AttemptID: "checkpoint-1", Profile: flow.ProfileImplementation,
		WorkspaceRoot: repo, ReadableRepositoryPaths: []string{"README.md", "src/file.txt"},
		Repository:       capability.RepositoryIdentity{IssueID: "GH-68", Canonical: repo, Branch: branch, BaseCommit: start, StartCommit: start},
		Approval:         capability.ApprovalBinding{Approved: true, TouchsetDigest: strings.Repeat("a", 64), DecisionID: "1", ArtifactID: "touchset-v1"},
		ApprovedTouchset: []string{"src/**"},
	})
	session := startRuntimeSession(t, repo, contract)
	defer session.Close()
	if err := session.WriteFile("src/file.txt", []byte("after\n"), capability.MutationModify); err != nil {
		t.Fatal(err)
	}
	commit, err := session.Commit(context.Background(), "bounded change")
	if err != nil {
		t.Fatal(err)
	}
	if parent := runtimeGit(t, repo, "rev-parse", commit+"^"); parent != start {
		t.Fatalf("commit parent=%s want=%s", parent, start)
	}
	if _, err := os.Stat(hookMarker); !os.IsNotExist(err) {
		t.Fatalf("commit hook executed: %v", err)
	}
	if changed := runtimeGit(t, repo, "diff", "--name-only", start+".."+commit); changed != "src/file.txt" {
		t.Fatalf("committed paths=%q", changed)
	}
}

func TestSessionRemovesScratchAndRedactsAudit(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "ISSUE.md"), []byte("top secret contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "spec", AttemptID: "checkpoint-1", Profile: flow.ProfileArtifact,
		WorkspaceRoot: workdir, MaterializedInputs: []string{"ISSUE.md"},
	})
	backend := capruntime.NewPlatformBackend()
	plan, err := backend.Preflight(contract)
	if err != nil {
		t.Fatal(err)
	}
	var audit []capability.AuditRecord
	session, err := capruntime.Start(context.Background(), capruntime.StartRequest{
		Contract: contract, Plan: plan, Worktree: workdir, Backend: backend,
		Environment: []string{"TOKEN=secret", "SAFE=value"},
		Audit:       func(record capability.AuditRecord) { audit = append(audit, record) },
	})
	if err != nil {
		t.Fatal(err)
	}
	scratch := session.ScratchRoot()
	if info, err := os.Stat(scratch); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("scratch info=%+v err=%v", info, err)
	}
	if _, err := session.ReadFile("ISSUE.md"); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch survived close: %v", err)
	}
	encoded, err := json.Marshal(audit)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"top secret contents", "TOKEN=", "secret", scratch} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("audit leaked %q: %s", secret, encoded)
		}
	}
}

func TestSessionRunsContainedArgv(t *testing.T) {
	workdir := t.TempDir()
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "inspect", AttemptID: "checkpoint-1", Profile: flow.ProfileInspect,
		WorkspaceRoot: workdir,
	})
	session := startRuntimeSession(t, workdir, contract)
	defer session.Close()
	body, err := session.Run(context.Background(), []string{"/usr/bin/printf", "contained"})
	if err != nil || string(body) != "contained" {
		t.Fatalf("contained argv body=%q err=%v", body, err)
	}
}

func TestGatewayEnforcesMutationClassesAndRenameEndpoints(t *testing.T) {
	workdir := t.TempDir()
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "execute", AttemptID: "checkpoint-1", Profile: flow.ProfileArtifact,
		WorkspaceRoot: workdir,
		Outputs:       []capability.RequiredOutput{{Path: "from.txt", Owner: capability.OwnerAgent}, {Path: "to.txt", Owner: capability.OwnerAgent}},
	})
	session := startRuntimeSession(t, workdir, contract)
	defer session.Close()
	if err := session.WriteFile("from.txt", []byte("data\n"), capability.MutationCreate); err != nil {
		t.Fatal(err)
	}
	if err := session.Rename("from.txt", "to.txt"); err != nil {
		t.Fatal(err)
	}
	if err := session.Rename("to.txt", "outside.txt"); err == nil {
		t.Fatal("rename with an ungranted destination succeeded")
	}
	if err := session.Chmod("to.txt", 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(workdir, "to.txt"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("metadata mutation info=%+v err=%v", info, err)
	}
}

func TestGatewayDeniesLifecycleAndIndirectProcessBypasses(t *testing.T) {
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, "secret.txt"), []byte("undisclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "inspect", AttemptID: "checkpoint-1", Profile: flow.ProfileInspect,
		WorkspaceRoot: workdir, ReadableRepositoryPaths: []string{"file.txt"},
	})
	session := startRuntimeSession(t, workdir, contract)
	defer session.Close()
	for _, argv := range [][]string{{"git", "push"}, {"git", "rebase", "--continue"}, {"/bin/sh", "-c", "touch escaped"}, {"curl", "https://example.test"}} {
		if _, err := session.Run(context.Background(), argv); err == nil || !strings.Contains(err.Error(), string(capability.ReasonRuntimeDenied)) {
			t.Fatalf("argv %v denial=%v", argv, err)
		}
	}
	if body, err := session.Run(context.Background(), []string{"/bin/cat", "secret.txt"}); err == nil || len(body) != 0 || !strings.Contains(err.Error(), string(capability.ReasonRuntimeDenied)) {
		t.Fatalf("undeclared subprocess read body=%q err=%v", body, err)
	}
}

func compileRuntimeContract(t *testing.T, input capability.CompileInput) capability.CompiledContract {
	t.Helper()
	contract, err := capability.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func startRuntimeSession(t *testing.T, workdir string, contract capability.CompiledContract) *capruntime.Session {
	t.Helper()
	backend := capruntime.NewPlatformBackend()
	plan, err := backend.Preflight(contract)
	if err != nil {
		t.Fatal(err)
	}
	session, err := capruntime.Start(context.Background(), capruntime.StartRequest{
		Contract: contract, Plan: plan, Worktree: workdir, Backend: backend,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func runtimeGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runtimeGit(t, repo, "init", "-b", "issue/GH-68")
	runtimeGit(t, repo, "config", "user.name", "Capability Test")
	runtimeGit(t, repo, "config", "user.email", "capability@example.test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("readme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtimeGit(t, repo, "add", "README.md")
	runtimeGit(t, repo, "commit", "-m", "baseline")
	return repo
}

func runtimeGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	body, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, body)
	}
	return strings.TrimSpace(string(body))
}
