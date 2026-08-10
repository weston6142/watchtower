package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/weston6142/watchtower/internal/contextpack"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
	"github.com/weston6142/watchtower/internal/store"
)

func lifecycleTestFlow() flow.Flow {
	return flow.Flow{Name: "lifecycle", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}},
		Workspace: "none", Gate: flow.GateAuto, Completion: flow.CompletionAll,
	}}}
}

func lifecycleTestRunner(starts *atomic.Int32) *runner.FakeRunner {
	return &runner.FakeRunner{
		Scripts: map[string]runner.Script{"execute/agent": {}},
		OnStart: func(_, _, _, _ string) error {
			starts.Add(1)
			return nil
		},
	}
}

func lifecycleTestEngine(t *testing.T, r runner.Runner) (*Engine, *store.Store) {
	t.Helper()
	f := lifecycleTestFlow()
	return newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
}

func lifecycleCommittedSubstates(t *testing.T, s *store.Store, issueID string) []string {
	t.Helper()
	records, err := s.StageLifecycleRecords(issueID, "execute", "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, record := range records {
		if record.Committed {
			got = append(got, string(record.Substate))
		}
	}
	return got
}

func TestStageLifecycleCommitsSixSubstatesInOrder(t *testing.T) {
	var starts atomic.Int32
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(&starts))
	f := lifecycleTestFlow()
	id, err := e.CreateIssue("lifecycle", "", f.Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	want := []string{"runner_succeeded", "artifacts_validated", "artifacts_archived", "gate_resolved", "verification_passed", "finalization_ready"}
	if got := lifecycleCommittedSubstates(t, s, id); !reflect.DeepEqual(got, want) {
		t.Fatalf("committed substates = %v, want %v", got, want)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts = %d, want one", starts.Load())
	}
}

func TestCheckpointFinalizationFailureIsVisibleAndResumableWithoutSecondRunner(t *testing.T) {
	var starts atomic.Int32
	r := lifecycleTestRunner(&starts)
	e, s := lifecycleTestEngine(t, r)
	f := lifecycleTestFlow()
	id, err := e.CreateIssue("lifecycle failure", "", f.Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextStageLifecycleCommitForTest()
	if err := e.StartIssue(context.Background(), id); err == nil ||
		!strings.Contains(err.Error(), string(stagelifecycle.CodeCheckpointFinalization)) {
		t.Fatalf("StartIssue error = %v", err)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts after failure = %d, want one", starts.Load())
	}
	if err := e.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got := lifecycleCommittedSubstates(t, s, id); len(got) != 6 || got[0] != "runner_succeeded" || got[5] != "finalization_ready" {
		t.Fatalf("recovered substates = %v", got)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts after deterministic retry = %d, want one", starts.Load())
	}
}

func TestCheckpointPreparationFailureResumesDurableResultWithoutSecondRunner(t *testing.T) {
	var starts atomic.Int32
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(&starts))
	f := lifecycleTestFlow()
	id, err := e.CreateIssue("lifecycle prepare failure", "", f.Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextStageLifecyclePrepareForTest()
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("StartIssue unexpectedly succeeded")
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts after prepare failure = %d, want one", starts.Load())
	}
	if err := e.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts after durable-result recovery = %d, want one", starts.Load())
	}
}

func TestLegacyCheckpointFailurePrecedesFinalizationReady(t *testing.T) {
	var starts atomic.Int32
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(&starts))
	f := lifecycleTestFlow()
	id, err := e.CreateIssue("legacy checkpoint failure", "", f.Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextStageCheckpointFinishForTest()
	if err := e.StartIssue(context.Background(), id); err == nil ||
		!strings.Contains(err.Error(), string(stagelifecycle.CodeCheckpointFinalization)) {
		t.Fatalf("StartIssue error = %v, want checkpoint finalization failure", err)
	}
	if got := lifecycleCommittedSubstates(t, s, id); len(got) != 5 || got[4] != "verification_passed" {
		t.Fatalf("committed substates after legacy finalization failure = %v, want through verification_passed", got)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts = %d, want one", starts.Load())
	}
}

