package pkgs

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Package is an agent package: a system prompt plus CLI options,
// loaded from a directory containing package.yaml and prompt.md.
type Package struct {
	Name         string   `yaml:"-"`
	Prompt       string   `yaml:"-"`
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
		p.Prompt = string(prompt)
		out[p.Name] = p
	}
	return out, nil
}
