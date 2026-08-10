package contextpack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeAttemptArtifact(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readAttemptArtifact(t *testing.T, dir, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestAttemptArchiveKeepsSameNamedBytesForTwoAttempts(t *testing.T) {
	source := t.TempDir()
	issueDir := t.TempDir()
	writeAttemptArtifact(t, source, "plan.md", "attempt one")
	first, err := MaterializeAttemptResult(source, issueDir, "checkpoint-1",
		[]string{"plan.md"}, AttemptResult{Stage: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	firstArchive, err := PublishAttemptArchive(issueDir, first, "checkpoint-1:artifacts_archived")
	if err != nil {
		t.Fatal(err)
	}

	writeAttemptArtifact(t, source, "plan.md", "attempt two")
	second, err := MaterializeAttemptResult(source, issueDir, "checkpoint-2",
		[]string{"plan.md"}, AttemptResult{Stage: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	secondArchive, err := PublishAttemptArchive(issueDir, second, "checkpoint-2:artifacts_archived")
	if err != nil {
		t.Fatal(err)
	}
	if firstArchive[0].Path == secondArchive[0].Path {
		t.Fatal("attempt archives share a path")
	}
	if readAttemptArtifact(t, issueDir, firstArchive[0].Path) != "attempt one" ||
		readAttemptArtifact(t, issueDir, secondArchive[0].Path) != "attempt two" {
		t.Fatal("attempt archive bytes were overwritten")
	}
}

func TestAttemptArchiveReplayRequiresSameDigest(t *testing.T) {
	source, issueDir := t.TempDir(), t.TempDir()
	writeAttemptArtifact(t, source, "result.md", "stable")
	result, err := MaterializeAttemptResult(source, issueDir, "checkpoint-1",
		[]string{"result.md"}, AttemptResult{Stage: "execute"})
	if err != nil {
		t.Fatal(err)
	}
	refs, err := PublishAttemptArchive(issueDir, result, "checkpoint-1:artifacts_archived")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishAttemptArchive(issueDir, result, "checkpoint-1:artifacts_archived"); err != nil {
		t.Fatalf("exact replay failed: %v", err)
	}
	refs[0].SHA256 = strings.Repeat("f", 64)
	if _, err := PublishAttemptArchive(issueDir, AttemptResult{AttemptID: result.AttemptID, Artifacts: refs},
		"checkpoint-1:artifacts_archived"); err == nil {
		t.Fatal("conflicting archive replay succeeded")
	}
}

func TestAttemptResultSlotIsImmutableAndLegacyMaterializationRemainsReadable(t *testing.T) {
	source, issueDir, workdir := t.TempDir(), t.TempDir(), t.TempDir()
	writeAttemptArtifact(t, source, "result.md", "durable result")
	result, err := MaterializeAttemptResult(source, issueDir, "checkpoint-1",
		[]string{"result.md"}, AttemptResult{IssueID: "GH-66", Stage: "execute"})
	if err != nil {
		t.Fatal(err)
	}
	if result.AttemptID != "checkpoint-1" || result.ResultPath == "" || result.ResultSHA256 == "" {
		t.Fatalf("result metadata = %+v", result)
	}
	if err := MaterializeAttemptArtifacts(issueDir, workdir, result.Artifacts); err != nil {
		t.Fatal(err)
	}
	if got := readAttemptArtifact(t, workdir, "result.md"); got != "durable result" {
		t.Fatalf("materialized result = %q", got)
	}
	writeAttemptArtifact(t, issueDir, "artifacts/legacy.md", "legacy bytes")
	if err := MaterializeLegacy(issueDir, workdir, []string{"legacy.md"}); err != nil {
		t.Fatal(err)
	}
	if got := readAttemptArtifact(t, workdir, "legacy.md"); got != "legacy bytes" {
		t.Fatalf("legacy materialization = %q", got)
	}
}

func TestAttemptResultRejectsMissingAndNonRegularSources(t *testing.T) {
	source, issueDir := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing.md", "directory", "../outside.md", "/tmp/outside.md"} {
		if _, err := MaterializeAttemptResult(source, issueDir, "checkpoint-1",
			[]string{name}, AttemptResult{}); err == nil {
			t.Fatalf("MaterializeAttemptResult accepted %q", name)
		}
	}
}
