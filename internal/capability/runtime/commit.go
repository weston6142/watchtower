package runtime

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/touchset"
)

func (s *Session) VCSRead(ctx context.Context, args ...string) ([]byte, error) {
	if !s.hasOperation(capability.OpVCSRead) || len(args) == 0 || !safeVCSReadArgs(args) {
		return nil, s.deny(capability.OpVCSRead)
	}
	safeArgs := append([]string(nil), args...)
	switch args[0] {
	case "diff", "log", "show":
		safeArgs = append([]string{args[0], "--no-ext-diff", "--no-textconv"}, args[1:]...)
	}
	return s.git(ctx, nil, safeArgs...)
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
	observedPaths, err := s.changedPaths(ctx)
	if err != nil || len(observedPaths) == 0 {
		return "", s.deny(capability.OpVCSCommit)
	}
	paths := make([]string, 0, len(observedPaths))
	for _, path := range observedPaths {
		switch {
		case pathAllowedForCommit(s.contract.Contract.Writes, path):
			paths = append(paths, path)
		case capability.IsWorkflowArtifact(path), pathMatches(s.contract.Contract.Reads, path):
			continue
		default:
			return "", s.deny(capability.OpVCSCommit, path)
		}
	}
	if len(paths) == 0 {
		return "", s.deny(capability.OpVCSCommit)
	}
	index := filepath.Join(s.agentScratch, "commit-index")
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
	environment := []string{
		"PATH=/usr/bin:/bin", "HOME=" + s.agentScratch, "TMPDIR=" + s.agentScratch,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat", "PAGER=cat", "GIT_OPTIONAL_LOCKS=0",
	}
	process, err := runner.StartProcessTree(ctx, runner.ProcessSpec{
		Path: gitPath, Args: append([]string{"-C", s.worktree}, args...), Dir: s.worktree,
		Env: append(environment, extraEnv...), Stdout: &output, Stderr: &output,
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

func safeVCSReadArgs(args []string) bool {
	switch args[0] {
	case "status", "diff", "log", "show", "rev-parse", "ls-files":
	default:
		return false
	}
	for _, argument := range args[1:] {
		if strings.ContainsAny(argument, "\x00\r\n") || argument == "--ext-diff" || argument == "--textconv" ||
			argument == "--output" || strings.HasPrefix(argument, "--output=") {
			return false
		}
	}
	return true
}

func pathAllowedForCommit(grants []capability.PathGrant, path string) bool {
	for _, mutation := range []capability.MutationClass{capability.MutationCreate, capability.MutationModify, capability.MutationDelete, capability.MutationRename} {
		if grantAllows(grants, path, mutation) {
			return true
		}
	}
	return false
}
