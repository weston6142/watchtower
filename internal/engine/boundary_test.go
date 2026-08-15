package engine

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/review"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/slots"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
	"github.com/weston6142/watchtower/internal/store"
	"github.com/weston6142/watchtower/internal/workspace"
)

type boundaryObserverFunc func(context.Context, DurableBoundary) error

func (f boundaryObserverFunc) AfterCommit(ctx context.Context, boundary DurableBoundary) error {
	return f(ctx, boundary)
}

func openBoundaryStore(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func boundaryLifecycleEngine(t *testing.T, s *store.Store, dataDir string, r runner.Runner, observer BoundaryObserver) *Engine {
	t.Helper()
	f := lifecycleTestFlow()
	flows := capabilityTestFlows(map[string]flow.Flow{f.Name: f})
	return New(Config{
		Store: s, Runner: r, BoundaryObserver: observer, Pool: slots.NewPool(1),
		Flows: flows, DataDir: dataDir,
		DecisionIdentities: testDecisionIdentities(),
	})
}

func TestDurableBoundaryObserverRunsAfterCommit(t *testing.T) {
	dataDir := t.TempDir()
	storePath := filepath.Join(dataDir, "watchtower.db")
	s := openBoundaryStore(t, storePath)
	var starts atomic.Int32
	interrupted := errors.New("simulated daemon interruption")
	var notifications atomic.Int32
	observer := boundaryObserverFunc(func(_ context.Context, boundary DurableBoundary) error {
		if boundary.Kind == BoundaryStageLifecycle && boundary.ID == string(stagelifecycle.RunnerSucceeded) && notifications.Add(1) == 1 {
			return interrupted
		}
		return nil
	})
	r := lifecycleTestRunner(&starts)
	e1 := boundaryLifecycleEngine(t, s, dataDir, r, observer)
	id, err := e1.CreateIssue("boundary", "", lifecycleTestFlow().Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e1.StartIssue(context.Background(), id); !errors.Is(err, interrupted) {
		t.Fatalf("StartIssue error = %v, want interruption", err)
	}
	if got := lifecycleCommittedSubstates(t, s, id); len(got) != 1 || got[0] != string(stagelifecycle.RunnerSucceeded) {
		t.Fatalf("committed substates at interruption = %v", got)
	}
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.IssueID == id && event.Type == "stage_completed" {
			t.Fatal("stage completed before the post-commit interruption returned")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openBoundaryStore(t, storePath)
	t.Cleanup(func() { _ = s.Close() })
	e2 := boundaryLifecycleEngine(t, s, dataDir, r, observer)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	if err := e2.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatalf("runner starts = %d, want 1", starts.Load())
	}
	if got := lifecycleCommittedSubstates(t, s, id); len(got) != len(stagelifecycle.DurableBoundaries()) {
		t.Fatalf("committed substates after recovery = %v", got)
	}
}

func TestBoundaryObserverDoesNotSeeFailedCommit(t *testing.T) {
	var notifications atomic.Int32
	observer := boundaryObserverFunc(func(context.Context, DurableBoundary) error {
		notifications.Add(1)
		return nil
	})
	var starts atomic.Int32
	e, s := lifecycleTestEngine(t, lifecycleTestRunner(&starts))
	e.cfg.BoundaryObserver = observer
	id, err := e.CreateIssue("failed commit", "", lifecycleTestFlow().Name, levers.Matrix{"execute": flow.LeverYolo}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.FailNextStageLifecycleCommitForTest()
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("StartIssue unexpectedly succeeded")
	}
	if notifications.Load() != 0 {
		t.Fatalf("observer notifications = %d, want 0", notifications.Load())
	}
}

type recordingBoundaryEffects struct {
	mu        sync.Mutex
	admitted  []marshal.Effect
	completed []marshal.Effect
}

func (s *recordingBoundaryEffects) Admit(effect marshal.Effect) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admitted = append(s.admitted, effect)
	return nil
}

func (s *recordingBoundaryEffects) Complete(effect marshal.Effect, _ error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed = append(s.completed, effect)
}

func (s *recordingBoundaryEffects) count(kind marshal.EffectKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, effect := range s.admitted {
		if effect.Kind == kind {
			count++
		}
	}
	return count
}

func boundaryVerificationScripts(t *testing.T, f flow.Flow, base string) map[string]runner.Script {
	t.Helper()
	decisionBody, err := json.Marshal(marshal.MergeDecision{Decision: "merge", BranchCommit: base, BaseCommit: base})
	if err != nil {
		t.Fatal(err)
	}
	scripts := map[string]runner.Script{}
	for _, stage := range f.Stages {
		for _, agent := range stage.Agents {
			scripts[stage.Name+"/"+agent.Package] = runner.Script{}
		}
	}
	scripts["approval/planner"] = runner.Script{Artifacts: map[string]string{
		"touchset.json": `{"globs":["feature.txt","repair-*"]}`,
	}}
	scripts["merge-verification/merge-verifier"] = runner.Script{Artifacts: map[string]string{
		"merge-report.md": "verified\n", "merge-decision.json": string(decisionBody),
	}}
	return scripts
}

func boundaryVerificationEngine(
	t *testing.T,
	s *store.Store,
	dataDir, repo string,
	r runner.Runner,
	observer BoundaryObserver,
	effects marshal.EffectSink,
) *Engine {
	t.Helper()
	f := verificationFlow()
	return New(Config{
		Store: s, Runner: r, BoundaryObserver: observer, Pool: slots.NewPool(1),
		Flows: map[string]flow.Flow{f.Name: f}, DataDir: dataDir,
		Workspace:          workspace.GitWorktree{Repo: repo},
		Train:              &marshal.Train{Repo: repo, TestCmd: []string{"true"}, Effects: effects},
		DecisionIdentities: testDecisionIdentities(),
		PlanReview: review.PolicySettings{
			ID: "boundary-test", Version: "1", AutoApproveRegular: true, Valid: true,
		},
	})
}

func TestFinalizationBoundaryResumesAfterFreshRuntime(t *testing.T) {
	dataDir := t.TempDir()
	repo := t.TempDir()
	initGitRepo(t, repo)
	base := gitOutput(t, repo, "rev-parse", "HEAD")
	storePath := filepath.Join(dataDir, "watchtower.db")
	s := openBoundaryStore(t, storePath)
	interrupted := errors.New("simulated finalization interruption")
	observer := boundaryObserverFunc(func(_ context.Context, boundary DurableBoundary) error {
		if boundary.Kind == BoundaryFinalization && boundary.ID == store.IntegrationVerificationReady {
			return interrupted
		}
		return nil
	})
	effects := &recordingBoundaryEffects{}
	var runnerStarts atomic.Int32
	firstRunner := &runner.FakeRunner{Scripts: boundaryVerificationScripts(t, verificationFlow(), base)}
	firstRunner.OnStart = func(_, stage, _, _ string) error {
		if stage == "merge-verification" {
			runnerStarts.Add(1)
		}
		return nil
	}
	e1 := boundaryVerificationEngine(t, s, dataDir, repo, firstRunner, observer, effects)
	id, err := e1.CreateIssue("finalization boundary", "", verificationFlow().Name, levers.Preset(verificationFlow(), flow.LeverYolo), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e1.StartIssue(context.Background(), id); !errors.Is(err, interrupted) {
		t.Fatalf("StartIssue error = %v, want interruption", err)
	}
	integration, found, err := s.IssueIntegration(id)
	if err != nil || !found || integration.State != store.IntegrationVerificationReady {
		t.Fatalf("integration at interruption = %+v, found=%v, err=%v", integration, found, err)
	}
	if effects.count(marshal.EffectLand) != 0 {
		t.Fatal("merge effect ran before verification_ready interruption returned")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openBoundaryStore(t, storePath)
	t.Cleanup(func() { _ = s.Close() })
	secondRunner := &runner.FakeRunner{Scripts: boundaryVerificationScripts(t, verificationFlow(), base)}
	secondRunner.OnStart = func(_, stage, _, _ string) error {
		if stage == "merge-verification" {
			runnerStarts.Add(1)
		}
		return nil
	}
	e2 := boundaryVerificationEngine(t, s, dataDir, repo, secondRunner, nil, effects)
	if err := e2.Rehydrate(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		integration, found, err = s.IssueIntegration(id)
		if err != nil {
			t.Fatal(err)
		}
		if found && integration.State == store.IntegrationMerged {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("integration did not recover: %+v", integration)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runnerStarts.Load() != 1 {
		t.Fatalf("merge-verifier starts = %d, want 1", runnerStarts.Load())
	}
	if effects.count(marshal.EffectLand) != 1 {
		t.Fatalf("land effects = %d, want 1", effects.count(marshal.EffectLand))
	}
}
