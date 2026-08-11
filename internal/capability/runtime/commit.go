package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/touchset"
)

func (s *Session) VCSRead(ctx context.Context, args ...string) ([]byte, error) {
	if !s.hasOperation(capability.OpVCSRead) || len(args) == 0 || !allowedVCSRead(args[0]) {
		return nil, s.deny(capability.OpVCSRead)
	}
	return s.git(ctx, nil, args...)
}

func (s *Session) Commit(ctx context.Context, message string) (string, error) {
	if !s.hasOperation(capability.OpVCSCommit) || strings.TrimSpace(message) == "" {
		return "", s.deny(capability.OpVCSCommit)
	}
	if err := s.ensureOpen(); err != nil {
		return "", err
	}
	branch := s.contract.Contract.Repository.Branch
	start := s.contract.Contract.Repository.StartCommit
	if branch == "" || start == "" || strings.ContainsAny(branch, "~^:\\ ") {
		return "", s.deny(capability.OpVCSCommit)
	}
	head, err := s.gitText(ctx, nil, "rev-parse", "HEAD")
	if err != nil || head != start {
		return "", s.deny(capability.OpVCSCommit)
	}
	paths, err := s.changedPaths(ctx)
	if err != nil || len(paths) == 0 {
		return "", s.deny(capability.OpVCSCommit)
	}
	for _, path := range paths {
		if workflowArtifact(path) || !pathAllowedForCommit(s.contract.Contract.Writes, path) {
			return "", s.deny(capability.OpVCSCommit, path)
		}
	}
	index := filepath.Join(s.scratch, "commit-index")
	environment := []string{"GIT_INDEX_FILE=" + index, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
	if _, err := s.git(ctx, environment, "read-tree", start); err != nil {
		return "", err
	}
	addArgs := []string{"add", "-A", "--"}
	addArgs = append(addArgs, paths...)
	if _, err := s.git(ctx, environment, addArgs...); err != nil {
		return "", err
	}
	tree, err := s.gitText(ctx, environment, "write-tree")
	if err != nil {
		return "", err
	}
	commit, err := s.gitText(ctx, environment, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgSign=false", "commit-tree", tree, "-p", start, "-m", message)
	if err != nil {
		return "", err
	}
	ref := "refs/heads/" + branch
	if _, err := s.git(ctx, nil, "update-ref", ref, commit, start); err != nil {
		return "", err
	}
	if _, err := s.git(ctx, nil, "reset", "--mixed", commit); err != nil {
		return "", err
	}
	parent, err := s.gitText(ctx, nil, "rev-parse", commit+"^")
	if err != nil || parent != start {
		return "", s.deny(capability.OpVCSCommit)
	}
	s.record("runtime", "passed", "", capability.OpVCSCommit, paths)
	return commit, nil
}

func (s *Session) changedPaths(ctx context.Context) ([]string, error) {
	body, err := s.git(ctx, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	fields := bytes.Split(body, []byte{0})
	for index := 0; index < len(fields); index++ {
		entry := string(fields[index])
		if len(entry) < 4 {
			continue
		}
		path := filepath.ToSlash(entry[3:])
		if entry[0] == 'R' || entry[1] == 'R' {
			index++
			if index < len(fields) {
				seen[filepath.ToSlash(string(fields[index]))] = true
			}
		}
		seen[path] = true
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		canonical, canonicalErr := touchset.CanonicalPath(path)
		if canonicalErr != nil {
			return nil, canonicalErr
		}
		paths = append(paths, canonical)
	}
	sort.Strings(paths)
	return paths, nil
}

func (s *Session) git(ctx context.Context, extraEnv []string, args ...string) ([]byte, error) {
	gitPath, err := resolveExecutable("git", []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	process, err := runner.StartProcessTree(ctx, runner.ProcessSpec{
		Path: gitPath, Args: append([]string{"-C", s.worktree}, args...), Dir: s.worktree,
		Env: append(scrubEnvironment(os.Environ()), extraEnv...), Stdout: &output, Stderr: &output,
	})
	if err != nil {
		return nil, err
	}
	if err := process.Wait(); err != nil {
		return nil, fmt.Errorf("mediated git operation failed: %w", err)
	}
	return output.Bytes(), nil
}

func (s *Session) gitText(ctx context.Context, environment []string, args ...string) (string, error) {
	body, err := s.git(ctx, environment, args...)
	return strings.TrimSpace(string(body)), err
}

func allowedVCSRead(operation string) bool {
	switch operation {
	case "status", "diff", "log", "show", "rev-parse", "ls-files":
		return true
	default:
		return false
	}
}

func pathAllowedForCommit(grants []capability.PathGrant, path string) bool {
	for _, mutation := range []capability.MutationClass{capability.MutationCreate, capability.MutationModify, capability.MutationDelete, capability.MutationRename} {
		if grantAllows(grants, path, mutation) {
			return true
		}
	}
	return false
}

func workflowArtifact(path string) bool {
	base := filepath.Base(path)
	switch base {
	case "ISSUE.md", "STAGE.md", "decisions.md", "brainstorm.md", "spec.md", "plan.md", "touchset.json", "verification.json":
		return true
	default:
		return strings.HasPrefix(filepath.ToSlash(path), ".watchtower/")
	}
}
