package capability

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/flow"
)

func TestValidatorAcceptsOnlyContractMutations(t *testing.T) {
	repo := observerRepo(t)
	writeObserverFile(t, filepath.Join(repo, "allowed", "file.txt"), "before")
	writeObserverFile(t, filepath.Join(repo, "outside.txt"), "before")
	observerGit(t, repo, "add", ".")
	observerGit(t, repo, "commit", "-m", "baseline")
	contract := validatorContract(t, repo)
	observer := Observer{}

	baseline, err := observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeObserverFile(t, filepath.Join(repo, "allowed", "file.txt"), "after")
	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := Validate(contract, baseline, delta, nil, nil); err != nil || !result.Passed {
		t.Fatalf("allowed validation = %+v, %v", result, err)
	}

	baseline, err = observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeObserverFile(t, filepath.Join(repo, "outside.txt"), "after")
	delta, err = observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Validate(contract, baseline, delta, nil, nil)
	assertPostStageViolation(t, err)
}

func TestValidatorAllowsAncestorDirectoriesRequiredByApprovedFile(t *testing.T) {
	repo := observerRepo(t)
	observerGit(t, repo, "commit", "--allow-empty", "-m", "baseline")
	contract := validatorContract(t, repo)
	observer := Observer{}
	baseline, err := observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeObserverFile(t, filepath.Join(repo, "allowed", "nested", "file.txt"), "created")
	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := Validate(contract, baseline, delta, nil, nil); err != nil || !result.Passed {
		t.Fatalf("approved file with new ancestors validation = %+v, %v", result, err)
	}
}

func TestValidatorRejectsPathAndLinkEscapes(t *testing.T) {
	repo := observerRepo(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeObserverFile(t, outside, "secret")
	observerGit(t, repo, "commit", "--allow-empty", "-m", "baseline")
	contract := validatorContract(t, repo)
	observer := Observer{}
	baseline, err := observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "allowed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "allowed", "escape")); err != nil {
		t.Fatal(err)
	}
	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Validate(contract, baseline, delta, nil, nil)
	assertPostStageViolation(t, err)
	if got, err := os.ReadFile(outside); err != nil || string(got) != "secret" {
		t.Fatalf("outside target changed: %q, %v", got, err)
	}
}

func TestValidatorEnforcesLinearCommitHistory(t *testing.T) {
	repo := observerRepo(t)
	writeObserverFile(t, filepath.Join(repo, "allowed", "file.txt"), "before")
	observerGit(t, repo, "add", ".")
	observerGit(t, repo, "commit", "-m", "baseline")
	contract := validatorContract(t, repo)
	observer := Observer{}

	baseline, err := observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeObserverFile(t, filepath.Join(repo, "allowed", "file.txt"), "linear")
	observerGit(t, repo, "add", ".")
	observerGit(t, repo, "commit", "-m", "linear")
	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(contract, baseline, delta, nil, nil); err != nil {
		t.Fatalf("linear commit rejected: %v", err)
	}

	baseline, err = observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeObserverFile(t, filepath.Join(repo, "allowed", "file.txt"), "amended")
	observerGit(t, repo, "add", ".")
	observerGit(t, repo, "commit", "--amend", "--no-edit")
	delta, err = observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Validate(contract, baseline, delta, nil, nil)
	assertPostStageViolation(t, err)
}

func TestValidatorRejectsDeclaredOutputInCommit(t *testing.T) {
	repo := observerRepo(t)
	writeObserverFile(t, filepath.Join(repo, "allowed", "file.txt"), "before")
	observerGit(t, repo, "add", ".")
	observerGit(t, repo, "commit", "-m", "baseline")
	contract := validatorContract(t, repo)
	contract.Contract.Outputs = []RequiredOutput{{Path: "allowed/report.md", Owner: OwnerAgent}}
	contract.ContractID = digest(mustJSON(t, contract.Contract))
	authority := contract.Contract
	authority.AttemptID = ""
	contract.AuthorityDigest = digest(mustJSON(t, authority))
	observer := Observer{}
	baseline, err := observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeObserverFile(t, filepath.Join(repo, "allowed", "file.txt"), "after")
	writeObserverFile(t, filepath.Join(repo, "allowed", "report.md"), "agent report")
	observerGit(t, repo, "add", ".")
	observerGit(t, repo, "commit", "-m", "includes output")
	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Validate(contract, baseline, delta, nil, map[string]ObservedOutput{
		"allowed/report.md": {Path: "allowed/report.md", Regular: true, SHA256: digestText("agent report")},
	})
	assertPostStageViolation(t, err)
}

