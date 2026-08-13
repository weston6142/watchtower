package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/touchset"
)

type resolvedCapabilityAuthority struct {
	Profile                   flow.CapabilityProfile
	MaterializedReadablePaths []string
	ReadableRepositoryPaths   []string
	AgentWritablePaths        []string
	RequiredOutputs           []capability.RequiredOutput
	DocumentationPaths        []string
	ConflictPaths             []string
	Repository                capability.RepositoryIdentity
	WorkspaceRoot             string
	ApprovedTouchset          []string
	ApprovedTouchsetDigest    string
	ApprovalBinding           capability.ApprovalBinding
	ScratchRoot               string
}

func (e *Engine) resolveCapabilityAuthority(
	is *issueState, stage flow.Stage, lifecycleAttempt store.StageLifecycleAttempt,
	materializedInputs, conflictPaths []string,
) (resolvedCapabilityAuthority, error) {
	if is == nil || is.id == "" || stage.Name == "" || lifecycleAttempt.AttemptID == "" {
		return resolvedCapabilityAuthority{}, fmt.Errorf("capability authority identity is incomplete")
	}
	worktree := e.stageWorkdir(is, stage)
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return resolvedCapabilityAuthority{}, fmt.Errorf("resolve capability worktree: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return resolvedCapabilityAuthority{}, err
	}
	branch := is.branch
	head, tree := is.baseRef, ""
	if stage.CapabilityProfile != flow.ProfileArtifact {
		head, err = gitRevision(root, "HEAD")
		if err != nil {
			return resolvedCapabilityAuthority{}, fmt.Errorf("resolve trusted HEAD: %w", err)
		}
		tree, err = gitRevision(root, "HEAD^{tree}")
		if err != nil {
			return resolvedCapabilityAuthority{}, fmt.Errorf("resolve trusted tree: %w", err)
		}
		if branch == "" {
			if branch, err = gitCommandOutput(root, "symbolic-ref", "--quiet", "--short", "HEAD"); err != nil {
				return resolvedCapabilityAuthority{}, fmt.Errorf("resolve issue branch: %w", err)
			}
		}
	}
	outputs, agentOutputPaths, err := capabilityOutputOwnership(stage)
	if err != nil {
		return resolvedCapabilityAuthority{}, err
	}
	materialized := append([]string{"ISSUE.md", "STAGE.md", "decisions.md"}, materializedInputs...)
	for _, output := range outputs {
		materialized = append(materialized, output.Path)
	}
	materialized, err = canonicalExactPaths(materialized)
	if err != nil {
		return resolvedCapabilityAuthority{}, err
	}

	authority := resolvedCapabilityAuthority{
		Profile: stage.CapabilityProfile, MaterializedReadablePaths: materialized,
		RequiredOutputs: outputs, Repository: capability.RepositoryIdentity{
			IssueID: is.id, Canonical: root, Branch: branch, BaseCommit: is.baseRef,
			StartCommit: head, Tree: tree,
		},
		WorkspaceRoot: root,
		ScratchRoot:   filepath.Join(os.TempDir(), "watchtower-capability", is.id, lifecycleAttempt.AttemptID),
	}

	if stage.CapabilityProfile == flow.ProfileArtifact {
		authority.AgentWritablePaths = agentOutputPaths
		return authority, nil
	}
	authority.ReadableRepositoryPaths, err = enumerateCapabilityRepository(root)
	if err != nil {
		return resolvedCapabilityAuthority{}, err
	}
	if stage.CapabilityProfile == flow.ProfileInspect {
		return authority, nil
	}

	approved, digest, binding, err := e.archivedApprovedTouchset(is)
	if err != nil {
		return resolvedCapabilityAuthority{}, err
	}
	authority.ApprovedTouchset = approved.Globs
	authority.ApprovedTouchsetDigest = digest
	authority.ApprovalBinding = binding
	switch stage.CapabilityProfile {
	case flow.ProfileImplementation, flow.ProfileReview, flow.ProfileFinalReview:
		authority.AgentWritablePaths = append([]string(nil), approved.Globs...)
	case flow.ProfileLibrarian:
		authority.DocumentationPaths, err = touchset.CanonicalGlobs(stage.DocumentationPaths)
		if err != nil {
			return resolvedCapabilityAuthority{}, fmt.Errorf("librarian documentation paths: %w", err)
		}
		authority.AgentWritablePaths, err = touchset.IntersectGlobs(approved.Globs, authority.DocumentationPaths)
		if err != nil {
			return resolvedCapabilityAuthority{}, fmt.Errorf("librarian documentation intersection: %w", err)
		}
		if len(authority.AgentWritablePaths) == 0 {
			return resolvedCapabilityAuthority{}, fmt.Errorf("librarian documentation scope has no approved touchset intersection")
		}
	case flow.ProfileConflictResolution:
		authority.ConflictPaths, err = canonicalExactPaths(conflictPaths)
		if err != nil || len(authority.ConflictPaths) == 0 {
			return resolvedCapabilityAuthority{}, fmt.Errorf("conflict paths are missing or unsafe")
		}
		for _, path := range authority.ConflictPaths {
			if !matchesAny(approved.Globs, path) {
				return resolvedCapabilityAuthority{}, fmt.Errorf("conflict path %q is outside approved touchset", path)
			}
		}
		authority.AgentWritablePaths = append([]string(nil), authority.ConflictPaths...)
	default:
		return resolvedCapabilityAuthority{}, fmt.Errorf("capability profile %q has no authority resolver", stage.CapabilityProfile)
	}
	return authority, nil
}

