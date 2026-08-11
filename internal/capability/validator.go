package capability

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/weston6142/watchtower/internal/touchset"
)

type ObservedOutput struct {
	Path    string `json:"path"`
	Regular bool   `json:"regular"`
	SHA256  string `json:"sha256"`
}

func Validate(contract CompiledContract, baseline Baseline, delta Delta, runtime []AuditRecord, outputs map[string]ObservedOutput) (ValidationResult, error) {
	fail := func(operation OperationClass, paths ...string) (ValidationResult, error) {
		return ValidationResult{}, &PolicyError{Phase: "post-stage", Reason: ReasonPostStageViolation, Operation: operation, Paths: paths, Diagnostic: "observed state is outside compiled contract"}
	}
	if contract.ContractID == "" || !sameFilesystemPath(contract.Contract.WorkspaceRoot, baseline.Workspace) ||
		delta.Workspace != baseline.Workspace || baseline.Digest == "" || delta.Digest == "" {
		return fail("", baseline.Workspace)
	}
	for _, audit := range runtime {
		if audit.Outcome == "denied" || (audit.Operation != "" && !containsOperation(contract.Contract.Operations, audit.Operation)) {
			return fail(audit.Operation, audit.Paths...)
		}
	}
	for _, entry := range delta.Entries {
		if entry.EngineOwned {
			continue
		}
		path, err := touchset.CanonicalPath(entry.Path)
		if err != nil || entry.ExternalTarget {
			return fail(OpWorkspaceMutate, entry.Path)
		}
		if entry.Mutation == MutationRename {
			from, fromErr := touchset.CanonicalPath(entry.FromPath)
			if fromErr != nil || !grantAllows(contract.Contract.Writes, from, MutationRename) || !grantAllows(contract.Contract.Writes, path, MutationRename) {
				return fail(OpWorkspaceMutate, entry.FromPath, entry.Path)
			}
			continue
		}
		if !grantAllows(contract.Contract.Writes, path, entry.Mutation) {
			return fail(OpWorkspaceMutate, entry.Path)
		}
	}
	for _, required := range contract.Contract.Outputs {
		observed, ok := outputs[required.Path]
		if required.Owner == OwnerAgent && (!ok || !observed.Regular || observed.Path != required.Path || !validSHA256(observed.SHA256)) {
			return fail(OpWorkspaceMutate, required.Path)
		}
		if required.Owner == OwnerEngine && ok {
			return fail(OpWorkspaceMutate, required.Path)
		}
	}
	if delta.Git.RemoteChanged || delta.Git.BranchChanged {
		return fail(OpVCSCommit)
	}
	if delta.Git.HeadChanged {
		if !containsOperation(contract.Contract.Operations, OpVCSCommit) ||
			baseline.Git.Head != contract.Contract.Repository.StartCommit ||
			!linearDescendant(delta.Workspace, baseline.Git.Head, delta.Git.After.Head) ||
			!commitPathsAllowed(delta.Workspace, baseline.Git.Head, delta.Git.After.Head, contract.Contract.Writes) {
			return fail(OpVCSCommit)
		}
		if refsOutsideBranchChanged(delta.Git, contract.Contract.Repository.Branch) {
			return fail(OpVCSCommit)
		}
	} else if delta.Git.IndexChanged || delta.Git.RefsChanged || delta.Git.TreeChanged {
		return fail(OpVCSCommit)
	}
	encoded, err := json.Marshal(delta)
	if err != nil {
		return ValidationResult{}, err
	}
	return ValidationResult{Passed: true, DeltaDigest: hashObserverBytes(encoded)}, nil
}

func sameFilesystemPath(left, right string) bool {
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftAbs, leftErr := filepath.Abs(leftResolved)
	rightAbs, rightErr := filepath.Abs(rightResolved)
	return leftErr == nil && rightErr == nil && leftAbs == rightAbs
}

func grantAllows(grants []PathGrant, path string, mutation MutationClass) bool {
	for _, grant := range grants {
		matched, err := touchset.Match(grant.Path, path)
		if err != nil || !matched {
			continue
		}
		for _, allowed := range grant.Mutations {
			if allowed == mutation {
				return true
			}
		}
	}
	return false
}

func linearDescendant(workspace, before, after string) bool {
	if before == "" || after == "" || before == after {
		return false
	}
	if err := gitRun(workspace, "merge-base", "--is-ancestor", before, after); err != nil {
		return false
	}
	output, err := gitOutput(workspace, "rev-list", "--parents", before+".."+after)
	if err != nil || strings.TrimSpace(output) == "" {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if len(strings.Fields(line)) != 2 {
			return false
		}
	}
	return true
}

func commitPathsAllowed(workspace, before, after string, grants []PathGrant) bool {
	output, err := gitOutput(workspace, "diff", "--name-only", "--format=", before+".."+after)
	if err != nil {
		return false
	}
	for _, path := range strings.Split(strings.TrimSpace(output), "\n") {
		if path == "" {
			continue
		}
		allowed := false
		for _, mutation := range []MutationClass{MutationCreate, MutationModify, MutationDelete, MutationRename} {
			allowed = allowed || grantAllows(grants, filepath.ToSlash(path), mutation)
		}
		if !allowed || isWorkflowArtifact(path) {
			return false
		}
	}
	return true
}

func refsOutsideBranchChanged(delta GitDelta, branch string) bool {
	allowed := "refs/heads/" + branch
	seen := make(map[string]bool)
	for ref := range delta.Before.Refs {
		seen[ref] = true
	}
	for ref := range delta.After.Refs {
		seen[ref] = true
	}
	for ref := range seen {
		if delta.Before.Refs[ref] != delta.After.Refs[ref] && ref != allowed {
			return true
		}
	}
	return false
}

func isWorkflowArtifact(path string) bool {
	base := filepath.Base(path)
	switch base {
	case "ISSUE.md", "STAGE.md", "decisions.md", "brainstorm.md", "spec.md", "plan.md", "touchset.json", "verification.json":
		return true
	default:
		return strings.HasPrefix(filepath.ToSlash(path), ".watchtower/")
	}
}

func gitRun(workspace string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", workspace}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}
