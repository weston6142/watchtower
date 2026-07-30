package pkgs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Package is an agent package: a system prompt plus CLI options,
// loaded from a directory containing package.yaml and prompt.md.
type Package struct {
	Name         string   `yaml:"-"`
	Prompt       string   `yaml:"-"`
	Includes     []string `yaml:"includes"`
	AllowedTools []string `yaml:"allowed_tools"`
	Model        string   `yaml:"model"`
	Effort       string   `yaml:"effort"` // low|medium|high; empty = CLI default
	MaxTurns     int      `yaml:"max_turns"`
}

// LoadDir loads all agent packages under root, keyed by directory name.
func LoadDir(root string) (map[string]Package, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := map[string]Package{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		cfgPath := filepath.Join(dir, "package.yaml")
		promptPath := filepath.Join(dir, "prompt.md")
		cfg, err := os.ReadFile(cfgPath)
		if err != nil {
			// Directories without a readable package.yaml are not packages; skip them.
			continue
		}
		prompt, err := os.ReadFile(promptPath)
		if err != nil {
			return nil, fmt.Errorf("package %s has package.yaml but no prompt.md", e.Name())
		}
		var p Package
		if err := yaml.Unmarshal(cfg, &p); err != nil {
			return nil, fmt.Errorf("package %s: %w", e.Name(), err)
		}
		p.Name = e.Name()
		p.Prompt, err = composePrompt(filepath.Join(filepath.Dir(root), "shared"), p, string(prompt))
		if err != nil {
			return nil, fmt.Errorf("package %s: %w", e.Name(), err)
		}
		out[p.Name] = p
	}
	return out, nil
}

func composePrompt(sharedRoot string, p Package, prompt string) (string, error) {
	var out strings.Builder
	seen := map[string]bool{}
	var evaluatedRoot string
	for _, name := range p.Includes {
		if name == "" || filepath.IsAbs(name) || filepath.Base(name) != name ||
			strings.ContainsAny(name, `/\`) {
			return "", fmt.Errorf("unsafe include name %q", name)
		}
		if seen[name] {
			return "", fmt.Errorf("duplicate include %q", name)
		}
		seen[name] = true
		if evaluatedRoot == "" {
			var err error
			evaluatedRoot, err = filepath.EvalSymlinks(sharedRoot)
			if err != nil {
				return "", fmt.Errorf("shared directory: %w", err)
			}
		}
		path, err := filepath.EvalSymlinks(filepath.Join(sharedRoot, name+".md"))
		if err != nil {
			return "", fmt.Errorf("include %q: %w", name, err)
		}
		rel, err := filepath.Rel(evaluatedRoot, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("include %q escapes shared directory", name)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("include %q: %w", name, err)
		}
		fmt.Fprintf(&out, "# Shared include: %s\n\n%s\n\n", name, strings.TrimSpace(string(body)))
	}
	fmt.Fprintf(&out, "# Package prompt: %s\n\n%s", p.Name, prompt)
	return out.String(), nil
}
