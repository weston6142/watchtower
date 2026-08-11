package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	capruntime "github.com/weston6142/watchtower/internal/capability/runtime"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/runner"
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
	if err := os.WriteFile(filepath.Join(repo, "ISSUE.md"), []byte("materialized control\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hookMarker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch \""+hookMarker+"\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "execute", AttemptID: "checkpoint-1", Profile: flow.ProfileImplementation,
		WorkspaceRoot: repo, MaterializedInputs: []string{"ISSUE.md"}, ReadableRepositoryPaths: []string{"README.md", "src/file.txt"},
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

func TestGatewayVCSReadCannotExecuteExternalDiffHelpers(t *testing.T) {
	repo := runtimeGitRepo(t)
	marker := filepath.Join(t.TempDir(), "external-diff-ran")
	helper := filepath.Join(t.TempDir(), "external-diff")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runtimeGit(t, repo, "config", "diff.external", helper)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "inspect", AttemptID: "checkpoint-vcs-read", Profile: flow.ProfileInspect,
		WorkspaceRoot: repo, Readonly: true, ReadableRepositoryPaths: []string{"README.md"},
	})
	session := startRuntimeSession(t, repo, contract)
	defer session.Close()
	if _, err := session.VCSRead(context.Background(), "diff", "--ext-diff"); err == nil {
		t.Fatal("external diff execution option was accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("VCS read executed an external diff helper: %v", err)
	}
	output := filepath.Join(t.TempDir(), "diff-output")
	if _, err := session.VCSRead(context.Background(), "diff", "--output="+output); err == nil {
		t.Fatal("VCS read output option was accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("VCS read wrote an output file: %v", err)
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

func TestSessionStartsProviderInsideContainment(t *testing.T) {
	workdir := t.TempDir()
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "inspect", AttemptID: "checkpoint-provider", Profile: flow.ProfileInspect,
		WorkspaceRoot: workdir,
	})
	session := startRuntimeSession(t, workdir, contract)
	defer session.Close()
	plan, err := capruntime.NewPlatformBackend().Preflight(contract)
	if err != nil {
		t.Fatal(err)
	}

	process, err := session.StartProvider(context.Background(), capruntime.ProviderProcessRequest{
		Path: "/usr/bin/printf", Args: []string{"contained-provider"}, Plan: plan, PipeStdout: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(process.StdoutPipe())
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil || string(body) != "contained-provider" {
		t.Fatalf("provider output=%q err=%v", body, err)
	}

	declared := filepath.Join(workdir, "declared.txt")
	if err := os.WriteFile(declared, []byte("workspace-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	process, err = session.StartProvider(context.Background(), capruntime.ProviderProcessRequest{
		Path: "/bin/sh", Args: []string{"-c", `IFS= read -r line < "$PROVIDER_DECLARED"; printf '%s' "$line"`},
		Plan: plan, Environment: []string{"PATH=/usr/bin:/bin", "PROVIDER_DECLARED=" + declared}, PipeStdout: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(process.StdoutPipe())
	waitErr := process.Wait()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if waitErr == nil || len(body) != 0 {
		t.Fatalf("provider read workspace directly: output=%q err=%v", body, waitErr)
	}

	script := filepath.Join(t.TempDir(), "provider-script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf script-provider\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var scriptStderr bytes.Buffer
	process, err = session.StartProvider(context.Background(), capruntime.ProviderProcessRequest{
		Path: script, Plan: plan, PipeStdout: true, Stderr: &scriptStderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(process.StdoutPipe())
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err != nil || string(body) != "script-provider" {
		t.Fatalf("provider script output=%q err=%v stderr=%q", body, err, scriptStderr.String())
	}

	escaped := filepath.Join(t.TempDir(), "provider-escaped")
	process, err = session.StartProvider(context.Background(), capruntime.ProviderProcessRequest{
		Path: "/bin/sh", Args: []string{"-c", `printf escaped > "$PROVIDER_ESCAPE"`},
		Plan: plan, Environment: []string{"PATH=/usr/bin:/bin", "PROVIDER_ESCAPE=" + escaped},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err == nil {
		t.Fatal("contained provider wrote outside its compiled workspace")
	}
	if _, err := os.Stat(escaped); !os.IsNotExist(err) {
		t.Fatalf("provider escaped containment: %v", err)
	}
}

func TestSessionRejectsProviderLaunchWithDifferentPlan(t *testing.T) {
	workdir := t.TempDir()
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "inspect", AttemptID: "checkpoint-plan", Profile: flow.ProfileInspect,
		WorkspaceRoot: workdir,
	})
	session := startRuntimeSession(t, workdir, contract)
	defer session.Close()
	plan, err := capruntime.NewPlatformBackend().Preflight(contract)
	if err != nil {
		t.Fatal(err)
	}
	plan.PlanID = "different"
	if _, err := session.StartProvider(context.Background(), capruntime.ProviderProcessRequest{
		Path: "/usr/bin/printf", Args: []string{"should-not-start"}, Plan: plan,
	}); err == nil {
		t.Fatal("provider launched with a different plan")
	}
}

type providerEnvironmentBackend struct{ provider string }

func (b providerEnvironmentBackend) Preflight(contract capability.CompiledContract) (capability.EnforcementPlan, error) {
	controls := runner.RequiredControls(contract)
	proofs := make([]capability.ControlProof, 0, len(controls))
	for _, control := range controls {
		proofs = append(proofs, capability.ControlProof{Control: control, Proven: true})
	}
	return runner.NewEnforcementPlan(contract, b.provider, "provider-environment-test", "1", proofs)
}

func (providerEnvironmentBackend) Wrap(request capruntime.ProcessRequest) (capruntime.ProcessRequest, error) {
	return request, nil
}

func TestProviderTransportRetainsOnlyItsOwnCredential(t *testing.T) {
	for _, test := range []struct {
		provider string
		allowed  string
		denied   string
	}{
		{provider: "codex", allowed: "OPENAI_API_KEY=openai-test", denied: "ANTHROPIC_API_KEY="},
		{provider: "claude", allowed: "ANTHROPIC_API_KEY=anthropic-test", denied: "OPENAI_API_KEY="},
	} {
		t.Run(test.provider, func(t *testing.T) {
			workdir := t.TempDir()
			contract := compileRuntimeContract(t, capability.CompileInput{
				IssueID: "GH-68", Stage: "inspect", AttemptID: "checkpoint-credential-" + test.provider,
				Profile: flow.ProfileInspect, WorkspaceRoot: workdir,
			})
			backend := providerEnvironmentBackend{provider: test.provider}
			plan, err := backend.Preflight(contract)
			if err != nil {
				t.Fatal(err)
			}
			environment := []string{
				"PATH=/usr/bin:/bin", "OPENAI_API_KEY=openai-test", "ANTHROPIC_API_KEY=anthropic-test", "GITHUB_TOKEN=publish-test",
			}
			session, err := capruntime.Start(context.Background(), capruntime.StartRequest{
				Contract: contract, Plan: plan, Worktree: workdir, Backend: backend, Environment: environment,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			process, err := session.StartProvider(context.Background(), capruntime.ProviderProcessRequest{
				Path: "/usr/bin/env", Plan: plan, Environment: environment, PipeStdout: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(process.StdoutPipe())
			if err != nil {
				t.Fatal(err)
			}
			if err := process.Wait(); err != nil {
				t.Fatal(err)
			}
			text := string(body)
			if !strings.Contains(text, test.allowed) || strings.Contains(text, test.denied) || strings.Contains(text, "GITHUB_TOKEN=") {
				t.Fatalf("%s provider environment did not preserve credential separation: %q", test.provider, text)
			}
		})
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
