package scaffold

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type MigrationChange struct {
	Path   string    `json:"path"`
	Class  FileClass `json:"class"`
	Before []byte    `json:"-"`
	After  []byte    `json:"-"`
}

type MigrationPreview struct {
	Health   ConfigurationHealth `json:"health"`
	Snapshot InputSnapshot       `json:"snapshot"`
	Changes  []MigrationChange   `json:"changes"`
}

type PreparedMigration struct {
	*PreparedReplacement
	Changes  []MigrationChange
	noChange bool
}

func PreviewMigration(repoRoot string) (MigrationPreview, error) {
	health, err := Inspect(repoRoot, nil)
	if err != nil {
		return MigrationPreview{}, err
	}
	snapshot, err := CaptureInputSnapshot(repoRoot)
	if err != nil {
		return MigrationPreview{}, err
	}
	preview := MigrationPreview{Health: health, Snapshot: snapshot}
	for _, status := range health.Files {
		if status.Class != FileStale {
			continue
		}
		body, exists, err := readManagedFile(repoRoot, status.Path)
		if err != nil {
			return MigrationPreview{}, err
		}
		if !exists {
			continue
		}
		current, err := defaultFile(status.Path)
		if err != nil {
			return MigrationPreview{}, err
		}
		preview.Changes = append(preview.Changes, MigrationChange{
			Path: status.Path, Class: status.Class, Before: body, After: current,
		})
	}
	manifestPath := filepath.Join(repoRoot, ".watchtower", ProvenanceFile)
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		flowPath := filepath.Join(repoRoot, ".watchtower", "flows", "default.yaml")
		if body, readErr := os.ReadFile(flowPath); readErr == nil {
			if patch, ok := RecognizeLegacyDefaultFlow(body); ok {
				preview.Changes = append(preview.Changes, MigrationChange{
					Path: patch.Path, Class: FileLegacy, Before: patch.Before, After: patch.After,
				})
			}
		} else if !os.IsNotExist(readErr) {
			return MigrationPreview{}, readErr
		}
	}
	slices.SortFunc(preview.Changes, func(left, right MigrationChange) int {
		return strings.Compare(left.Path, right.Path)
	})
	return preview, nil
}

func PrepareMigration(repoRoot string) (*PreparedMigration, error) {
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	preview, err := PreviewMigration(abs)
	if err != nil {
		return nil, err
	}
	if preview.Health.Overall == HealthInvalid {
		return nil, fmt.Errorf("configuration migration is invalid: %s", preview.Health.NextAction)
	}
	if len(preview.Changes) == 0 {
		return &PreparedMigration{Changes: nil, noChange: true}, nil
	}
	tempRoot, err := os.MkdirTemp(abs, ".watchtower-migrate-*")
	if err != nil {
		return nil, err
	}
	prepared := &PreparedMigration{
		PreparedReplacement: &PreparedReplacement{
			RepoRoot: abs, Staged: filepath.Join(tempRoot, ".watchtower"), tempRoot: tempRoot,
			rename: os.Rename,
		},
		Changes: preview.Changes,
	}
	if err := copyCurrentTree(abs, prepared.Staged); err != nil {
		_ = prepared.Cancel()
		return nil, err
	}
	for _, change := range preview.Changes {
		name := filepath.Join(prepared.Staged, filepath.FromSlash(change.Path))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			_ = prepared.Cancel()
			return nil, err
		}
		if err := os.WriteFile(name, change.After, 0o644); err != nil {
			_ = prepared.Cancel()
			return nil, err
		}
	}
	if err := updateMigrationManifest(tempRoot, preview); err != nil {
		_ = prepared.Cancel()
		return nil, err
	}
	if err := validateResetTree(tempRoot); err != nil {
		_ = prepared.Cancel()
		return nil, err
	}
	prepared.sourceRecheck = func() error {
		current, err := CaptureInputSnapshot(abs)
		if err != nil {
			return err
		}
		if !snapshotsEqual(current, preview.Snapshot) {
			return fmt.Errorf("concurrent configuration edit detected before migration swap")
		}
		return nil
	}
	return prepared, nil
}

func (p *PreparedMigration) Apply() error {
	if p == nil {
		return fmt.Errorf("migration is not prepared")
	}
	if p.noChange {
		return nil
	}
	if err := validateResetTree(filepath.Dir(p.Staged)); err != nil {
		return fmt.Errorf("validate staged migration: %w", err)
	}
	return p.PreparedReplacement.Apply()
}

func (p *PreparedMigration) Commit() error {
	if p == nil {
		return fmt.Errorf("migration is not prepared")
	}
	if p.noChange {
		return nil
	}
	return p.PreparedReplacement.Commit()
}

func (p *PreparedMigration) Rollback() error {
	if p == nil || p.noChange {
		return nil
	}
	return p.PreparedReplacement.Rollback()
}

func (p *PreparedMigration) Cancel() error {
	if p == nil || p.noChange {
		return nil
	}
	return p.PreparedReplacement.Cancel()
}

func copyCurrentTree(repoRoot, destination string) error {
	source := filepath.Join(repoRoot, ".watchtower")
	if _, err := os.Stat(source); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("configuration input %s is a symlink", name)
		}
		rel, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("configuration input %s is not a regular file", name)
		}
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644)
	})
}

func updateMigrationManifest(tempRoot string, preview MigrationPreview) error {
	manifestPath := filepath.Join(tempRoot, ".watchtower", ProvenanceFile)
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		migrated := make([]string, 0, len(preview.Changes))
		for _, change := range preview.Changes {
			if change.Class == FileLegacy {
				migrated = append(migrated, change.Path)
			}
		}
		manifest, err := BuildLegacyManifest(tempRoot, migrated)
		if err != nil {
			return err
		}
		return WriteManifest(manifestPath, manifest)
	}
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		return err
	}
	for index := range manifest.Files {
		for _, change := range preview.Changes {
			if change.Path != manifest.Files[index].Path {
				continue
			}
			manifest.Files[index].SHA256 = SHA256Bytes(change.After)
			manifest.Files[index].BaselineVersion = DefaultsVersion
		}
	}
	return WriteManifest(manifestPath, manifest)
}
