package scaffold

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInitWritesCurrentProvenanceManifest(t *testing.T) {
	root := t.TempDir()
	created, _, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}

	manifest, err := ReadManifest(filepath.Join(root, ".watchtower", ProvenanceFile))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != ManifestSchemaVersion {
		t.Fatalf("schema version = %d, want %d", manifest.SchemaVersion, ManifestSchemaVersion)
	}
	if manifest.DefaultsVersion != DefaultsVersion {
		t.Fatalf("defaults version = %q, want %q", manifest.DefaultsVersion, DefaultsVersion)
	}
	if len(manifest.Files) != len(created) {
		t.Fatalf("manifest entries = %d, created = %d", len(manifest.Files), len(created))
	}
	paths := make([]string, 0, len(manifest.Files))
	for _, entry := range manifest.Files {
		paths = append(paths, entry.Path)
		if entry.Path == ProvenanceFile || strings.Contains(entry.Path, "provenance.yaml") {
			t.Fatalf("manifest contains provenance path %q", entry.Path)
		}
		body, err := os.ReadFile(filepath.Join(root, ".watchtower", filepath.FromSlash(entry.Path)))
		if err != nil {
			t.Fatal(err)
		}
		if got := SHA256Bytes(body); got != entry.SHA256 {
			t.Errorf("%s hash = %q, want %q", entry.Path, entry.SHA256, got)
		}
		if entry.BaselineVersion != DefaultsVersion {
			t.Errorf("%s baseline = %q, want %q", entry.Path, entry.BaselineVersion, DefaultsVersion)
		}
	}
	if !slices.IsSorted(paths) {
		t.Fatalf("manifest paths are not sorted: %v", paths)
	}
}

func TestInitWithExistingFilesManifestsOnlyCreatedFiles(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, ".watchtower", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(existing), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("runner: custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	created, skipped, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(skipped, "config.yaml") {
		t.Fatalf("skipped = %v, want config.yaml", skipped)
	}
	if !slices.Contains(created, "flows/default.yaml") {
		t.Fatalf("created = %v, want flow", created)
	}
	manifest, err := ReadManifest(filepath.Join(root, ".watchtower", ProvenanceFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Files {
		if entry.Path == "config.yaml" {
			t.Fatal("manifest falsely adopted existing config.yaml")
		}
	}
	body, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, []byte("runner: custom\n")) {
		t.Fatalf("existing config changed: %q", body)
	}
}

func TestPreparedResetStagesCurrentProvenance(t *testing.T) {
	root := t.TempDir()
	if _, _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareReset(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Cancel() })
	manifest, err := ReadManifest(filepath.Join(prepared.Staged, ProvenanceFile))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.DefaultsVersion != DefaultsVersion {
		t.Fatalf("staged defaults version = %q", manifest.DefaultsVersion)
	}
	if got, want := len(manifest.Files), len(ManagedFiles(defaults)); got != want {
		t.Fatalf("staged manifest entries = %d, want %d", got, want)
	}
}

func TestDefaultFlowCopiesStaySynchronizedAndUseTargetGates(t *testing.T) {
	const path = "flows/default.yaml"
	want, err := defaults.ReadFile(filepath.ToSlash(filepath.Join("defaults", path)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join("..", "..", "dist", filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("dist %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("embedded and dist files differ: %s", path)
	}
	flow, err := defaults.ReadFile("defaults/flows/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(flow)
	for _, want := range []string{"gate: approve_artifact", "gate: plan_review"} {
		if !strings.Contains(text, want) {
			t.Errorf("default flow missing %q", want)
		}
	}
}
