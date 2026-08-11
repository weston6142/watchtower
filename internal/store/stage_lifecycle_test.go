package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
)

func openLifecycleStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func lifecycleRecord(attempt StageLifecycleAttempt, substate stagelifecycle.Substate, version, predecessor int, id string) stagelifecycle.Record {
	return stagelifecycle.Record{
		SchemaVersion: 1, IssueID: attempt.IssueID, Stage: attempt.Stage, AttemptID: attempt.AttemptID,
		Version: version, Substate: substate, PredecessorVersion: predecessor,
		TransitionID: id, ResultRef: "artifacts/attempts/" + attempt.AttemptID + "/result/manifest.json", ResultDigest: strings.Repeat("a", 64),
		PayloadDigest: strings.Repeat("a", 64),
	}
}

func putLifecycleResult(t *testing.T, s *Store, attempt StageLifecycleAttempt) {
	t.Helper()
	if err := s.PutStageLifecycleResult(attempt, contextpack.AttemptResult{
		AttemptID: attempt.AttemptID, ResultPath: "artifacts/attempts/" + attempt.AttemptID + "/result/manifest.json", ResultSHA256: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStageLifecycleCommitsOnlyValidatedRecords(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "spec", "checkpoint-1")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	putLifecycleResult(t, s, attempt)
	runnerRecord := lifecycleRecord(attempt, stagelifecycle.RunnerSucceeded, 1, 0, "checkpoint-1:runner_succeeded")
	if err := s.PrepareStageLifecycle(runnerRecord); err != nil {
		t.Fatal(err)
	}
	if got, found, err := s.LatestCommittedStageLifecycle(attempt); err != nil || found {
		t.Fatalf("prepared record became recovery-visible: %+v found=%v err=%v", got, found, err)
	}
	if err := s.CommitPreparedStageLifecycle(runnerRecord); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.LatestCommittedStageLifecycle(attempt)
	if err != nil || !found || got.Substate != stagelifecycle.RunnerSucceeded {
		t.Fatalf("committed record = %+v found=%v err=%v", got, found, err)
	}
}

func TestStageLifecycleCommitRequiresDurableResult(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "execute", "checkpoint-1")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	record := lifecycleRecord(attempt, stagelifecycle.RunnerSucceeded, 1, 0, "checkpoint-1:runner_succeeded")
	if err := s.PrepareStageLifecycle(record); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitPreparedStageLifecycle(record); err == nil {
		t.Fatal("checkpoint committed without a durable result")
	}
}

func TestStageLifecycleResultRequiresExistingAttempt(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "execute", "checkpoint-1")
	result := contextpack.AttemptResult{
		AttemptID: attempt.AttemptID, ResultPath: "artifacts/attempts/" + attempt.AttemptID + "/result/manifest.json", ResultSHA256: strings.Repeat("a", 64),
	}
	if err := s.PutStageLifecycleResult(attempt, result); err == nil {
		t.Fatal("result was accepted for a missing attempt")
	}
}

func TestStageLifecycleResultReplayRejectsConflictingIdentity(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "execute", "checkpoint-1")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	first := contextpack.AttemptResult{
		AttemptID: attempt.AttemptID, ResultPath: "artifacts/attempts/" + attempt.AttemptID + "/result/manifest.json", ResultSHA256: strings.Repeat("a", 64),
	}
	if err := s.PutStageLifecycleResult(attempt, first); err != nil {
		t.Fatal(err)
	}
	conflicting := first
	conflicting.ResultPath = "artifacts/attempts/" + attempt.AttemptID + "/result/other-manifest.json"
	conflicting.ResultSHA256 = strings.Repeat("b", 64)
	if err := s.PutStageLifecycleResult(attempt, conflicting); err == nil {
		t.Fatal("conflicting result replay was accepted")
	}
}

func TestStageLifecycleResultRejectsNonCanonicalPath(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "execute", "checkpoint-1")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStageLifecycleResult(attempt, contextpack.AttemptResult{
		AttemptID: attempt.AttemptID, ResultPath: "artifacts/attempts/checkpoint-1/result/other.json", ResultSHA256: strings.Repeat("a", 64),
	}); err == nil {
		t.Fatal("result slot accepted a non-canonical manifest path")
	}
}

func TestStageArchiveManifestRejectsCrossAttemptPath(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "execute", "checkpoint-1")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	ref := contextpack.AttemptArtifact{
		Name: "plan.md", Path: "artifacts/attempts/checkpoint-2/plan.md", SHA256: strings.Repeat("a", 64),
	}
	if err := s.PutStageArchiveManifest(attempt, "checkpoint-1:artifacts_archived", []contextpack.AttemptArtifact{ref}); err == nil {
		t.Fatal("archive manifest accepted a cross-attempt path")
	}
}

