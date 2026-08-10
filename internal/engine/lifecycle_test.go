package engine

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

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
