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

type FileClass string

const (
	FileCurrent    FileClass = "current"
	FileStale      FileClass = "stale"
	FileCustomized FileClass = "customized"
	FileLegacy     FileClass = "legacy"
	FileMissing    FileClass = "missing"
	FileExtra      FileClass = "extra"
	FileInvalid    FileClass = "invalid"
)

var allFileClasses = [...]FileClass{
	FileCurrent, FileStale, FileCustomized, FileLegacy,
	FileMissing, FileExtra, FileInvalid,
}

type HealthState string

const (
	HealthCurrent HealthState = "current"
	HealthDrift   HealthState = "drift"
	HealthInvalid HealthState = "invalid"
)

type FileStatus struct {
	Path         string    `json:"path"`
	Class        FileClass `json:"class"`
	RecordedHash string    `json:"recorded_hash,omitempty"`
	CurrentHash  string    `json:"current_hash,omitempty"`
	Diagnostic   string    `json:"diagnostic,omitempty"`
}

type ConfigurationHealth struct {
	Overall         HealthState       `json:"overall"`
	DefaultsVersion string            `json:"defaults_version"`
	ReloadRequired  bool              `json:"reload_required"`
	Counts          map[FileClass]int `json:"counts"`
	Files           []FileStatus      `json:"files"`
	AffectedPaths   []string          `json:"affected_paths"`
	Diff            string            `json:"diff"`
	NextAction      string            `json:"next_action"`
}

type SnapshotFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type InputSnapshot struct {
	Files []SnapshotFile `json:"files"`
}

type LegacyPatch struct {
	Path   string
	Before []byte
	After  []byte
}

const maxHealthDiffBytes = 8192

// CaptureInputSnapshot hashes every regular file in the repository-local
// .watchtower tree without following symlinks.
func CaptureInputSnapshot(repoRoot string) (InputSnapshot, error) {
	root := filepath.Join(repoRoot, ".watchtower")
	var files []SnapshotFile
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return InputSnapshot{}, nil
	} else if err != nil {
		return InputSnapshot{}, err
	}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("configuration input %s is a symlink", name)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("configuration input %s is not a regular file", name)
		}
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		files = append(files, SnapshotFile{Path: filepath.ToSlash(rel), SHA256: SHA256Bytes(body)})
		return nil
	})
	if err != nil {
		return InputSnapshot{}, err
	}
	slices.SortFunc(files, func(left, right SnapshotFile) int { return strings.Compare(left.Path, right.Path) })
	return InputSnapshot{Files: files}, nil
}