func TestStageArchiveManifestRejectsResultSlotPath(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "execute", "checkpoint-1")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	ref := contextpack.AttemptArtifact{
		Name: "plan.md", Path: "artifacts/attempts/checkpoint-1/result/plan.md", SHA256: strings.Repeat("a", 64),
	}
	if err := s.PutStageArchiveManifest(attempt, "checkpoint-1:artifacts_archived", []contextpack.AttemptArtifact{ref}); err == nil {
		t.Fatal("archive manifest accepted a result-slot path")
	}
}

func TestStageLifecycleReplayAndConflicts(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "spec", "checkpoint-1")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	putLifecycleResult(t, s, attempt)
	record := lifecycleRecord(attempt, stagelifecycle.RunnerSucceeded, 1, 0, "checkpoint-1:runner_succeeded")
	if err := s.PrepareStageLifecycle(record); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitPreparedStageLifecycle(record); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareStageLifecycle(record); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitPreparedStageLifecycle(record); err != nil {
		t.Fatal(err)
	}
	conflicting := record
	conflicting.PayloadDigest = strings.Repeat("b", 64)
	if err := s.PrepareStageLifecycle(conflicting); err == nil {
		t.Fatal("conflicting transition was accepted")
	}
}

func TestStageLifecycleFinalizationFailureRetainsPriorRecoveryPoint(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "execute", "checkpoint-1")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	putLifecycleResult(t, s, attempt)
	first := lifecycleRecord(attempt, stagelifecycle.RunnerSucceeded, 1, 0, "checkpoint-1:runner_succeeded")
	if err := s.PrepareStageLifecycle(first); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitPreparedStageLifecycle(first); err != nil {
		t.Fatal(err)
	}
	next := lifecycleRecord(attempt, stagelifecycle.ArtifactsValidated, 2, 1, "checkpoint-1:artifacts_validated")
	if err := s.PrepareStageLifecycle(next); err != nil {
		t.Fatal(err)
	}
	s.FailNextStageLifecycleCommitForTest()
	if err := s.CommitPreparedStageLifecycle(next); err == nil || !strings.Contains(err.Error(), string(stagelifecycle.CodeCheckpointFinalization)) {
		t.Fatalf("commit error = %v", err)
	}
	got, found, err := s.LatestCommittedStageLifecycle(attempt)
	if err != nil || !found || got.Substate != stagelifecycle.RunnerSucceeded {
		t.Fatalf("recovery point after failed commit = %+v found=%v err=%v", got, found, err)
	}
	if err := s.CommitPreparedStageLifecycle(next); err != nil {
		t.Fatal(err)
	}
}

func TestStageLifecycleRejectsCorruptCommittedPredecessor(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "execute", "checkpoint-1")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	putLifecycleResult(t, s, attempt)
	first := lifecycleRecord(attempt, stagelifecycle.RunnerSucceeded, 1, 0, "checkpoint-1:runner_succeeded")
	if err := s.PrepareStageLifecycle(first); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitPreparedStageLifecycle(first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE stage_lifecycle_checkpoints SET artifacts=?
		WHERE issue_id=? AND stage=? AND attempt_id=? AND version=1`,
		`[{"name":"plan.md","path":"artifacts/attempts/checkpoint-1/plan.md","sha256":"bad"}]`,
		attempt.IssueID, attempt.Stage, attempt.AttemptID); err != nil {
		t.Fatal(err)
	}
	next := lifecycleRecord(attempt, stagelifecycle.ArtifactsValidated, 2, 1, "checkpoint-1:artifacts_validated")
	if err := s.PrepareStageLifecycle(next); err == nil {
		t.Fatal("transition accepted a corrupt committed predecessor")
	}
}

func TestStageLifecycleLegacyDataSurvivesReopen(t *testing.T) {
	database := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	checkpointID, err := s.InsertStageCheckpoint(StageCheckpoint{
		IssueID: "GH-66", Stage: "spec", Status: "running",
		Artifacts: []contextpack.Artifact{{Name: "spec.md", SHA256: strings.Repeat("a", 64)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStageCheckpoint(checkpointID, "succeeded", "head", "session", "", []contextpack.Artifact{{Name: "spec.md", SHA256: strings.Repeat("a", 64)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.StageCheckpoints("GH-66")
	if err != nil || len(rows) != 1 || rows[0].ID != checkpointID || rows[0].Artifacts[0].Name != "spec.md" {
		t.Fatalf("legacy checkpoints after reopen = %+v err=%v", rows, err)
	}
	if _, found, err := s.LegacyStageLifecycle("GH-66", "spec"); err != nil || !found {
		t.Fatalf("legacy lifecycle found=%v err=%v", found, err)
	}
}
