package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/marshal"
)

type shelvedWorkflowInput struct {
	source string
	stored string
}

// replayWithoutWorkflowInputs runs repository-owned checks against the product
// tree, not the issue briefing and artifacts Watchtower materializes beside it.
func replayWithoutWorkflowInputs(
	ctx context.Context, workdir string, commands [][]string, paths []string,
) error {
	normalized, err := normalizeWorkflowInputPaths(paths)
	if err != nil {
		return err
	}
	shelf, err := os.MkdirTemp(filepath.Dir(workdir), ".watchtower-verification-*")
	if err != nil {
		return fmt.Errorf("create verification shelf: %w", err)
	}
	var moved []shelvedWorkflowInput
	restore := func() error {
		var restoreErr error
		for index := len(moved) - 1; index >= 0; index-- {
			item := moved[index]
			if _, statErr := os.Lstat(item.source); statErr == nil {
				restoreErr = errors.Join(restoreErr,
					fmt.Errorf("restore workflow input %s: path was recreated during verification", item.source))
				continue
			} else if !os.IsNotExist(statErr) {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("restore workflow input %s: %w", item.source, statErr))
				continue
			}
			if mkdirErr := os.MkdirAll(filepath.Dir(item.source), 0o755); mkdirErr != nil {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("restore workflow input %s: %w", item.source, mkdirErr))
				continue
			}
			if renameErr := os.Rename(item.stored, item.source); renameErr != nil {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("restore workflow input %s: %w", item.source, renameErr))
			}
		}
		if restoreErr == nil {
			if removeErr := os.RemoveAll(shelf); removeErr != nil {
				restoreErr = fmt.Errorf("remove verification shelf %s: %w", shelf, removeErr)
			}
		}
		return restoreErr
	}

	for _, relative := range normalized {
		source := filepath.Join(workdir, relative)
		if _, statErr := os.Lstat(source); os.IsNotExist(statErr) {
			continue
		} else if statErr != nil {
			return errors.Join(fmt.Errorf("shelve workflow input %s: %w", relative, statErr), restore())
		}
		stored := filepath.Join(shelf, relative)
		if err := os.MkdirAll(filepath.Dir(stored), 0o755); err != nil {
			return errors.Join(fmt.Errorf("shelve workflow input %s: %w", relative, err), restore())
		}
		if err := os.Rename(source, stored); err != nil {
			return errors.Join(fmt.Errorf("shelve workflow input %s: %w", relative, err), restore())
		}
		moved = append(moved, shelvedWorkflowInput{source: source, stored: stored})
	}

	replayErr := marshal.Replay(ctx, workdir, commands)
	return errors.Join(replayErr, restore())
}

func normalizeWorkflowInputPaths(paths []string) ([]string, error) {
	unique := make(map[string]bool, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(filepath.FromSlash(path))
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("workflow input path %q must stay within the worktree", path)
		}
		unique[clean] = true
	}
	ordered := make([]string, 0, len(unique))
	for path := range unique {
		ordered = append(ordered, path)
	}
	sort.Slice(ordered, func(i, j int) bool {
		leftDepth := strings.Count(ordered[i], string(filepath.Separator))
		rightDepth := strings.Count(ordered[j], string(filepath.Separator))
		if leftDepth == rightDepth {
			return ordered[i] < ordered[j]
		}
		return leftDepth < rightDepth
	})
	result := make([]string, 0, len(ordered))
	for _, candidate := range ordered {
		covered := false
		for _, parent := range result {
			if strings.HasPrefix(candidate, parent+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			result = append(result, candidate)
		}
	}
	return result, nil
}