// Inspect classifies the current repository-local configuration. Comparison
// failures are returned as invalid health so callers can still render them.
func Inspect(repoRoot string, loadedSnapshot *InputSnapshot) (ConfigurationHealth, error) {
	health := newHealth()
	managed := ManagedFiles(defaults)
	managedSet := make(map[string]struct{}, len(managed))
	for _, file := range managed {
		managedSet[file] = struct{}{}
	}

	if loadedSnapshot != nil {
		currentSnapshot, err := CaptureInputSnapshot(repoRoot)
		if err != nil {
			return invalidHealth(err), nil
		}
		health.ReloadRequired = !snapshotsEqual(currentSnapshot, *loadedSnapshot)
	}

	manifestPath := filepath.Join(repoRoot, ".watchtower", ProvenanceFile)
	manifest, manifestErr := ReadManifest(manifestPath)
	manifestMissing := os.IsNotExist(manifestErr)
	if manifestErr != nil && !manifestMissing {
		return invalidHealth(manifestErr), nil
	}

	var recorded map[string]ManifestEntry
	if !manifestMissing {
		recorded = make(map[string]ManifestEntry, len(manifest.Files))
		for _, entry := range manifest.Files {
			if _, ok := managedSet[entry.Path]; !ok {
				return invalidHealth(fmt.Errorf("manifest path %s is not an embedded managed file", entry.Path)), nil
			}
			recorded[entry.Path] = entry
		}
		health.DefaultsVersion = manifest.DefaultsVersion
	} else {
		health.DefaultsVersion = DefaultsVersion
	}

	var statuses []FileStatus
	for _, file := range managed {
		entry, hasBaseline := recorded[file]
		body, exists, err := readManagedFile(repoRoot, file)
		if err != nil {
			return invalidHealth(err), nil
		}
		if !exists {
			if hasBaseline {
				statuses = append(statuses, FileStatus{Path: file, Class: FileMissing, RecordedHash: entry.SHA256})
			}
			continue
		}
		currentDefault, err := defaultFile(file)
		if err != nil {
			return invalidHealth(err), nil
		}
		currentHash := SHA256Bytes(body)
		defaultHash := SHA256Bytes(currentDefault)
		status := FileStatus{Path: file, RecordedHash: entry.SHA256, CurrentHash: currentHash}
		switch {
		case manifestMissing:
			status.Class = FileLegacy
		case hasBaseline && entry.BaselineVersion == "legacy-adopted":
			status.Class = FileLegacy
		case bytes.Equal(body, currentDefault):
			status.Class = FileCurrent
		case hasBaseline && currentHash == entry.SHA256:
			status.Class = FileStale
		case hasBaseline:
			status.Class = FileCustomized
		default:
			status.Class = FileLegacy
		}
		if status.Class == FileStale && currentHash == defaultHash {
			status.Class = FileCurrent
		}
		statuses = append(statuses, status)
	}

	extra, err := extraFiles(repoRoot, managedSet)
	if err != nil {
		return invalidHealth(err), nil
	}
	for _, file := range extra {
		statuses = append(statuses, FileStatus{Path: file, Class: FileExtra})
	}
	slices.SortFunc(statuses, func(left, right FileStatus) int { return strings.Compare(left.Path, right.Path) })
	health.Files = statuses
	for _, status := range statuses {
		health.Counts[status.Class]++
		if status.Class != FileCurrent {
			health.AffectedPaths = append(health.AffectedPaths, status.Path)
		}
	}
	health.Diff = configurationDiff(repoRoot, statuses)
	health.Overall = HealthCurrent
	if len(health.AffectedPaths) > 0 || health.ReloadRequired {
		health.Overall = HealthDrift
	}
	health.NextAction = nextAction(health, manifestMissing, repoRoot)
	return health, nil
}

func BuildLegacyManifest(repoRoot string, migratedPaths []string) (Manifest, error) {
	migrated := make(map[string]struct{}, len(migratedPaths))
	for _, file := range migratedPaths {
		normalized, err := validateManifestPath(file)
		if err != nil {
			return Manifest{}, err
		}
		migrated[normalized] = struct{}{}
	}
	var paths []string
	for _, file := range ManagedFiles(defaults) {
		if _, exists, err := readManagedFile(repoRoot, file); err != nil {
			return Manifest{}, err
		} else if exists {
			paths = append(paths, file)
		}
	}
	slices.Sort(paths)
	entries := make([]ManifestEntry, 0, len(paths))
	for _, file := range paths {
		hash, err := SHA256File(filepath.Join(repoRoot, ".watchtower", filepath.FromSlash(file)))
		if err != nil {
			return Manifest{}, err
		}
		baseline := "legacy-adopted"
		if _, ok := migrated[file]; ok {
			baseline = DefaultsVersion
		}
		entries = append(entries, ManifestEntry{Path: file, SHA256: hash, BaselineVersion: baseline})
	}
	return Manifest{SchemaVersion: ManifestSchemaVersion, DefaultsVersion: DefaultsVersion, Files: entries}, nil
}

func RecognizeLegacyDefaultFlow(body []byte) (LegacyPatch, bool) {
	current, err := defaultFile("flows/default.yaml")
	if err != nil {
		return LegacyPatch{}, false
	}
	old := bytes.Replace(current, []byte("    gate: approve_artifact\n"), []byte("    gate: auto\n"), 1)
	old = bytes.Replace(old, []byte("    gate: plan_review\n"), []byte("    gate: auto\n"), 1)
	if !bytes.Equal(body, old) {
		return LegacyPatch{}, false
	}
	return LegacyPatch{Path: "flows/default.yaml", Before: old, After: current}, true
}

