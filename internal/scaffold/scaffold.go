// Package scaffold writes the embedded default .watchtower tree into a repo.
package scaffold

import (
	"embed"
	"os"
	"path/filepath"
	"slices"
)

//go:embed all:defaults
var defaults embed.FS

// Init copies the embedded defaults into <repoRoot>/.watchtower/. It never
// overwrites: existing files are reported in skipped instead.
func Init(repoRoot string) (created, skipped []string, err error) {
	dst := filepath.Join(repoRoot, ".watchtower")
	paths := ManagedFiles(defaults)
	for _, rel := range paths {
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return created, skipped, err
		}
		if _, statErr := os.Lstat(target); statErr == nil {
			skipped = append(skipped, rel)
			continue
		} else if !os.IsNotExist(statErr) {
			return created, skipped, statErr
		}
		body, readErr := defaults.ReadFile(filepath.ToSlash(filepath.Join("defaults", rel)))
		if readErr != nil {
			return created, skipped, readErr
		}
		if writeErr := os.WriteFile(target, body, 0o644); writeErr != nil {
			return created, skipped, writeErr
		}
		created = append(created, rel)
	}
	slices.Sort(created)
	slices.Sort(skipped)
	manifestPath := filepath.Join(dst, ProvenanceFile)
	if len(created) > 0 {
		if _, statErr := os.Lstat(manifestPath); os.IsNotExist(statErr) {
			manifest, buildErr := BuildManifest(repoRoot, created, DefaultsVersion)
			if buildErr != nil {
				return created, skipped, buildErr
			}
			if writeErr := WriteManifest(manifestPath, manifest); writeErr != nil {
				return created, skipped, writeErr
			}
		} else if statErr != nil {
			return created, skipped, statErr
		}
	}
	return created, skipped, nil
}
