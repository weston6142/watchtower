package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
	"github.com/weston6142/watchtower/internal/stageresult"
)

func TestStageResultAttemptsPreservePredecessorsAndExactReplay(t *testing.T) {
	s := openLifecycleStore(t)
	first := BeginAttempt("GH-67", "execute", "checkpoint-1")
	first.CreatedAt = time.Unix(1, 0).UTC()
	second := BeginAttempt("GH-67", "execute", "checkpoint-2")
	second.CreatedAt = time.Unix(2, 0).UTC()
	for _, attempt := range []StageLifecycleAttempt{first, second} {
		if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
			t.Fatal(err)
		}
	}
	firstResult := structuredResultRef(t, first, "", stageresult.OutcomeRetryable, "a")
	secondResult := structuredResultRef(t, second, first.AttemptID, stageresult.OutcomeCompleted, "b")
	if err := putValidatedLifecycleResult(t, s, first, firstResult); err != nil {
		t.Fatal(err)
	}
	if err := putValidatedLifecycleResult(t, s, second, secondResult); err != nil {
		t.Fatal(err)
	}
	if err := putValidatedLifecycleResult(t, s, second, secondResult); err != nil {
		t.Fatalf("exact replay failed: %v", err)
	}
	attempts, err := s.StageResultAttempts("GH-67", "execute", stageresult.KindExecute)
	if err != nil || len(attempts) != 2 || attempts[0].AttemptID != "checkpoint-1" ||
		attempts[1].PredecessorAttemptID != "checkpoint-1" {
		t.Fatalf("attempt history = %+v, %v", attempts, err)
	}
	latest, found, err := s.LatestValidStageResultAttempt("GH-67", "execute", stageresult.KindExecute)
	if err != nil || !found || latest.AttemptID != "checkpoint-2" {
		t.Fatalf("latest = %+v, %v, %v", latest, found, err)
	}
	conflicting := secondResult
	conflicting.ResultSHA256 = strings.Repeat("c", 64)
	if err := putValidatedLifecycleResult(t, s, second, conflicting); diagnosticCode(err) != CodeConflict {
		t.Fatalf("conflicting replay error = %v", err)
	}
}

func TestStageResultExactReplayRemainsIdempotentAfterNewerAttempt(t *testing.T) {
	s := openLifecycleStore(t)
	first := BeginAttempt("GH-67", "execute", "checkpoint-1")
	second := BeginAttempt("GH-67", "execute", "checkpoint-2")
	first.CreatedAt, second.CreatedAt = time.Unix(1, 0).UTC(), time.Unix(2, 0).UTC()
	for _, attempt := range []StageLifecycleAttempt{first, second} {
		if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
			t.Fatal(err)
		}
	}
	firstResult := structuredResultRef(t, first, "", stageresult.OutcomeRetryable, "a")
	if err := putValidatedLifecycleResult(t, s, first, firstResult); err != nil {
		t.Fatal(err)
	}
	if err := putValidatedLifecycleResult(t, s, second, structuredResultRef(t, second, first.AttemptID, stageresult.OutcomeCompleted, "b")); err != nil {
		t.Fatal(err)
	}
	if err := putValidatedLifecycleResult(t, s, first, firstResult); err != nil {
		t.Fatalf("older exact replay failed after newer result: %v", err)
	}
}

