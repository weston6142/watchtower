package scaffold

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationPreviewDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	seedDefaultsWithoutManifest(t, root)
	if err := os.WriteFile(filepath.Join(root, ".watchtower", "flows", "default.yaml"), legacyDefaultFlow(t), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := CaptureInputSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Changes) != 1 || preview.Changes[0].Path != "flows/default.yaml" {
		t.Fatalf("preview changes = %+v", preview.Changes)
	}
	after, err := CaptureInputSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshotsEqual(before, after) {
		t.Fatalf("preview changed input snapshot: before=%+v after=%+v", before, after)
	}
	if _, err := os.Stat(filepath.Join(root, ".watchtower", ProvenanceFile)); !os.IsNotExist(err) {
		t.Fatalf("preview created manifest: %v", err)
	}
}

func TestMigrationApplyUpdatesOnlyStaleAndRecognizedLegacy(t *testing.T) {
	root := t.TempDir()
	seedDefaultsWithoutManifest(t, root)
	extra := filepath.Join(root, ".watchtower", "operator.txt")
	if err := os.WriteFile(extra, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	flowPath := filepath.Join(root, ".watchtower", "flows", "default.yaml")
	if err := os.WriteFile(flowPath, legacyDefaultFlow(t), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(flowPath)
	if err != nil {
		t.Fatal(err)
	}
	current, err := defaults.ReadFile("defaults/flows/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, current) {
		t.Fatal("legacy flow was not migrated to current defaults")
	}
	if body, err := os.ReadFile(extra); err != nil || string(body) != "keep" {
		t.Fatalf("extra file changed: %q err=%v", body, err)
	}
	manifest, err := ReadManifest(filepath.Join(root, ".watchtower", ProvenanceFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Files {
		if entry.Path == "flows/default.yaml" && entry.BaselineVersion != DefaultsVersion {
			t.Fatalf("migrated flow baseline = %q", entry.BaselineVersion)
		}
	}
}

func TestMigrationApplyPreservesCustomizedExtraMissingAndUncertain(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, ".watchtower", ProvenanceFile)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for index := range manifest.Files {
		if manifest.Files[index].Path == "flows/default.yaml" {
			old := legacyDefaultFlow(t)
			manifest.Files[index].SHA256 = SHA256Bytes(old)
			if err := os.WriteFile(filepath.Join(root, ".watchtower", "flows", "default.yaml"), old, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := WriteManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(root, ".watchtower", "packages", "planner", "prompt.md")
	customBefore, err := os.ReadFile(customPath)
	if err != nil {
		t.Fatal(err)
	}
	customBefore = append(customBefore, []byte("# preserve\n")...)
	if err := os.WriteFile(customPath, customBefore, 0o644); err != nil {
		t.Fatal(err)
	}
	extraPath := filepath.Join(root, ".watchtower", "extra.txt")
	if err := os.WriteFile(extraPath, []byte("extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(root, ".watchtower", "config.yaml")
	if err := os.Remove(missingPath); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(customPath); err != nil || !bytes.Equal(got, customBefore) {
		t.Fatalf("customized file changed: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(extraPath); err != nil || string(got) != "extra" {
		t.Fatalf("extra file changed: %q err=%v", got, err)
	}
	if _, err := os.Stat(missingPath); !os.IsNotExist(err) {
		t.Fatalf("missing file was recreated: %v", err)
	}
}

func TestMigrationRefusesConcurrentEdit(t *testing.T) {
	root := t.TempDir()
	seedDefaultsWithoutManifest(t, root)
	flowPath := filepath.Join(root, ".watchtower", "flows", "default.yaml")
	if err := os.WriteFile(flowPath, legacyDefaultFlow(t), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".watchtower", "operator.txt"), []byte("race"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Apply(); err == nil || !strings.Contains(err.Error(), "concurrent") {
		t.Fatalf("concurrent edit error = %v", err)
	}
	if body, err := os.ReadFile(flowPath); err != nil || !bytes.Equal(body, legacyDefaultFlow(t)) {
		t.Fatalf("original flow changed after race: %q err=%v", body, err)
	}
	_ = prepared.Cancel()
}

func TestMigrationValidationFailureLeavesOriginalTree(t *testing.T) {
	root := t.TempDir()
	seedDefaultsWithoutManifest(t, root)
	flowPath := filepath.Join(root, ".watchtower", "flows", "default.yaml")
	if err := os.WriteFile(flowPath, legacyDefaultFlow(t), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared.Staged, "config.yaml"), []byte("runner: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Apply(); err == nil {
		t.Fatal("invalid staged configuration was accepted")
	}
	if body, err := os.ReadFile(flowPath); err != nil || !bytes.Equal(body, legacyDefaultFlow(t)) {
		t.Fatalf("original tree changed after validation failure: %q err=%v", body, err)
	}
	_ = prepared.Cancel()
}

func TestMigrationApplyIsIdempotent(t *testing.T) {
	root := t.TempDir()
	seedDefaultsWithoutManifest(t, root)
	if err := os.WriteFile(filepath.Join(root, ".watchtower", "flows", "default.yaml"), legacyDefaultFlow(t), 0o644); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	second, err := PrepareMigration(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Changes) != 0 {
		t.Fatalf("second migration changes = %+v", second.Changes)
	}
	if err := second.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(); err != nil {
		t.Fatal(err)
	}
}
