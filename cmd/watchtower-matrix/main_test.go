package main

import (
	"os"
	"os/exec"
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
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "calls.log")
	gitStub := `#!/usr/bin/env bash
set -euo pipefail
printf 'git %s\n' "$*" >>"$GATE_LOG"
if [[ "$*" == "rev-parse HEAD" ]]; then printf 'fixture-revision\n'; fi
`
	goStub := `#!/usr/bin/env bash
set -euo pipefail
printf 'go %s\n' "$*" >>"$GATE_LOG"
if [[ "$*" == *"watchtower-matrix complete"* ]]; then
  while (($#)); do
    if [[ "$1" == "--receipt" ]]; then mkdir -p "$(dirname "$2")"; printf '{}\n' >"$2"; break; fi
    shift
  done
fi
`
	for name, body := range map[string]string{"git": gitStub, "go": goStub} {
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gatePath, err := filepath.Abs(filepath.Join("..", "..", "scripts", "verify"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(gatePath)
	command.Dir = root
	command.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "GATE_LOG="+logPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("gate failed: %v: %s", err, output)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	ordered := []string{"watchtower-matrix run", "go test ./... -race", "go vet ./...", "go build ./...", "git diff --check", "watchtower-matrix complete"}
	previous := -1
	for _, marker := range ordered {
		index := strings.Index(string(calls), marker)
		if index < 0 || index <= previous {
			t.Fatalf("gate call %q is out of order in %s", marker, calls)
		}
		previous = index
	}
	if _, err := os.Stat(filepath.Join(root, ".watchtower", "matrix-receipts", "fixture-revision.json")); err != nil {
		t.Fatalf("worktree-local receipt: %v", err)
	}
}

func TestLoadInventoryUsesManifestContentIdentity(t *testing.T) {
	shippedManifest := filepath.Join("..", "..", "internal", "engineharness", "testdata", "failure-recovery-matrix.yaml")
	body, err := os.ReadFile(shippedManifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "matrix.yaml")
	if err := os.WriteFile(manifestPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	flowPath := filepath.Join("..", "..", "internal", "scaffold", "defaults", "flows", "default.yaml")
	_, _, firstIdentity, err := loadInventory(manifestPath, flowPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(body), "seed: 690003", "seed: 690004", 1)
	if err := os.WriteFile(manifestPath, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, secondIdentity, err := loadInventory(manifestPath, flowPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(firstIdentity, "sha256:") || firstIdentity == secondIdentity {
		t.Fatalf("content identities = %q and %q", firstIdentity, secondIdentity)
	}
}

func TestRepositoryGateRejectsDirtyWorktreeBeforeRunningChecks(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitStub := `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == "rev-parse HEAD" ]]; then printf 'fixture-revision\n'; exit 0; fi
if [[ "$*" == "status --porcelain --untracked-files=all" ]]; then printf ' M dirty.go\n'; exit 0; fi
`
	goStub := "#!/usr/bin/env bash\nset -euo pipefail\nprintf ran >\"$GATE_MARKER\"\n"
	for name, body := range map[string]string{"git": gitStub, "go": goStub} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gatePath, err := filepath.Abs(filepath.Join("..", "..", "scripts", "verify"))
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "go-ran")
	command := exec.Command(gatePath)
	command.Dir = root
	command.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "GATE_MARKER="+marker)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "must be clean") {
		t.Fatalf("dirty gate error = %v, output=%s", err, output)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("verification command ran before clean-tree rejection: %v", err)
	}
}