func TestStageResultRejectsWrongOrStalePredecessor(t *testing.T) {
	s := openLifecycleStore(t)
	attempts := []StageLifecycleAttempt{
		BeginAttempt("GH-67", "execute", "checkpoint-1"),
		BeginAttempt("GH-67", "execute", "checkpoint-2"),
		BeginAttempt("GH-67", "execute", "checkpoint-3"),
	}
	for i := range attempts {
		attempts[i].CreatedAt = time.Unix(int64(i+1), 0).UTC()
		if err := createValidatedLifecycleAttempt(t, s, attempts[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := putValidatedLifecycleResult(t, s, attempts[0], structuredResultRef(t, attempts[0], "", stageresult.OutcomeRetryable, "a")); err != nil {
		t.Fatal(err)
	}
	if err := putValidatedLifecycleResult(t, s, attempts[1], structuredResultRef(t, attempts[1], "unknown", stageresult.OutcomeRetryable, "b")); err == nil {
		t.Fatal("unknown predecessor was accepted")
	}
	if err := putValidatedLifecycleResult(t, s, attempts[1], structuredResultRef(t, attempts[1], attempts[0].AttemptID, stageresult.OutcomeRetryable, "b")); err != nil {
		t.Fatal(err)
	}
	if err := putValidatedLifecycleResult(t, s, attempts[2], structuredResultRef(t, attempts[2], attempts[0].AttemptID, stageresult.OutcomeCompleted, "c")); err == nil {
		t.Fatal("stale predecessor was accepted")
	}
}

func TestStageResultPersistenceFailureLeavesPreviousLatest(t *testing.T) {
	s := openLifecycleStore(t)
	first := BeginAttempt("GH-67", "execute", "checkpoint-1")
	second := BeginAttempt("GH-67", "execute", "checkpoint-2")
	first.CreatedAt, second.CreatedAt = time.Unix(1, 0).UTC(), time.Unix(2, 0).UTC()
	for _, attempt := range []StageLifecycleAttempt{first, second} {
		if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
			t.Fatal(err)
		}
	}
	if err := putValidatedLifecycleResult(t, s, first, structuredResultRef(t, first, "", stageresult.OutcomeRetryable, "a")); err != nil {
		t.Fatal(err)
	}
	s.FailNextStageResultPutForTest()
	if err := putValidatedLifecycleResult(t, s, second, structuredResultRef(t, second, first.AttemptID, stageresult.OutcomeCompleted, "b")); err == nil {
		t.Fatal("injected persistence failure succeeded")
	}
	if _, found, err := s.StageLifecycleResult(second); err != nil || found {
		t.Fatalf("failed attempt result found=%v err=%v", found, err)
	}
	latest, found, err := s.LatestValidStageResultAttempt("GH-67", "execute", stageresult.KindExecute)
	if err != nil || !found || latest.AttemptID != first.AttemptID {
		t.Fatalf("latest = %+v, %v, %v", latest, found, err)
	}
}

func TestLatestStageResultIgnoresUnsupportedHistoricalSummary(t *testing.T) {
	s := openLifecycleStore(t)
	first := BeginAttempt("GH-67", "execute", "checkpoint-1")
	second := BeginAttempt("GH-67", "execute", "checkpoint-2")
	for _, attempt := range []StageLifecycleAttempt{first, second} {
		if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
			t.Fatal(err)
		}
	}
	if err := putValidatedLifecycleResult(t, s, first, structuredResultRef(t, first, "", stageresult.OutcomeCompleted, "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE stage_lifecycle_attempts SET stage_result_schema_version=2,
		stage_result_kind='execute',stage_result_outcome='completed',stage_result_status='valid'
		WHERE attempt_id='checkpoint-2'`); err != nil {
		t.Fatal(err)
	}
	latest, found, err := s.LatestValidStageResultAttempt("GH-67", "execute", stageresult.KindExecute)
	if err != nil || !found || latest.AttemptID != first.AttemptID {
		t.Fatalf("latest = %+v, %v, %v", latest, found, err)
	}
}

func TestStageResultColumnsMigrateLegacyLifecycleTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE stage_lifecycle_attempts(
		issue_id TEXT NOT NULL,stage TEXT NOT NULL,attempt_id TEXT NOT NULL,
		legacy_checkpoint_id INTEGER NOT NULL DEFAULT 0,result_path TEXT NOT NULL DEFAULT '',
		result_sha256 TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,
		PRIMARY KEY(issue_id,stage,attempt_id))`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	attempt := BeginAttempt("GH-67", "execute", "checkpoint-1")
	if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
		t.Fatal(err)
	}
	if err := putValidatedLifecycleResult(t, s, attempt, structuredResultRef(t, attempt, "", stageresult.OutcomeCompleted, "a")); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.LatestValidStageResultAttempt("GH-67", "execute", stageresult.KindExecute); err != nil || !found {
		t.Fatalf("migrated latest found=%v err=%v", found, err)
	}
}

func structuredResultRef(t *testing.T, attempt StageLifecycleAttempt, predecessor string, outcome stageresult.Outcome, digestByte string) contextpack.AttemptResult {
	t.Helper()
	evidence := stageresult.Evidence{
		SchemaVersion: stageresult.SchemaVersion, StageKind: stageresult.KindExecute, Outcome: outcome,
		Execute: &stageresult.ExecutePayload{
			PlanTasks: []stageresult.PlanTask{{ID: "task-0001", Outcome: stageresult.TaskCompleted, Summary: "implemented"}},
			Skips: []stageresult.Skip{
				{Activity: "commits", Explanation: "fixture has no commit"},
				{Activity: "checks", Explanation: "fixture has no check"},
			},
		},
	}
	if outcome == stageresult.OutcomeRetryable {
		evidence.RemainingWork = []stageresult.WorkItem{{Kind: stageresult.WorkPlanTask, Description: "continue implementation"}}
	}
	candidate, err := stageresult.Build(stageresult.BuildInput{
		IssueID: attempt.IssueID, AttemptID: attempt.AttemptID, PredecessorAttemptID: predecessor,
		ExpectedKind: stageresult.KindExecute, Evidence: evidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := stageresult.Validate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	return contextpack.AttemptResult{
		AttemptID: attempt.AttemptID, IssueID: attempt.IssueID, Stage: attempt.Stage,
		ResultPath:   "artifacts/attempts/" + attempt.AttemptID + "/result/manifest.json",
		ResultSHA256: strings.Repeat(digestByte, 64), StageResult: &result,
	}
}

func diagnosticCode(err error) DiagnosticCode {
	var diagnostic *DiagnosticError
	if errors.As(err, &diagnostic) {
		return diagnostic.Code
	}
	return ""
}

func openLifecycleStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func createValidatedLifecycleAttempt(t *testing.T, s *Store, attempt StageLifecycleAttempt) error {
	t.Helper()
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		return err
	}
	compiled, err := capability.Compile(capability.CompileInput{
		IssueID: attempt.IssueID, Stage: attempt.Stage, AttemptID: attempt.AttemptID,
		Profile: flow.ProfileArtifact, WorkspaceRoot: "/tmp/worktree",
		MaterializedInputs: []string{"ISSUE.md"},
		Repository:         capability.RepositoryIdentity{Branch: "issue/fixture", BaseCommit: "base", StartCommit: "head", Tree: "tree"},
	})
	if err != nil {
		return err
	}
	return s.CreateCapabilityAttempt(capability.AttemptRecord{
		Identity:      capability.AttemptIdentity{IssueID: attempt.IssueID, Stage: attempt.Stage, AttemptID: attempt.AttemptID},
		SchemaVersion: capability.ContractVersion, Contract: compiled,
	})
}

func putValidatedLifecycleResult(t *testing.T, s *Store, attempt StageLifecycleAttempt, result contextpack.AttemptResult) error {
	t.Helper()
	identity := capability.AttemptIdentity{IssueID: attempt.IssueID, Stage: attempt.Stage, AttemptID: attempt.AttemptID}
	if err := s.BindCapabilityValidation(identity, result.ResultSHA256, capability.ValidationResult{
		Passed: true, ResultDigest: result.ResultSHA256, DeltaDigest: strings.Repeat("d", 64),
	}); err != nil {
		return err
	}
	return s.PutStageLifecycleResult(attempt, result)
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
	if err := putValidatedLifecycleResult(t, s, attempt, contextpack.AttemptResult{
		AttemptID: attempt.AttemptID, ResultPath: "artifacts/attempts/" + attempt.AttemptID + "/result/manifest.json", ResultSHA256: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStageLifecycleCommitsOnlyValidatedRecords(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "spec", "checkpoint-1")
	if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
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
	if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
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
	if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
		t.Fatal(err)
	}
	first := contextpack.AttemptResult{
		AttemptID: attempt.AttemptID, ResultPath: "artifacts/attempts/" + attempt.AttemptID + "/result/manifest.json", ResultSHA256: strings.Repeat("a", 64),
	}
	if err := putValidatedLifecycleResult(t, s, attempt, first); err != nil {
		t.Fatal(err)
	}
	conflicting := first
	conflicting.ResultPath = "artifacts/attempts/" + attempt.AttemptID + "/result/other-manifest.json"
	conflicting.ResultSHA256 = strings.Repeat("b", 64)
	if err := putValidatedLifecycleResult(t, s, attempt, conflicting); err == nil {
		t.Fatal("conflicting result replay was accepted")
	}
}

func TestStageLifecycleResultRejectsNonCanonicalPath(t *testing.T) {
	s := openLifecycleStore(t)
	attempt := BeginAttempt("GH-66", "execute", "checkpoint-1")
	if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
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
	if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
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
	if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
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
	if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
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
	if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
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
	if err := createValidatedLifecycleAttempt(t, s, attempt); err != nil {
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
