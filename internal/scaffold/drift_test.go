package scaffold

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInspectClassifiesCurrentStaleCustomizedMissingAndExtra(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, ".watchtower", ProvenanceFile)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	oldFlow := legacyDefaultFlow(t)
	for index := range manifest.Files {
		if manifest.Files[index].Path == "flows/default.yaml" {
			manifest.Files[index].SHA256 = SHA256Bytes(oldFlow)
			manifest.Files[index].BaselineVersion = "1"
		}
	}
	if err := WriteManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".watchtower", "flows", "default.yaml"), oldFlow, 0o644); err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(root, ".watchtower", "config.yaml")
	custom, err := os.ReadFile(customPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customPath, append(custom, []byte("# intentional\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, ".watchtower", "packages", "planner", "prompt.md")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".watchtower", "operator.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	health, err := Inspect(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	classes := make(map[string]FileClass, len(health.Files))
	paths := make([]string, 0, len(health.Files))
	for _, file := range health.Files {
		classes[file.Path] = file.Class
		paths = append(paths, file.Path)
	}
	if !slices.IsSorted(paths) {
		t.Fatalf("health paths are not sorted: %v", paths)
	}
	for path, want := range map[string]FileClass{
		"config.yaml":                 FileCustomized,
		"flows/default.yaml":          FileStale,
		"packages/planner/prompt.md":  FileMissing,
		"packages/executor/prompt.md": FileCurrent,
		"operator.txt":                FileExtra,
	} {
		if got := classes[path]; got != want {
			t.Errorf("%s class = %q, want %q", path, got, want)
		}
	}
	if health.Overall != HealthDrift {
		t.Fatalf("overall = %q, want %q", health.Overall, HealthDrift)
	}
	if health.DefaultsVersion != DefaultsVersion {
		t.Fatalf("defaults version = %q", health.DefaultsVersion)
	}
	if health.NextAction == "" || health.Diff == "" {
		t.Fatalf("health omitted action or diff: %+v", health)
	}
	if !strings.Contains(health.Diff, "flows/default.yaml") {
		t.Fatalf("diff omitted stale flow: %s", health.Diff)
	}
}

func TestInspectRejectsInvalidManifest(t *testing.T) {
	for name, body := range map[string]string{
		"malformed":      "files: [",
		"duplicate":      "schema_version: 1\ndefaults_version: '2'\nfiles:\n  - {path: config.yaml, sha256: " + strings.Repeat("a", 64) + ", baseline_version: '2'}\n  - {path: config.yaml, sha256: " + strings.Repeat("b", 64) + ", baseline_version: '2'}\n",
		"traversal":      "schema_version: 1\ndefaults_version: '2'\nfiles:\n  - {path: ../config.yaml, sha256: " + strings.Repeat("a", 64) + ", baseline_version: '2'}\n",
		"parent":         "schema_version: 1\ndefaults_version: '2'\nfiles:\n  - {path: .., sha256: " + strings.Repeat("a", 64) + ", baseline_version: '2'}\n",
		"bad hash":       "schema_version: 1\ndefaults_version: '2'\nfiles:\n  - {path: config.yaml, sha256: nope, baseline_version: '2'}\n",
		"unknown schema": "schema_version: 9\ndefaults_version: '2'\nfiles: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".watchtower"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".watchtower", ProvenanceFile), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadManifest(filepath.Join(root, ".watchtower", ProvenanceFile)); err == nil {
				t.Fatal("manifest path traversal was accepted")
			}
			health, err := Inspect(root, nil)
			if err != nil {
				t.Fatal(err)
			}
			if health.Overall != HealthInvalid || health.NextAction == "" || health.Diff == "" {
				t.Fatalf("invalid manifest health = %+v", health)
			}
			for _, class := range allFileClasses {
				if _, ok := health.Counts[class]; !ok {
					t.Errorf("invalid manifest omitted %s count", class)
				}
			}
		})
	}
}

