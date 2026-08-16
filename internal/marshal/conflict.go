package marshal

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/touchset"
)

// ConflictState is the bounded rebase state exposed to the engine. It contains
// identities and canonical conflicted paths, never a general Git callback.
type ConflictState struct {
	BaseSHA string
	Paths   []string
	Done    bool
}

// ConflictController owns start/continue/abort for one exact issue worktree.
type ConflictController struct {
	worktree string
	baseSHA  string
}

func NewConflictController(worktree string) (*ConflictController, error) {
	resolved, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return nil, fmt.Errorf("resolve conflict worktree: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, err
	}
	if _, err := conflictGit(resolved, "rev-parse", "--show-toplevel"); err != nil {
		return nil, err
	}
	return &ConflictController{worktree: resolved}, nil
}

func (c *ConflictController) Start(baseSHA string) (ConflictState, error) {
	if c.baseSHA != "" || strings.TrimSpace(baseSHA) == "" {
		return ConflictState{}, fmt.Errorf("conflict rebase start identity is invalid")
	}
	canonical, err := conflictGit(c.worktree, "rev-parse", "--verify", baseSHA+"^{commit}")
	if err != nil {
		return ConflictState{}, fmt.Errorf("resolve conflict base: %w", err)
	}
	c.baseSHA = strings.TrimSpace(canonical)
	_, runErr := conflictGit(c.worktree, "rebase", c.baseSHA)
	state, stateErr := c.Current()
	if stateErr != nil {
		return ConflictState{}, stateErr
	}
	if runErr != nil && state.Done {
		return ConflictState{}, runErr
	}
	return state, nil
}

func (c *ConflictController) Current() (ConflictState, error) {
	if c.baseSHA == "" {
		return ConflictState{}, fmt.Errorf("conflict rebase has not started")
	}
	paths, err := currentConflictPaths(c.worktree)
	if err != nil {
		return ConflictState{}, err
	}
	if len(paths) > 0 {
		return ConflictState{BaseSHA: c.baseSHA, Paths: paths}, nil
	}
	active, err := rebaseActive(c.worktree)
	if err != nil {
		return ConflictState{}, err
	}
	return ConflictState{BaseSHA: c.baseSHA, Done: !active}, nil
}

func (c *ConflictController) Continue(paths []string) (ConflictState, error) {
	current, err := c.Current()
	if err != nil {
		return ConflictState{}, err
	}
	canonical, err := canonicalConflictPaths(paths)
	if err != nil {
		return ConflictState{}, err
	}
	if current.Done || !equalStrings(current.Paths, canonical) {
		return ConflictState{}, fmt.Errorf("resolved paths do not match current conflict set")
	}
	args := append([]string{"add", "--"}, canonical...)
	if _, err := conflictGit(c.worktree, args...); err != nil {
		return ConflictState{}, fmt.Errorf("stage resolved conflict paths: %w", err)
	}
	_, runErr := conflictGit(c.worktree, "-c", "core.editor=true", "rebase", "--continue")
	next, stateErr := c.Current()
	if stateErr != nil {
		return ConflictState{}, stateErr
	}
	if runErr != nil && next.Done {
		return ConflictState{}, runErr
	}
	return next, nil
}

func (c *ConflictController) Abort() error {
	if c.baseSHA == "" {
		return nil
	}
	active, err := rebaseActive(c.worktree)
	if err != nil || !active {
		return err
	}
	if _, err := conflictGit(c.worktree, "rebase", "--abort"); err != nil {
		return fmt.Errorf("abort conflict rebase: %w", err)
	}
	return nil
}

func currentConflictPaths(worktree string) ([]string, error) {
	cmd := conflictCommand(worktree, "diff", "--name-only", "--diff-filter=U", "-z")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read current conflicts: %w", err)
	}
	var paths []string
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		canonical, err := touchset.CanonicalPath(filepath.ToSlash(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("unsafe conflict path: %w", err)
		}
		paths = append(paths, canonical)
	}
	sort.Strings(paths)
	return paths, nil
}

func canonicalConflictPaths(paths []string) ([]string, error) {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		canonical, err := touchset.CanonicalPath(path)
		if err != nil {
			return nil, err
		}
		result = append(result, canonical)
	}
	sort.Strings(result)
	for index := 1; index < len(result); index++ {
		if result[index] == result[index-1] {
			return nil, fmt.Errorf("duplicate conflict path %q", result[index])
		}
	}
	return result, nil
}

func rebaseActive(worktree string) (bool, error) {
	gitDir, err := conflictGit(worktree, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return false, err
	}
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		path := filepath.Join(strings.TrimSpace(gitDir), name)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return true, nil
		} else if err != nil && !os.IsNotExist(err) {
			return false, err
		}
	}
	return false, nil
}

func conflictGit(worktree string, args ...string) (string, error) {
	output, err := conflictCommand(worktree, args...).CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(output)), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func conflictCommand(worktree string, args ...string) *exec.Cmd {
	secure := []string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "credential.helper="}
	cmd := exec.Command("git", append(append([]string{"-C", worktree}, secure...), args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/usr/bin/false", "SSH_ASKPASS=/usr/bin/false")
	return cmd
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
