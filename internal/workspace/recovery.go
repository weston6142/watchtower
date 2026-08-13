package workspace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RecoveryRequest binds destructive recovery to one rejected issue worktree,
// one exact ref transition, and one trusted capability baseline.
type RecoveryRequest struct {
	IssueID             string
	RejectedPath        string
	Provider            string
	Repository          string
	Branch              string
	TrustedCommit       string
	TrustedTree         string
	ObservedCommit      string
	ObservedRef         string
	CapabilityAttemptID string
}

// Recoverer reconstructs a rejected issue workspace without consuming any
// bytes from it. The returned release callback owns only the replacement.
type Recoverer interface {
	Recover(RecoveryRequest) (path string, release func() error, err error)
}

type RecoveryProvider interface {
	Provider
	Recoverer
	RepositoryRoot() string
}

func (g GitWorktree) RepositoryRoot() string { return g.Repo }
func (t Treehouse) RepositoryRoot() string   { return t.Repo }

func (g GitWorktree) Recover(request RecoveryRequest) (string, func() error, error) {
	repo, rejected, ref, err := validateRecoveryRequest(g.Repo, g.Name(), request)
	if err != nil {
		return "", nil, err
	}
	expected := filepath.Join(repo, ".worktrees", request.IssueID)
	if rejected != expected {
		return "", nil, recoveryRefusal("rejected path is not the exact issue worktree")
	}
	return recoverWorkspace(request, repo, rejected, ref, g.ReleasePath, func() (string, func() error, error) {
		output, addErr := gitOutput(repo, "worktree", "add", rejected, request.Branch)
		if addErr != nil {
			return "", nil, fmt.Errorf("reacquire recovered worktree: %w: %s", addErr, output)
		}
		return rejected, func() error { return g.ReleasePath(rejected) }, nil
	})
}

func (t Treehouse) Recover(request RecoveryRequest) (string, func() error, error) {
	repo, rejected, ref, err := validateRecoveryRequest(t.Repo, t.Name(), request)
	if err != nil {
		return "", nil, err
	}
	if rejected == repo || !registeredWorktree(repo, rejected) {
		return "", nil, recoveryRefusal("rejected path is not an exact registered issue worktree")
	}
	return recoverWorkspace(request, repo, rejected, ref, t.ReleasePath, func() (string, func() error, error) {
		return t.Acquire(request.IssueID)
	})
}

