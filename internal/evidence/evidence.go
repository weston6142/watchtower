// Package evidence collects the source-control evidence emitted by worktree
// stages for gates and downstream clients.
package evidence

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wbushyeager/guildhall/internal/archmap"
)

type FileStat struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

type Bundle struct {
	Files      []FileStat     `json:"files"`
	Added      int            `json:"added"`
	Removed    int            `json:"removed"`
	Biggest    string         `json:"biggest"`
	AreaWeight map[string]int `json:"area_weight"`
}

// Collect diffs the worktree against baseRef and writes evidence.json and
// diff.patch into outDir. The diff intentionally includes uncommitted changes.
func Collect(worktree, baseRef, outDir string) (Bundle, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Bundle{}, err
	}
	numstat, err := gitOutput(worktree, "diff", "--numstat", baseRef)
	if err != nil {
		return Bundle{}, err
	}

	b := Bundle{AreaWeight: map[string]int{}}
	for _, line := range strings.Split(strings.TrimSpace(string(numstat)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 || parts[0] == "-" || parts[1] == "-" {
			continue
		}
		added, err := strconv.Atoi(parts[0])
		if err != nil {
			return Bundle{}, fmt.Errorf("parse git diff additions for %q: %w", parts[2], err)
		}
		removed, err := strconv.Atoi(parts[1])
		if err != nil {
			return Bundle{}, fmt.Errorf("parse git diff removals for %q: %w", parts[2], err)
		}
		stat := FileStat{Path: filepath.ToSlash(parts[2]), Added: added, Removed: removed}
		b.Files = append(b.Files, stat)
		b.Added += added
		b.Removed += removed
		area := archmap.AreaOf(stat.Path)
		b.AreaWeight[area] += added + removed
		if b.Biggest == "" || added+removed > biggestChange(b.Files, b.Biggest) {
			b.Biggest = stat.Path
		}
	}

	patch, err := gitOutput(worktree, "diff", baseRef)
	if err != nil {
		return Bundle{}, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "diff.patch"), patch, 0o644); err != nil {
		return Bundle{}, err
	}
	encoded, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return Bundle{}, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "evidence.json"), encoded, 0o644); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

func biggestChange(files []FileStat, path string) int {
	for _, f := range files {
		if f.Path == path {
			return f.Added + f.Removed
		}
	}
	return -1
}

func gitOutput(worktree string, args ...string) ([]byte, error) {
	cmdArgs := append([]string{"-C", worktree}, args...)
	out, err := exec.Command("git", cmdArgs...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
