package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
)

func TestStageFailsBeforeRunnerWhenContractInvalidOrUnsupported(t *testing.T) {
	t.Run("contract invalid", func(t *testing.T) {
		started := false
		r := &runner.FakeRunner{Scripts: map[string]runner.Script{"spec/spec-writer": {}}, OnStart: func(_, _, _, _ string) error {
			started = true
			return nil
		}}
		e, _ := newEngine(t, r)
		id, err := e.CreateIssue("invalid capability", "", "default", nil, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		stage := noneStage()
		stage.CapabilityProfile = "not-a-profile"
		err = e.runStageOnce(context.Background(), e.issues[id], stage, 1, 1, nil)
		if err == nil || started {
			t.Fatalf("invalid contract err=%v started=%t", err, started)
		}
	})

	t.Run("provider unsupported", func(t *testing.T) {
		started := false
		r := &runner.FakeRunner{
			Scripts:        map[string]runner.Script{"spec/spec-writer": {}},
			PreflightError: &capability.PolicyError{Phase: "preflight", Reason: capability.ReasonProviderUnsupported, Diagnostic: "control proof missing"},
			OnStart:        func(_, _, _, _ string) error { started = true; return nil },
		}
		e, _ := newEngine(t, r)
		id, err := e.CreateIssue("unsupported capability", "", "default", nil, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		stage := noneStage()
		stage.CapabilityProfile = flow.ProfileArtifact
		err = e.runStageOnce(context.Background(), e.issues[id], stage, 1, 1, nil)
		if err == nil || !strings.Contains(err.Error(), string(capability.ReasonProviderUnsupported)) || started {
			t.Fatalf("unsupported preflight err=%v started=%t", err, started)
		}
	})
}

func TestStageValidatesBeforeRunnerSucceeded(t *testing.T) {
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"spec/agent": {Artifacts: map[string]string{"spec.md": "validated output\n"}},
	}}
	e, s := newEngine(t, r)
	id, err := e.CreateIssue("validated capability", "", "default", nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	stage := flow.Stage{
		Name: "spec", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none",
		Completion: flow.CompletionAll, Gate: flow.GateAuto, CapabilityProfile: flow.ProfileArtifact,
		Artifacts: []string{"spec.md"},
	}
	if err := e.runStageOnce(context.Background(), e.issues[id], stage, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.StageLifecycleAttempts(id, stage.Name)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts=%+v err=%v", attempts, err)
	}
	record, found, err := s.CapabilityAttempt(id, stage.Name, attempts[0].AttemptID)
	if err != nil || !found || !record.Validation.Passed || record.ImmutableResultID == "" {
		t.Fatalf("capability record=%+v found=%t err=%v", record, found, err)
	}
	lifecycle, found, err := s.LatestCommittedStageLifecycle(attempts[0])
	if err != nil || !found || !lifecycleReached(lifecycle.Substate, stagelifecycle.RunnerSucceeded) {
		t.Fatalf("lifecycle=%+v found=%t err=%v", lifecycle, found, err)
	}
}

func TestDishonestRunnerCannotArchiveOrAdvance(t *testing.T) {
	r := &runner.FakeRunner{
		Scripts: map[string]runner.Script{"inspect/agent": {}},
		OnStart: func(_, _, _, workdir string) error {
			return os.WriteFile(filepath.Join(workdir, "outside.txt"), []byte("unapproved\n"), 0o644)
		},
	}
	e, s := newEngine(t, r)
	id, err := e.CreateIssue("dishonest capability", "", "default", nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	stage := flow.Stage{
		Name: "inspect", Agents: []flow.AgentRef{{Package: "agent"}}, Workspace: "none",
		Completion: flow.CompletionAll, Gate: flow.GateAuto, CapabilityProfile: flow.ProfileArtifact,
	}
	err = e.runStageOnce(context.Background(), e.issues[id], stage, 1, 1, nil)
	if err == nil || !strings.Contains(err.Error(), string(capability.ReasonPostStageViolation)) {
		t.Fatalf("dishonest stage error=%v", err)
	}
	attempts, loadErr := s.StageLifecycleAttempts(id, stage.Name)
	if loadErr != nil || len(attempts) != 1 {
		t.Fatalf("attempts=%+v err=%v", attempts, loadErr)
	}
	if _, found, loadErr := s.StageLifecycleResult(attempts[0]); loadErr != nil || found {
		t.Fatalf("rejected result found=%t err=%v", found, loadErr)
	}
	record, found, loadErr := s.CapabilityAttempt(id, stage.Name, attempts[0].AttemptID)
	if loadErr != nil || !found || record.Validation.Passed {
		t.Fatalf("capability record=%+v found=%t err=%v", record, found, loadErr)
	}
}

type reapingCapabilityRunner struct {
	reaped atomic.Bool
}

func (r *reapingCapabilityRunner) Preflight(_ context.Context, request runner.PreflightRequest) (capability.EnforcementPlan, error) {
	return testRunnerPreflight(request)
}

func (r *reapingCapabilityRunner) Run(ctx context.Context, request runner.StageRequest, _ chan<- runner.Ask) <-chan runner.Result {
	done := make(chan runner.Result, 1)
	go func() {
		if request.Agent == "fast" {
			done <- runner.Result{}
			return
		}
		<-ctx.Done()
		r.reaped.Store(true)
		done <- runner.Result{Err: ctx.Err()}
	}()
	return done
}

func TestParallelStageReapsAllAgentsBeforeValidation(t *testing.T) {
	r := &reapingCapabilityRunner{}
	e, _ := newEngine(t, r)
	id, err := e.CreateIssue("parallel capability", "", "default", nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	stage := flow.Stage{
		Name: "inspect", Agents: []flow.AgentRef{{Package: "fast"}, {Package: "slow"}},
		Workspace: "none", Parallel: true, Completion: flow.CompletionAny, Gate: flow.GateAuto,
		CapabilityProfile: flow.ProfileArtifact,
	}
	if err := e.runStageOnce(context.Background(), e.issues[id], stage, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if !r.reaped.Load() {
		t.Fatal("stage returned before the canceled peer was reaped")
	}
}
