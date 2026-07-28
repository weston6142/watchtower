package repocfg

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

type RegistryEntry struct {
	Path         string    `yaml:"path"`
	RegisteredAt time.Time `yaml:"registered_at"`
}

func registryDir(base string) string { return filepath.Join(base, "repos.d") }

// Register records repoRoot in the global registry. Re-registering the same
// repo overwrites its entry.
func Register(base, repoRoot string) error {
	if err := os.MkdirAll(registryDir(base), 0o755); err != nil {
		return err
	}
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return err
	}
	b, err := yaml.Marshal(RegistryEntry{Path: abs, RegisteredAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(registryDir(base), RepoID(repoRoot)+".yaml"), b, 0o644)
}

// ListRegistered returns all registered repos sorted by path.
func ListRegistered(base string) ([]RegistryEntry, error) {
	matches, err := filepath.Glob(filepath.Join(registryDir(base), "*.yaml"))
	if err != nil {
		return nil, err
	}
	var out []RegistryEntry
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		var e RegistryEntry
		if err := yaml.Unmarshal(b, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