func recoverWorkspace(
	request RecoveryRequest,
	repo, rejected, ref string,
	releaseRejected func(string) error,
	reacquire func() (string, func() error, error),
) (string, func() error, error) {
	currentRef, err := gitRequired(repo, "rev-parse", "--verify", ref)
	if err != nil {
		return "", nil, recoveryRefusal("issue ref is unavailable")
	}
	if currentRef != request.ObservedRef && currentRef != request.TrustedCommit {
		return "", nil, recoveryRefusal("issue ref changed concurrently")
	}
	if currentRef == request.TrustedCommit {
		if pathReadyForReplay(rejected, request, repo) {
			return rejected, func() error { return releaseRejected(rejected) }, nil
		}
		if _, statErr := os.Lstat(rejected); statErr == nil {
			if err := validateRejectedWorkspace(rejected, request, repo); err != nil {
				return "", nil, err
			}
			if err := releaseRejected(rejected); err != nil {
				return "", nil, fmt.Errorf("release rejected workspace: %w", err)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", nil, recoveryRefusal("trusted ref has an ambiguous workspace")
		}
	} else {
		if err := validateRejectedWorkspace(rejected, request, repo); err != nil {
			return "", nil, err
		}
		if err := releaseRejected(rejected); err != nil {
			return "", nil, fmt.Errorf("release rejected workspace: %w", err)
		}
		if _, err := gitOutput(repo, "update-ref", ref, request.TrustedCommit, request.ObservedRef); err != nil {
			return "", nil, recoveryRefusal("issue ref compare-and-swap failed")
		}
	}

	path, release, err := reacquire()
	if err != nil {
		return "", nil, err
	}
	canonicalPath, err := canonicalExistingPath(path)
	if err != nil {
		_ = release()
		return "", nil, recoveryRefusal("replacement workspace identity is unavailable")
	}
	if err := validateTrustedWorkspace(canonicalPath, request, repo); err != nil {
		_ = release()
		return "", nil, err
	}
	return canonicalPath, release, nil
}

func validateRecoveryRequest(repoPath, provider string, request RecoveryRequest) (string, string, string, error) {
	if request.IssueID == "" || request.CapabilityAttemptID == "" || request.Provider != provider ||
		request.Branch != "issue/"+request.IssueID || request.TrustedCommit == "" || request.TrustedTree == "" ||
		request.ObservedCommit == "" || request.ObservedRef == "" || request.ObservedCommit != request.ObservedRef {
		return "", "", "", recoveryRefusal("recovery identity is incomplete")
	}
	repo, err := canonicalExistingPath(repoPath)
	if err != nil {
		return "", "", "", recoveryRefusal("configured repository identity is unavailable")
	}
	requestedRepo, err := canonicalExistingPath(request.Repository)
	if err != nil || requestedRepo != repo {
		return "", "", "", recoveryRefusal("repository identity mismatch")
	}
	rejected, err := canonicalRecoveryTarget(request.RejectedPath)
	if err != nil || rejected == repo || !isBeneath(filepath.Dir(repo), rejected) {
		return "", "", "", recoveryRefusal("recovery target is broad or ambiguous")
	}
	trustedTree, err := gitRequired(repo, "rev-parse", request.TrustedCommit+"^{tree}")
	if err != nil || trustedTree != request.TrustedTree {
		return "", "", "", recoveryRefusal("trusted baseline identity mismatch")
	}
	return repo, rejected, "refs/heads/" + request.Branch, nil
}

func validateRejectedWorkspace(path string, request RecoveryRequest, repo string) error {
	if err := rejectSymlinkTarget(path); err != nil {
		return err
	}
	if !registeredWorktree(repo, path) {
		return recoveryRefusal("rejected workspace is not registered")
	}
	if !sameCommonGitDir(repo, path) {
		return recoveryRefusal("rejected workspace belongs to another repository")
	}
	branch, err := gitRequired(path, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch != request.Branch {
		return recoveryRefusal("rejected workspace branch mismatch")
	}
	head, err := gitRequired(path, "rev-parse", "HEAD")
	if err != nil || head != request.ObservedCommit || head != request.ObservedRef {
		return recoveryRefusal("rejected workspace commit mismatch")
	}
	return nil
}

func validateTrustedWorkspace(path string, request RecoveryRequest, repo string) error {
	if err := rejectSymlinkTarget(path); err != nil {
		return err
	}
	if !sameCommonGitDir(repo, path) {
		return recoveryRefusal("replacement workspace belongs to another repository")
	}
	branch, err := gitRequired(path, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch != request.Branch {
		return recoveryRefusal("replacement branch mismatch")
	}
	head, err := gitRequired(path, "rev-parse", "HEAD")
	if err != nil || head != request.TrustedCommit {
		return recoveryRefusal("replacement commit mismatch")
	}
	tree, err := gitRequired(path, "rev-parse", "HEAD^{tree}")
	if err != nil || tree != request.TrustedTree {
		return recoveryRefusal("replacement tree mismatch")
	}
	status, err := gitRequired(path, "status", "--porcelain", "--untracked-files=all")
	if err != nil || status != "" {
		return recoveryRefusal("replacement workspace is not clean")
	}
	return nil
}

func pathReadyForReplay(path string, request RecoveryRequest, repo string) bool {
	return validateTrustedWorkspace(path, request, repo) == nil
}

func rejectSymlinkTarget(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return recoveryRefusal("workspace target is unavailable")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return recoveryRefusal("workspace target is not a real directory")
	}
	return nil
}

func canonicalExistingPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(filepath.Clean(abs))
}

func canonicalRecoveryTarget(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if info, statErr := os.Lstat(abs); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", recoveryRefusal("recovery target is a symlink")
		}
		return filepath.EvalSymlinks(abs)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func isBeneath(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func registeredWorktree(repo, path string) bool {
	output, err := gitOutput(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			candidate, candidateErr := canonicalExistingPath(strings.TrimPrefix(line, "worktree "))
			if candidateErr == nil && candidate == path {
				return true
			}
		}
	}
	return false
}

func sameCommonGitDir(repo, worktree string) bool {
	want, err := canonicalExistingPath(filepath.Join(repo, ".git"))
	if err != nil {
		return false
	}
	got, err := gitRequired(worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false
	}
	got, err = canonicalExistingPath(got)
	return err == nil && got == want
}

func gitRequired(root string, args ...string) (string, error) {
	output, err := gitOutput(root, args...)
	return strings.TrimSpace(output), err
}

func gitOutput(root string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func recoveryRefusal(message string) error {
	return fmt.Errorf("workspace recovery refused: %s", message)
}
