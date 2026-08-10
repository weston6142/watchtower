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
		TransitionID: id, ResultRef: "result.json", PayloadDigest: strings.Repeat("a", 64),
	}
}

func TestStageLifecycleCommitsOnlyValidatedRecords(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "spec", "checkpoint-1")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
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

func TestStageLifecycleReplayAndConflicts(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "spec", "checkpoint-1")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
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
