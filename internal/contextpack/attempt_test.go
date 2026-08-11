package contextpack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/stageresult"
)

func TestAttemptResultRoundTripsValidatedStageResult(t *testing.T) {
	result := validatedAttemptStageResult(t, "checkpoint-2", "checkpoint-1")
	issueDir := t.TempDir()
	want, err := MaterializeAttemptResult(t.TempDir(), issueDir, "checkpoint-2", nil, AttemptResult{
		IssueID: "GH-67", Stage: "execute", StageResult: &result,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadAttemptResult(issueDir, want)
	if err != nil {
		t.Fatal(err)
	}
	if got.StageResult == nil || got.StageResult.AttemptID != "checkpoint-2" ||
		got.StageResult.PredecessorAttemptID != "checkpoint-1" ||
		got.StageResult.Execute.PlanTasks[0].ID != "task-0001" ||
		got.StageResult.Execute.Checks[0].Result != stageresult.CheckRed ||
		got.StageResult.Execute.Skips[0].Explanation == "" {
		t.Fatalf("loaded structured result = %#v", got.StageResult)
	}
	replayed, err := MaterializeAttemptResult(t.TempDir(), issueDir, "checkpoint-2", nil, AttemptResult{
		IssueID: "GH-67", Stage: "execute", StageResult: &result,
	})
	if err != nil || replayed.ResultSHA256 != want.ResultSHA256 {
		t.Fatalf("exact replay = %+v, %v", replayed, err)
	}
}

func TestAttemptResultRejectsInvalidOrUnsupportedStageResult(t *testing.T) {
	valid := validatedAttemptStageResult(t, "checkpoint-2", "checkpoint-1")
	invalid := valid
	invalid.ValidationStatus = stageresult.ValidationInvalid
	unsupported := valid
	unsupported.SchemaVersion = 2
	for _, tt := range []struct {
		name   string
		result stageresult.Result
	}{{"invalid", invalid}, {"unsupported", unsupported}} {
		t.Run(tt.name, func(t *testing.T) {
			issueDir := t.TempDir()
			if _, err := MaterializeAttemptResult(t.TempDir(), issueDir, "checkpoint-2", nil, AttemptResult{
				IssueID: "GH-67", Stage: "execute", StageResult: &tt.result,
			}); err == nil {
				t.Fatal("invalid structured result was materialized")
			}
			if _, err := os.Stat(filepath.Join(issueDir, "artifacts", "attempts", "checkpoint-2", "result", "manifest.json")); !os.IsNotExist(err) {
				t.Fatalf("manifest exists after rejection: %v", err)
			}
		})
	}

	for _, status := range []any{"invalid", 2} {
		issueDir := t.TempDir()
		stored, err := MaterializeAttemptResult(t.TempDir(), issueDir, "checkpoint-2", nil, AttemptResult{
			IssueID: "GH-67", Stage: "execute", StageResult: &valid,
		})
		if err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(issueDir, filepath.FromSlash(stored.ResultPath))
		body, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		var manifest map[string]any
		if err := json.Unmarshal(body, &manifest); err != nil {
			t.Fatal(err)
		}
		stageResult := manifest["stage_result"].(map[string]any)
		if version, ok := status.(int); ok {
			stageResult["schema_version"] = version
		} else {
			stageResult["validation_status"] = status
		}
		body, _ = json.Marshal(manifest)
		if err := os.WriteFile(manifestPath, body, 0o644); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(body)
		stored.ResultSHA256 = hex.EncodeToString(digest[:])
		if _, err := LoadAttemptResult(issueDir, stored); err == nil {
			t.Fatalf("tampered structured result %v was loaded", status)
		}
	}
}

func validatedAttemptStageResult(t *testing.T, attemptID, predecessor string) stageresult.Result {
	t.Helper()
	evidence := stageresult.Evidence{
		SchemaVersion: stageresult.SchemaVersion,
		StageKind:     stageresult.KindExecute,
		Outcome:       stageresult.OutcomeRetryable,
		RemainingWork: []stageresult.WorkItem{{Kind: stageresult.WorkPlanTask, Description: "finish task-0002"}},
		Execute: &stageresult.ExecutePayload{
			PlanTasks: []stageresult.PlanTask{{ID: "task-0001", Outcome: stageresult.TaskCompleted, Summary: "implemented contract"}},
			Checks:    []stageresult.Check{{Name: "red", Command: "go test ./internal/stageresult", Result: stageresult.CheckRed, Affected: true}},
			Skips:     []stageresult.Skip{{Activity: "commits", Explanation: "commit follows the test"}},
		},
	}
	candidate, err := stageresult.Build(stageresult.BuildInput{
		IssueID: "GH-67", AttemptID: attemptID, PredecessorAttemptID: predecessor,
		ExpectedKind: stageresult.KindExecute, Evidence: evidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stageresult.Validate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

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

func TestAttemptArchiveRejectsSourceOutsideIssueDirectory(t *testing.T) {
	issueDir := t.TempDir()
	outsidePath := filepath.Join(filepath.Dir(issueDir), "outside.md")
	content := []byte("outside issue data")
	if err := os.WriteFile(outsidePath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outsidePath) })
	digest := sha256.Sum256(content)
	_, err := PublishAttemptArchive(issueDir, AttemptResult{
		AttemptID: "checkpoint-1",
		Artifacts: []AttemptArtifact{{
			Name: "plan.md", Path: "../outside.md", SHA256: hex.EncodeToString(digest[:]),
		}},
	}, "checkpoint-1:artifacts_archived")
	if err == nil {
		t.Fatal("archive publication accepted a source outside the issue directory")
	}
}

func TestAttemptArchiveRejectsNonCanonicalResultPath(t *testing.T) {
	issueDir := t.TempDir()
	content := []byte("result bytes")
	sourcePath := filepath.Join(issueDir, "artifacts", "attempts", "checkpoint-1", "result", "nested", "plan.md")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	_, err := PublishAttemptArchive(issueDir, AttemptResult{
		AttemptID: "checkpoint-1",
		Artifacts: []AttemptArtifact{{
			Name: "plan.md", Path: "artifacts/attempts/checkpoint-1/result/nested/plan.md", SHA256: hex.EncodeToString(digest[:]),
		}},
	}, "checkpoint-1:artifacts_archived")
	if err == nil {
		t.Fatal("archive publication accepted a non-canonical result path")
	}
}

func TestLoadAttemptResultValidatesAndReturnsImmutableManifest(t *testing.T) {
	source, issueDir := t.TempDir(), t.TempDir()
	writeAttemptArtifact(t, source, "plan.md", "durable plan")
	want, err := MaterializeAttemptResult(source, issueDir, "checkpoint-1", []string{"plan.md"}, AttemptResult{
		IssueID: "GH-66", Stage: "plan", DependsOn: []string{"GH-12"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadAttemptResult(issueDir, AttemptResult{
		AttemptID: want.AttemptID, IssueID: want.IssueID, Stage: want.Stage,
		ResultPath: want.ResultPath, ResultSHA256: want.ResultSHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.AttemptID != want.AttemptID || got.IssueID != want.IssueID || got.Stage != want.Stage ||
		got.ResultPath != want.ResultPath || got.ResultSHA256 != want.ResultSHA256 ||
		len(got.Artifacts) != 1 || got.Artifacts[0] != want.Artifacts[0] ||
		len(got.DependsOn) != 1 || got.DependsOn[0] != "GH-12" {
		t.Fatalf("loaded attempt result = %+v, want %+v", got, want)
	}
}