func TestRehydratedArtifactReviewCommitsGateResolvedBeforeNextStage(t *testing.T) {
	f := artifactGateFlow()
	e1, s := newEngineCfg(t, artifactReviewRunner(), func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
	})
	id, err := e1.CreateIssue("rehydrated artifact review", "", f.Name, levers.Preset(f, flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e1.StartIssue(context.Background(), id) }()
	spec := waitForPendingStage(t, e1, "spec")
	e2 := newEngineOnFileWithFlow(t, s, artifactReviewRunner(), e1.cfg.DataDir, f)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e2.Answer(spec.ID, levers.ChoiceResponse(0)); err != nil {
		t.Fatal(err)
	}
	_ = waitForPendingStage(t, e2, "plan")
	records, err := s.StageLifecycleRecords(id, "spec", fmt.Sprintf("checkpoint-%d", spec.Review.CheckpointID))
	if err != nil {
		t.Fatal(err)
	}
	var committed []string
	for _, record := range records {
		if record.Committed {
			committed = append(committed, string(record.Substate))
		}
	}
	want := []string{"runner_succeeded", "artifacts_validated", "artifacts_archived", "gate_resolved"}
	if len(committed) < len(want) || !reflect.DeepEqual(committed[:len(want)], want) {
		t.Fatalf("spec committed substates = %v, want prefix %v", committed, want)
	}
}

func TestRehydrateRecoversDurableLifecycleBeforeLegacyFallback(t *testing.T) {
	var starts atomic.Int32
	r := lifecycleTestRunner(&starts)
	e1, s := lifecycleTestEngine(t, r)
	f := lifecycleTestFlow()
	id, err := e1.CreateIssue("lifecycle restart", "", f.Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextStageLifecycleCommitForTest()
	if err := e1.StartIssue(context.Background(), id); err == nil {
		t.Fatal("StartIssue unexpectedly succeeded")
	}

	e2 := newEngineOnFileWithFlow(t, s, r, e1.cfg.DataDir, f)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e2.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts after restart recovery = %d, want one", starts.Load())
	}
}

func TestLifecycleAttemptForUsesNewestNumericAttempt(t *testing.T) {
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(new(atomic.Int32)))
	for _, attemptID := range []string{"checkpoint-9", "checkpoint-10"} {
		attempt := store.BeginAttempt("GH-66", "execute", attemptID)
		if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
			t.Fatal(err)
		}
		if err := s.PutStageLifecycleResult(attempt, contextpack.AttemptResult{
			AttemptID: attemptID, ResultPath: "artifacts/attempts/" + attemptID + "/result/manifest.json", ResultSHA256: strings.Repeat("a", 64),
		}); err != nil {
			t.Fatal(err)
		}
		record := stagelifecycle.Record{
			SchemaVersion: 1, IssueID: attempt.IssueID, Stage: attempt.Stage, AttemptID: attempt.AttemptID,
			Version: 1, Substate: stagelifecycle.RunnerSucceeded, TransitionID: attemptID + ":runner_succeeded",
			ResultRef: "artifacts/attempts/" + attemptID + "/result/manifest.json", ResultDigest: strings.Repeat("a", 64), PayloadDigest: strings.Repeat("a", 64),
		}
		if err := s.PrepareStageLifecycle(record); err != nil {
			t.Fatal(err)
		}
		if err := s.CommitPreparedStageLifecycle(record); err != nil {
			t.Fatal(err)
		}
	}
	got, records, err := e.lifecycleAttemptFor(&issueState{id: "GH-66"}, flow.Stage{Name: "execute"}, 11)
	if err != nil {
		t.Fatal(err)
	}
	if got.AttemptID != "checkpoint-10" {
		t.Fatalf("selected attempt = %q, want checkpoint-10", got.AttemptID)
	}
	for _, record := range records {
		if record.AttemptID != got.AttemptID {
			t.Fatalf("recovery record from attempt %q was returned for selected attempt %q", record.AttemptID, got.AttemptID)
		}
	}
}

