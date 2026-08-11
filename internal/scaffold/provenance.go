package scaffold

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ProvenanceFile        = "provenance.yaml"
	ManifestSchemaVersion = 1
	DefaultsVersion       = "3"
)

type Manifest struct {
	SchemaVersion   int             `yaml:"schema_version"`
	DefaultsVersion string          `yaml:"defaults_version"`
	Files           []ManifestEntry `yaml:"files"`
}

type ManifestEntry struct {
	Path            string `yaml:"path"`
	SHA256          string `yaml:"sha256"`
	BaselineVersion string `yaml:"baseline_version"`
}

// ManagedFiles enumerates regular embedded default files in normalized order.
func ManagedFiles(source fs.FS) []string {
	var files []string
	_ = fs.WalkDir(source, "defaults", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		clean := path.Clean(name)
		rel := strings.TrimPrefix(clean, "defaults/")
		if rel == clean || rel == "" || rel == ProvenanceFile {
			return nil
		}
		files = append(files, path.Clean(rel))
		return nil
	})
	slices.Sort(files)
	return files
}

func SHA256Bytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func SHA256File(name string) (string, error) {
	body, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return SHA256Bytes(body), nil
}

// BuildManifest hashes the selected files under repoRoot/.watchtower without
// writing anything. Paths must be normalized relative managed paths.
func BuildManifest(repoRoot string, paths []string, defaultsVersion string) (Manifest, error) {
	entries := make([]ManifestEntry, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, file := range paths {
		normalized, err := validateManifestPath(file)
		if err != nil {
			return Manifest{}, err
		}
		if _, ok := seen[normalized]; ok {
			return Manifest{}, fmt.Errorf("duplicate manifest path %q", file)
		}
		seen[normalized] = struct{}{}
		hash, err := SHA256File(filepath.Join(repoRoot, ".watchtower", filepath.FromSlash(normalized)))
		if err != nil {
			return Manifest{}, fmt.Errorf("hash %s: %w", normalized, err)
		}
		entries = append(entries, ManifestEntry{
			Path: normalized, SHA256: hash, BaselineVersion: defaultsVersion,
		})
	}
	slices.SortFunc(entries, func(left, right ManifestEntry) int {
		return strings.Compare(left.Path, right.Path)
	})
	return Manifest{
		SchemaVersion: ManifestSchemaVersion, DefaultsVersion: defaultsVersion, Files: entries,
	}, nil
}

func WriteManifest(name string, manifest Manifest) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	body, err := yaml.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	return os.WriteFile(name, body, 0o644)
}

func ReadManifest(name string) (Manifest, error) {
	body, err := os.ReadFile(name)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := yaml.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func validateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("unsupported manifest schema version %d", manifest.SchemaVersion)
	}
	if manifest.DefaultsVersion == "" {
		return fmt.Errorf("manifest defaults version is empty")
	}
	previous := ""
	for index, entry := range manifest.Files {
		if _, err := validateManifestPath(entry.Path); err != nil {
			return err
		}
		if index > 0 && entry.Path <= previous {
			return fmt.Errorf("manifest paths are not unique and sorted")
		}
		previous = entry.Path
		if len(entry.SHA256) != sha256.Size*2 {
			return fmt.Errorf("manifest hash for %s is not a 64-character hexadecimal value", entry.Path)
		}
		if _, err := hex.DecodeString(entry.SHA256); err != nil {
			return fmt.Errorf("manifest hash for %s is invalid: %w", entry.Path, err)
		}
		if entry.BaselineVersion == "" {
			return fmt.Errorf("manifest baseline version for %s is empty", entry.Path)
		}
	}
	return nil
}

func validateManifestPath(file string) (string, error) {
	if file == "" || filepath.IsAbs(file) || strings.Contains(file, `\`) {
		return "", fmt.Errorf("invalid manifest path %q", file)
	}
	normalized := path.Clean(file)
	if normalized == "." || normalized == ".." || normalized != file || normalized == ProvenanceFile || strings.HasPrefix(normalized, "../") {
		return "", fmt.Errorf("invalid manifest path %q", file)
	}
	return normalized, nil
}
