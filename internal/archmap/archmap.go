package archmap

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Module struct {
	Name  string `json:"name"`
	Files int    `json:"files"`
}

type Overlay struct {
	IssueID string   `json:"issue_id"`
	Globs   []string `json:"globs"`
}

type Map struct {
	Modules  []Module  `json:"modules"`
	Overlays []Overlay `json:"overlays"`
}

func skippedDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == ".worktrees" || name == "node_modules" || name == "vendor"
}

func countFiles(root string) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && skippedDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		return nil
	})
	return count, err
}

// Scan returns repository modules and recursive file counts.
func Scan(repo string) ([]Module, error) {
	entries, err := os.ReadDir(repo)
	if err != nil {
		return nil, err
	}
	var modules []Module
	depthTwo := map[string]bool{"cmd": true, "internal": true, "pkg": true, "src": true}
	for _, entry := range entries {
		if !entry.IsDir() || skippedDir(entry.Name()) {
			continue
		}
		if depthTwo[entry.Name()] {
			children, err := os.ReadDir(filepath.Join(repo, entry.Name()))
			if err != nil {
				return nil, err
			}
			for _, child := range children {
				if !child.IsDir() || skippedDir(child.Name()) {
					continue
				}
				path := filepath.Join(repo, entry.Name(), child.Name())
				files, err := countFiles(path)
				if err != nil {
					return nil, err
				}
				modules = append(modules, Module{Name: filepath.ToSlash(filepath.Join(entry.Name(), child.Name())), Files: files})
			}
			continue
		}
		path := filepath.Join(repo, entry.Name())
		files, err := countFiles(path)
		if err != nil {
			return nil, err
		}
		modules = append(modules, Module{Name: entry.Name(), Files: files})
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Name < modules[j].Name })
	return modules, nil
}