func newHealth() ConfigurationHealth {
	counts := make(map[FileClass]int, len(allFileClasses))
	for _, class := range allFileClasses {
		counts[class] = 0
	}
	return ConfigurationHealth{Overall: HealthCurrent, DefaultsVersion: DefaultsVersion, Counts: counts}
}

func invalidHealth(err error) ConfigurationHealth {
	health := newHealth()
	health.Overall = HealthInvalid
	health.Counts[FileInvalid] = 1
	health.Files = []FileStatus{{Path: ProvenanceFile, Class: FileInvalid, Diagnostic: err.Error()}}
	health.AffectedPaths = []string{ProvenanceFile}
	health.Diff = err.Error()
	if len(health.Diff) > maxHealthDiffBytes {
		health.Diff = health.Diff[:maxHealthDiffBytes]
	}
	health.NextAction = "manual review or watchtower reset --yes"
	return health
}

func defaultFile(file string) ([]byte, error) {
	return defaults.ReadFile(filepath.ToSlash(filepath.Join("defaults", file)))
}

func readManagedFile(repoRoot, file string) ([]byte, bool, error) {
	name := filepath.Join(repoRoot, ".watchtower", filepath.FromSlash(file))
	info, err := os.Lstat(name)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("configuration input %s is a symlink", file)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("configuration input %s is not a regular file", file)
	}
	body, err := os.ReadFile(name)
	return body, true, err
}

func extraFiles(repoRoot string, managed map[string]struct{}) ([]string, error) {
	root := filepath.Join(repoRoot, ".watchtower")
	var files []string
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("configuration input %s is a symlink", name)
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ProvenanceFile {
			return nil
		}
		if _, ok := managed[rel]; !ok {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	return files, nil
}

func configurationDiff(repoRoot string, statuses []FileStatus) string {
	var out strings.Builder
	for _, status := range statuses {
		if status.Class == FileCurrent {
			continue
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(&out, "--- %s\n+++ %s (current default)\n", status.Path, status.Path)
		body, exists, err := readManagedFile(repoRoot, status.Path)
		if status.Class == FileExtra {
			if err == nil && exists {
				for _, line := range strings.Split(string(body), "\n") {
					fmt.Fprintf(&out, "+%s\n", line)
				}
			}
		} else if err == nil && exists {
			want, wantErr := defaultFile(status.Path)
			if wantErr == nil {
				before := strings.Split(string(body), "\n")
				after := strings.Split(string(want), "\n")
				limit := len(before)
				if len(after) > limit {
					limit = len(after)
				}
				for index := 0; index < limit; index++ {
					var left, right string
					if index < len(before) {
						left = before[index]
					}
					if index < len(after) {
						right = after[index]
					}
					if left != right {
						if index < len(before) {
							fmt.Fprintf(&out, "-%s\n", left)
						}
						if index < len(after) {
							fmt.Fprintf(&out, "+%s\n", right)
						}
					}
				}
			}
		}
		if out.Len() >= maxHealthDiffBytes {
			result := out.String()
			return result[:maxHealthDiffBytes]
		}
	}
	return out.String()
}

func nextAction(health ConfigurationHealth, manifestMissing bool, repoRoot string) string {
	if health.Overall == HealthCurrent {
		return "no action"
	}
	if health.Overall == HealthInvalid {
		return "manual review or watchtower reset --yes"
	}
	if manifestMissing {
		flowPath := filepath.Join(repoRoot, ".watchtower", "flows", "default.yaml")
		if body, err := os.ReadFile(flowPath); err == nil {
			if _, ok := RecognizeLegacyDefaultFlow(body); ok {
				return "watchtower migrate"
			}
		}
		return "manual review or watchtower reset --yes"
	}
	if health.Counts[FileStale] > 0 {
		return "watchtower migrate"
	}
	return "manual review or watchtower reset --yes"
}

func snapshotsEqual(left, right InputSnapshot) bool {
	if len(left.Files) != len(right.Files) {
		return false
	}
	for index := range left.Files {
		if left.Files[index] != right.Files[index] {
			return false
		}
	}
	return true
}