func TestRecognizeLegacyDefaultFlowChangesOnlyGates(t *testing.T) {
	old := legacyDefaultFlow(t)
	patch, ok := RecognizeLegacyDefaultFlow(old)
	if !ok {
		t.Fatal("legacy default flow was not recognized")
	}
	current, err := defaults.ReadFile("defaults/flows/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(patch.After, current) {
		t.Fatal("legacy patch did not produce current default flow")
	}
	if got := countGateDifferences(old, patch.After); got != 2 {
		t.Fatalf("changed %d gate lines, want 2", got)
	}
	if patch.Path != "flows/default.yaml" {
		t.Fatalf("patch path = %q", patch.Path)
	}
}

func TestRecognizeLegacyShapeVariationIsPreserved(t *testing.T) {
	old := legacyDefaultFlow(t)
	variations := map[string][]byte{
		"whitespace": bytes.Replace(old, []byte("name: default"), []byte("name: custom"), 1),
		"stage":      bytes.Replace(old, []byte("workspace: worktree"), []byte("workspace: repo"), 1),
		"package":    bytes.Replace(old, []byte("package: planner"), []byte("package: custom"), 1),
		"artifact":   bytes.Replace(old, []byte("artifacts: [plan.md, touchset.json]"), []byte("artifacts: [plan.md]"), 1),
		"unrelated":  append(append([]byte(nil), old...), []byte("# custom\n")...),
	}
	for name, body := range variations {
		t.Run(name, func(t *testing.T) {
			if _, ok := RecognizeLegacyDefaultFlow(body); ok {
				t.Fatal("shape variation was recognized")
			}
		})
	}
}

func TestLegacyMigrationAdoptsUntouchedBytes(t *testing.T) {
	root := t.TempDir()
	seedDefaultsWithoutManifest(t, root)
	old := legacyDefaultFlow(t)
	flowPath := filepath.Join(root, ".watchtower", "flows", "default.yaml")
	if err := os.WriteFile(flowPath, old, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildLegacyManifest(root, []string{"flows/default.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.DefaultsVersion; got != DefaultsVersion {
		t.Fatalf("defaults version = %q", got)
	}
	for _, entry := range manifest.Files {
		if entry.Path == "config.yaml" && entry.BaselineVersion != "legacy-adopted" {
			t.Fatalf("untouched config baseline = %q", entry.BaselineVersion)
		}
		if entry.Path == "flows/default.yaml" && entry.BaselineVersion != DefaultsVersion {
			t.Fatalf("migrated flow baseline = %q", entry.BaselineVersion)
		}
	}
}

func TestLegacyAdoptedDefaultRemainsLegacy(t *testing.T) {
	root := t.TempDir()
	seedDefaultsWithoutManifest(t, root)
	manifest, err := BuildLegacyManifest(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(filepath.Join(root, ".watchtower", ProvenanceFile), manifest); err != nil {
		t.Fatal(err)
	}

	health, err := Inspect(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range health.Files {
		if file.Path == "config.yaml" && file.Class != FileLegacy {
			t.Fatalf("legacy-adopted default classified as %q, want %q", file.Class, FileLegacy)
		}
	}
}

func legacyDefaultFlow(t *testing.T) []byte {
	t.Helper()
	body, err := defaults.ReadFile("defaults/flows/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.Replace(body, []byte("    gate: approve_artifact\n"), []byte("    gate: auto\n"), 1)
	body = bytes.Replace(body, []byte("    gate: plan_review\n"), []byte("    gate: auto\n"), 1)
	return body
}

func countGateDifferences(before, after []byte) int {
	beforeLines := strings.Split(string(before), "\n")
	afterLines := strings.Split(string(after), "\n")
	count := 0
	for index := range beforeLines {
		if index < len(afterLines) && beforeLines[index] != afterLines[index] {
			count++
		}
	}
	return count
}

func seedDefaultsWithoutManifest(t *testing.T, root string) {
	t.Helper()
	for _, rel := range ManagedFiles(defaults) {
		body, err := defaults.ReadFile(filepath.ToSlash(filepath.Join("defaults", rel)))
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(root, ".watchtower", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