func (e *Engine) archivedApprovedTouchset(is *issueState) (touchset.Set, string, capability.ApprovalBinding, error) {
	approval, _, err := e.planReviewAuthorization(is)
	if err != nil {
		return touchset.Set{}, "", capability.ApprovalBinding{}, err
	}
	if approval.Review == nil {
		return touchset.Set{}, "", capability.ApprovalBinding{}, fmt.Errorf("plan review target is missing")
	}
	target, err := approval.Review.Canonical()
	if err != nil {
		return touchset.Set{}, "", capability.ApprovalBinding{}, fmt.Errorf("canonicalize plan review target: %w", err)
	}
	var touchsetDigest string
	for _, artifact := range target.Artifacts {
		if artifact.Name == "touchset.json" {
			touchsetDigest = artifact.SHA256
			break
		}
	}
	if touchsetDigest == "" {
		return touchset.Set{}, "", capability.ApprovalBinding{}, fmt.Errorf("approved plan review has no touchset.json")
	}
	attempts, err := e.cfg.Store.StageLifecycleAttempts(is.id, target.Stage)
	if err != nil {
		return touchset.Set{}, "", capability.ApprovalBinding{}, err
	}
	attemptID := ""
	for _, attempt := range attempts {
		if attempt.LegacyCheckpointID == target.CheckpointID {
			if attemptID != "" && attemptID != attempt.AttemptID {
				return touchset.Set{}, "", capability.ApprovalBinding{}, fmt.Errorf("approved touchset checkpoint maps to multiple attempts")
			}
			attemptID = attempt.AttemptID
		}
	}
	if attemptID == "" {
		return touchset.Set{}, "", capability.ApprovalBinding{}, fmt.Errorf("approved touchset attempt is missing")
	}
	archivePath := filepath.Join(e.issueDir(is.id), "artifacts", "attempts", attemptID, "touchset.json")
	info, err := os.Lstat(archivePath)
	if err != nil {
		return touchset.Set{}, "", capability.ApprovalBinding{}, fmt.Errorf("read approved touchset archive: %w", err)
	}
	if !info.Mode().IsRegular() {
		return touchset.Set{}, "", capability.ApprovalBinding{}, fmt.Errorf("approved touchset archive is not a regular file")
	}
	bytes, err := os.ReadFile(archivePath)
	if err != nil {
		return touchset.Set{}, "", capability.ApprovalBinding{}, err
	}
	if got := sha256Hex(bytes); got != touchsetDigest {
		return touchset.Set{}, "", capability.ApprovalBinding{}, fmt.Errorf("approved touchset digest mismatch")
	}
	var approved touchset.Set
	if err := json.Unmarshal(bytes, &approved); err != nil {
		return touchset.Set{}, "", capability.ApprovalBinding{}, fmt.Errorf("decode approved touchset: %w", err)
	}
	approved.Globs, err = touchset.CanonicalGlobs(approved.Globs)
	if err != nil || len(approved.Globs) == 0 {
		return touchset.Set{}, "", capability.ApprovalBinding{}, fmt.Errorf("approved touchset is empty or unsafe")
	}
	return approved, touchsetDigest, capability.ApprovalBinding{
		Approved: true, TouchsetDigest: touchsetDigest, DecisionID: strconv.FormatInt(approval.ID, 10),
		ArtifactID: target.ArtifactVersion, AttemptID: attemptID,
	}, nil
}

