package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/recoverymatrix"
)

func TestMatrixCLI(t *testing.T) {
	err := runCommand([]string{
		"--manifest", "manifest.yaml", "--flow", "flow.yaml", "--revision", "revision",
		"--scenario", "failure/brainstorm/runner", "--evidence", "evidence.json",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("run diagnostic/evidence error = %v", err)
	}
}

func TestMatrixCompletionCLI(t *testing.T) {
	root := t.TempDir()
	evidence := filepath.Join(root, "evidence.json")
	if err := os.WriteFile(evidence, []byte(`{"version":1,"unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := completeCommand([]string{
		"--matrix-evidence", evidence, "--repository-revision", "revision", "--receipt", filepath.Join(root, "receipt.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown evidence error = %v", err)
	}

	if err := os.WriteFile(evidence, []byte(`{"version":1} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = completeCommand([]string{
		"--matrix-evidence", evidence, "--repository-revision", "revision", "--receipt", filepath.Join(root, "receipt.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing evidence error = %v", err)
	}
}

func TestMatrixCompletionCLIPartialEvidence(t *testing.T) {
	root := t.TempDir()
	evidence := filepath.Join(root, "evidence.json")
	partial := matrixEvidence{
		Version: evidenceVersion, ManifestIdentity: "manifest", Revision: "revision", DeterminismPass: true,
		Runs: [2]recoverymatrix.RunSummary{{ManifestIdentity: "manifest", Revision: "revision", Compiled: 1, Executed: 0, Partial: true}, {ManifestIdentity: "manifest", Revision: "revision", Compiled: 1, Executed: 0, Partial: true}},
	}
	if err := writeAtomicJSON(evidence, partial); err != nil {
		t.Fatal(err)
	}
	err := completeCommand([]string{
		"--matrix-evidence", evidence, "--repository-revision", "revision", "--receipt", filepath.Join(root, "receipt.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "partial") {
		t.Fatalf("partial evidence error = %v", err)
	}
}

func TestRepositoryGateOrderAndSmokeSeparation(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "scripts", "verify"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(contents)
	ordered := []string{"watchtower-matrix run", "go test ./... -race", "go vet ./...", "go build ./...", "git diff --check", "watchtower-matrix complete"}
	previous := -1
	for _, marker := range ordered {
		index := strings.Index(script, marker)
		if index < 0 || index <= previous {
			t.Fatalf("gate marker %q is out of order", marker)
		}
		previous = index
	}
	if strings.Contains(strings.ToLower(script), "provider smoke") {
		t.Fatal("real-provider smoke is in the normal gate")
	}
}