func TestValidatorIgnoresNoEngineWrite(t *testing.T) {
	repo := observerRepo(t)
	observerGit(t, repo, "commit", "--allow-empty", "-m", "baseline")
	contract := validatorContract(t, repo)
	observer := Observer{}
	expected := digestText("engine result")
	baseline, err := observer.Capture(repo, []EngineWrite{{Path: "verification.json", SHA256: expected}})
	if err != nil {
		t.Fatal(err)
	}
	writeObserverFile(t, filepath.Join(repo, "verification.json"), "engine result")
	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(contract, baseline, delta, nil, nil); err != nil {
		t.Fatalf("exact engine write rejected: %v", err)
	}

	baseline, err = observer.Capture(repo, []EngineWrite{{Path: "verification.json", SHA256: expected}})
	if err != nil {
		t.Fatal(err)
	}
	writeObserverFile(t, filepath.Join(repo, "verification.json"), "agent bytes")
	delta, err = observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Validate(contract, baseline, delta, nil, nil)
	assertPostStageViolation(t, err)
}

func TestValidatorAllowsAncestorDirectoryForExactEngineWrite(t *testing.T) {
	repo := observerRepo(t)
	observerGit(t, repo, "commit", "--allow-empty", "-m", "baseline")
	contract := validatorContract(t, repo)
	observer := Observer{}
	expected := digestText("engine decision")
	baseline, err := observer.Capture(repo, []EngineWrite{{Path: "decisions/1.html", SHA256: expected}})
	if err != nil {
		t.Fatal(err)
	}
	writeObserverFile(t, filepath.Join(repo, "decisions", "1.html"), "engine decision")
	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := Validate(contract, baseline, delta, nil, nil); err != nil || !result.Passed {
		t.Fatalf("engine write with new ancestor validation = %+v, %v", result, err)
	}
}

func TestValidatorRejectsGitControlMutation(t *testing.T) {
	repo := observerRepo(t)
	observerGit(t, repo, "commit", "--allow-empty", "-m", "baseline")
	contract := validatorContract(t, repo)
	observer := Observer{}
	baseline, err := observer.Capture(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(repo, ".git", "hooks", "post-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	delta, err := observer.Compare(baseline)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Validate(contract, baseline, delta, nil, nil)
	assertPostStageViolation(t, err)
}

func validatorContract(t *testing.T, repo string) CompiledContract {
	t.Helper()
	compiled, err := Compile(CompileInput{
		IssueID: "GH-68", Stage: "execute", AttemptID: "attempt-1", Profile: flow.ProfileImplementation,
		WorkspaceRoot: repo, ReadableRepositoryPaths: []string{"allowed/file.txt", "outside.txt"},
		Repository:       RepositoryIdentity{Branch: "issue/GH-68", BaseCommit: observerGit(t, repo, "rev-parse", "HEAD"), StartCommit: observerGit(t, repo, "rev-parse", "HEAD"), Tree: observerGit(t, repo, "rev-parse", "HEAD^{tree}")},
		Approval:         ApprovalBinding{Approved: true, TouchsetDigest: "touchset", DecisionID: "decision", ArtifactID: "artifact"},
		ApprovedTouchset: []string{"allowed/**"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func assertPostStageViolation(t *testing.T, err error) {
	t.Helper()
	var policyErr *PolicyError
	if !errors.As(err, &policyErr) || policyErr.Reason != ReasonPostStageViolation {
		t.Fatalf("error = %v, want %q", err, ReasonPostStageViolation)
	}
}

func observerRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	observerGit(t, repo, "init", "-b", "issue/GH-68")
	observerGit(t, repo, "config", "user.name", "Capability Test")
	observerGit(t, repo, "config", "user.email", "capability@example.test")
	return repo
}

func observerGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeObserverFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