func TestLifecycleAttemptForReusesDurableResultWithoutLedgerRecord(t *testing.T) {
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(new(atomic.Int32)))
	attempt := store.BeginAttempt("GH-66", "execute", "checkpoint-9")
	if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStageLifecycleResult(attempt, contextpack.AttemptResult{
		AttemptID: attempt.AttemptID, ResultPath: "artifacts/attempts/" + attempt.AttemptID + "/result/manifest.json", ResultSHA256: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err := e.lifecycleAttemptFor(&issueState{id: "GH-66"}, flow.Stage{Name: "execute"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.AttemptID != attempt.AttemptID {
		t.Fatalf("selected attempt = %q, want %q", got.AttemptID, attempt.AttemptID)
	}
}

func TestMaterializeStageContextUsesNewestNumericAttempt(t *testing.T) {
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(new(atomic.Int32)))
	issueID := "GH-66"
	for _, item := range []struct {
		attemptID string
		contents  string
	}{
		{attemptID: "checkpoint-9", contents: "older"},
		{attemptID: "checkpoint-10", contents: "newer"},
	} {
		attempt := store.BeginAttempt(issueID, "spec", item.attemptID)
		if err := s.CreateStageLifecycleAttempt(attempt); err != nil {
			t.Fatal(err)
		}
		resultPath := "artifacts/attempts/" + item.attemptID + "/result/manifest.json"
		if err := s.PutStageLifecycleResult(attempt, contextpack.AttemptResult{
			AttemptID: item.attemptID, ResultPath: resultPath, ResultSHA256: strings.Repeat("a", 64),
		}); err != nil {
			t.Fatal(err)
		}
		archivePath := filepath.Join(e.issueDir(issueID), "artifacts", "attempts", item.attemptID, "plan.md")
		if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archivePath, []byte(item.contents), 0o644); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(item.contents))
		archiveRef := contextpack.AttemptArtifact{
			Name: "plan.md", Path: filepath.ToSlash(filepath.Join("artifacts", "attempts", item.attemptID, "plan.md")),
			SHA256: hex.EncodeToString(digest[:]),
		}
		transitionID := item.attemptID + ":artifacts_archived"
		if err := s.PutStageArchiveManifest(attempt, transitionID, []contextpack.AttemptArtifact{archiveRef}); err != nil {
			t.Fatal(err)
		}
		for version, substate := range []stagelifecycle.Substate{
			stagelifecycle.RunnerSucceeded, stagelifecycle.ArtifactsValidated, stagelifecycle.ArtifactsArchived,
		} {
			record := stagelifecycle.Record{
				SchemaVersion: 1, IssueID: issueID, Stage: "spec", AttemptID: item.attemptID,
				Version: version + 1, Substate: substate, PredecessorVersion: version,
				TransitionID: item.attemptID + ":" + string(substate), PayloadDigest: strings.Repeat("b", 64),
				ResultRef: resultPath, ResultDigest: strings.Repeat("a", 64),
			}
			if substate == stagelifecycle.ArtifactsArchived {
				record.Artifacts = []stagelifecycle.ArtifactRef{{Name: archiveRef.Name, Path: archiveRef.Path, SHA256: archiveRef.SHA256}}
			}
			if err := s.PrepareStageLifecycle(record); err != nil {
				t.Fatal(err)
			}
			if err := s.CommitPreparedStageLifecycle(record); err != nil {
				t.Fatal(err)
			}
		}
		checkpointID, err := s.InsertStageCheckpoint(store.StageCheckpoint{IssueID: issueID, Stage: "spec", Status: "running"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishStageCheckpoint(checkpointID, "succeeded", "", "", "", nil); err != nil {
			t.Fatal(err)
		}
	}

	workdir := t.TempDir()
	if err := e.materializeStageContext(issueID, workdir, []string{"plan.md"}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(workdir, "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "newer" {
		t.Fatalf("materialized plan = %q, want newer attempt bytes", contents)
	}
}

func TestRestoreLifecycleResultRejectsManifestIdentityMismatch(t *testing.T) {
	e, _ := lifecycleTestEngine(t, lifecycleTestRunner(new(atomic.Int32)))
	issueID, stage, attemptID := "GH-66", "execute", "checkpoint-1"
	resultPath := "artifacts/attempts/" + attemptID + "/result/manifest.json"
	manifest := []byte(`{"attempt_id":"checkpoint-2","issue_id":"GH-66","stage":"other","artifacts":[]}`)
	manifestPath := filepath.Join(e.issueDir(issueID), filepath.FromSlash(resultPath))
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifest)
	_, err := e.restoreLifecycleResult(e.issueDir(issueID), stagelifecycle.Record{
		SchemaVersion: 1, IssueID: issueID, Stage: stage, AttemptID: attemptID,
		Version: 1, Substate: stagelifecycle.RunnerSucceeded,
		TransitionID: attemptID + ":runner_succeeded", PayloadDigest: strings.Repeat("b", 64),
		ResultRef: resultPath, ResultDigest: hex.EncodeToString(digest[:]),
	}, t.TempDir())
	if err == nil {
		t.Fatal("restore accepted a result manifest with a conflicting identity")
	}
}
