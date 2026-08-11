package scaffold

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// capabilityV2Hashes identifies only byte-exact generated defaults from the
// immediately previous release. Hash matching avoids filename or YAML-shape
// guesses and leaves customized/pre-provenance files untouched.
var capabilityV2Hashes = map[string]string{
	"flows/default.yaml":                         "5a8aa51426aaaa1041993219b0f9b119fe161d7a89c2ef37acbc19a4fd04535a",
	"packages/brainstorm/package.yaml":           "062caf9a641ae1f74c0c4409c17f98710acae980832fafd6c61361ea672871e4",
	"packages/brainstorm/prompt.md":              "62947cff71f4368f012aba0bd543ecb3b56e6f3e07ee5337dcf1416cb7dbc307",
	"packages/clean-code-reviewer/package.yaml":  "1f7aa1f2e94ee26ce197feb066e17bda845673159f19f7909110ec78b2234a3e",
	"packages/clean-code-reviewer/prompt.md":     "155c19e5d85808f8c5e864590c3dacf8b8befd8f7bc2d7599249e0d0881517e5",
	"packages/conflict-resolver/package.yaml":    "ae2e46357adf983467cb5eae5c8449d09a284d7f6793c50b9dd74fab2604f617",
	"packages/conflict-resolver/prompt.md":       "c1788e271ba0a6f11dcc3be9a202c5d7cd2ff405464c00ef0aabae1a2cdfe3cd",
	"packages/correctness-reviewer/package.yaml": "c85a868dee5199ffcdd778d2f576c2a990f6e88e169d7b5060451e347a26906a",
	"packages/correctness-reviewer/prompt.md":    "d7ee52bf576f2db3daa7ca84693337769f1f70eebe5ded22b399703ce0348c63",
	"packages/executor/package.yaml":             "a486a005c7a49088b4ce2dd123df9eadd4c6dba175b1c5f6d726a07c45bf9606",
	"packages/executor/prompt.md":                "c99e5c02e5b3a06c5709412ba667ddd8cbe8f95ca775d30e03b615cc18ef6e1f",
	"packages/librarian/package.yaml":            "6edc4eefdfa8fbbf872eb670dca832694ad3e652f42b83cc997699e8dbd862e5",
	"packages/librarian/prompt.md":               "7256358ddeee6dbce5209c2d874cd162bb0c151c38c586668e7fe4bd52a19dcd",
	"packages/merge-verifier/package.yaml":       "641ad43ba28ff6b7652b5fa5af0bf695f5b7dbcf192dc35e5cab3d4b2760dc7a",
	"packages/merge-verifier/prompt.md":          "7c285e20e1d85499cabef6dd33663abf3ee0fe4edc230b2fb43b5e07c462f70c",
	"packages/planner/package.yaml":              "2ae933fc9bfc661611836c75744de6700c76c5a96ec2a6db31cd9aad08c9102f",
	"packages/planner/prompt.md":                 "20eac18c0138d91ebb5f209f909ed57410755dc45cd5398b47eca9b6d42e2725",
	"packages/spec-writer/package.yaml":          "cc1b8f2ff5fe043addc1155649e257331e9c75a4afb527a80881a0e067de5208",
	"packages/spec-writer/prompt.md":             "e099aaa144e916a5acdb4e347a4249d6848883f2b8cc26f7ed96daac6b99c7af",
}

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
	// Capture the source boundary before classifying it. Any edit after this
	// point is rejected by the prepared migration's source recheck, while an
	// edit that happened before the boundary is included in the inspection.
	snapshot, err := CaptureInputSnapshot(repoRoot)
	if err != nil {
		return MigrationPreview{}, err
	}
	health, err := Inspect(repoRoot, nil)
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
		already := make(map[string]bool, len(preview.Changes))
		for _, change := range preview.Changes {
			already[change.Path] = true
		}
		for path, expected := range capabilityV2Hashes {
			if already[path] {
				continue
			}
			body, exists, readErr := readManagedFile(repoRoot, path)
			if readErr != nil {
				return MigrationPreview{}, readErr
			}
			if !exists || SHA256Bytes(body) != expected {
				continue
			}
			current, defaultErr := defaultFile(path)
			if defaultErr != nil {
				return MigrationPreview{}, defaultErr
			}
			if bytes.Equal(body, current) {
				continue
			}
			preview.Changes = append(preview.Changes, MigrationChange{
				Path: path, Class: FileLegacy, Before: body, After: current,
			})
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