func capabilityOutputOwnership(stage flow.Stage) ([]capability.RequiredOutput, []string, error) {
	outputs := make([]capability.RequiredOutput, 0, len(stage.Artifacts))
	var agent []string
	for _, path := range stage.Artifacts {
		canonical, err := touchset.CanonicalPath(path)
		if err != nil {
			return nil, nil, fmt.Errorf("stage output %q is unsafe: %w", path, err)
		}
		owner := capability.OwnerAgent
		if canonical == "verification.json" {
			owner = capability.OwnerEngine
		} else {
			agent = append(agent, canonical)
		}
		outputs = append(outputs, capability.RequiredOutput{Path: canonical, Owner: owner})
	}
	sort.Slice(outputs, func(i, j int) bool { return outputs[i].Path < outputs[j].Path })
	sort.Strings(agent)
	return outputs, agent, nil
}

func stageOutputOwnership(stage flow.Stage, fallback []string) (agent, engine []string) {
	for _, output := range stage.Artifacts {
		if output == "verification.json" {
			engine = append(engine, output)
		} else {
			agent = append(agent, output)
		}
	}
	if len(stage.Artifacts) == 0 {
		agent = append(agent, fallback...)
	}
	if stage.MergeBarrier {
		engine = append(engine, "verification attempts, lifecycle state, receipts, and integration evidence")
	}
	return agent, engine
}

func enumerateCapabilityRepository(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" || rel == ".watchtower" || strings.HasPrefix(rel, ".git/") || strings.HasPrefix(rel, ".watchtower/") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		base := strings.ToLower(entry.Name())
		if strings.HasPrefix(base, ".env") || strings.Contains(base, "credential") || strings.Contains(base, "token") || strings.HasSuffix(base, ".sock") {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			resolved, resolveErr := filepath.EvalSymlinks(path)
			if resolveErr != nil {
				return fmt.Errorf("resolve repository link %q: %w", rel, resolveErr)
			}
			inside, insideErr := filepath.Rel(root, resolved)
			if insideErr != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
				return fmt.Errorf("repository link %q escapes workspace", rel)
			}
		}
		canonical, canonicalErr := touchset.CanonicalPath(rel)
		if canonicalErr != nil {
			return fmt.Errorf("repository path %q is unsafe: %w", rel, canonicalErr)
		}
		paths = append(paths, canonical)
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func canonicalExactPaths(paths []string) ([]string, error) {
	seen := make(map[string]bool, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		canonical, err := touchset.CanonicalPath(path)
		if err != nil {
			return nil, fmt.Errorf("capability path %q is unsafe: %w", path, err)
		}
		if !seen[canonical] {
			seen[canonical] = true
			result = append(result, canonical)
		}
	}
	sort.Strings(result)
	return result, nil
}

func matchesAny(globs []string, path string) bool {
	for _, glob := range globs {
		if matched, err := touchset.Match(glob, path); err == nil && matched {
			return true
		}
	}
	return false
}

func stageLegacyRestrictions(stageRunner runner.Runner, agents []flow.AgentRef) pkgs.LegacyRestrictions {
	source, ok := stageRunner.(runner.LegacyRestrictionSource)
	if !ok {
		return pkgs.LegacyRestrictions{}
	}
	var result pkgs.LegacyRestrictions
	for _, agent := range agents {
		restriction, found := source.LegacyRestrictions(agent.Package)
		if !found || !restriction.Declared {
			continue
		}
		result.Declared = true
		result.DenyWorkspaceRead = result.DenyWorkspaceRead || restriction.DenyWorkspaceRead
		result.DenyWorkspaceMutate = result.DenyWorkspaceMutate || restriction.DenyWorkspaceMutate
		result.DenyLocalProcess = result.DenyLocalProcess || restriction.DenyLocalProcess
	}
	return result
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
