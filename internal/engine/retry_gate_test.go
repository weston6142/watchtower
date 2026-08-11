package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/levers"
	"github.com/weston6142/watchtower/internal/retry"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/store"
)

type retryCountingRunner struct {
	calls  atomic.Int32
	result runner.Result
}

func (r *retryCountingRunner) Run(context.Context, string, string, string, string, chan<- runner.Ask) <-chan runner.Result {
	r.calls.Add(1)
	done := make(chan runner.Result, 1)
	done <- r.result
	return done
}

func retryEnginePolicy(transient, deterministic, model int, classes ...failure.Class) retry.Policy {
	return retry.Policy{
		ID: "test", Version: "1", TransientLimit: transient,
		DeterministicLimit: deterministic, ModelResampleLimit: model,
		ModelResampleClasses: classes,
	}
}

func retryEngineFlow(retries int, artifacts ...string) flow.Flow {
	return flow.Flow{Name: "retry", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "agent"}},
		Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll,
		Retries: retries, Artifacts: artifacts,
	}}}
}

func prepareRetryEngineBranch(t *testing.T, e *Engine, title string) string {
	t.Helper()
	id, err := e.CreateIssue(title, "", "retry", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	repo := e.cfg.Workspace.(*fakeWS).dir
	if out, err := exec.Command("git", "-C", repo, "checkout", "-qb", "issue/"+id).CombinedOutput(); err != nil {
		t.Fatalf("create issue branch: %v: %s", err, out)
	}
	return id
}

func retryEvents(t *testing.T, s *store.Store, issueID string, eventType core.EventType) []core.Event {
	t.Helper()
	events, err := s.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	var matching []core.Event
	for _, event := range events {
		if event.IssueID == issueID && event.Type == eventType {
			matching = append(matching, event)
		}
	}
	return matching
}

func TestAutomaticAndExplicitRetriesShareOneAllowance(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	f := retryEngineFlow(1)
	r := &retryCountingRunner{result: runner.Result{
		FailureClass: runner.FailureExecution, Err: errors.New("transient execution failed"),
	}}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.Workspace = &fakeWS{dir: repo}
		cfg.RetryPolicy = retryEnginePolicy(1, 1, 1, failure.ClassExecution)
	})
	id := prepareRetryEngineBranch(t, e, "shared allowance")
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("transiently failing stage unexpectedly succeeded")
	}
	if got := r.calls.Load(); got != 2 {
		t.Fatalf("automatic runner calls = %d, want 2", got)
	}
	if err := e.RetryStage(context.Background(), id); err == nil || !strings.Contains(err.Error(), string(retry.ReasonCapExhausted)) {
		t.Fatalf("explicit retry error = %v, want cap exhaustion", err)
	}
	if got := r.calls.Load(); got != 2 {
		t.Fatalf("cap-exhausted explicit retry started runner; calls=%d", got)
	}
	if len(retryEvents(t, s, id, core.EvRetryAuthorized)) != 1 || len(retryEvents(t, s, id, core.EvRetryRejected)) != 1 {
		t.Fatalf("retry events = authorized %d rejected %d",
			len(retryEvents(t, s, id, core.EvRetryAuthorized)), len(retryEvents(t, s, id, core.EvRetryRejected)))
	}
}

func TestUnchangedDeterministicRetryIsRejectedBeforeVerification(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	f := retryEngineFlow(1, "required.out")
	var calls atomic.Int32
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{"execute/agent": {}}, OnStart: func(_, _, _, _ string) error {
		calls.Add(1)
		return nil
	}}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.Workspace = &fakeWS{dir: repo}
		cfg.RetryPolicy = retryEnginePolicy(2, 1, 1, failure.ClassValidation)
	})
	id := prepareRetryEngineBranch(t, e, "unchanged deterministic")
	err := e.StartIssue(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), string(retry.ReasonStateUnchanged)) {
		t.Fatalf("stage error = %v, want unchanged-state rejection", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("unchanged retry called runner %d times", calls.Load())
	}
	rejected := retryEvents(t, s, id, core.EvRetryRejected)
	if len(rejected) != 1 || !strings.Contains(string(rejected[0].Payload), `"reason":"state_unchanged"`) ||
		!strings.Contains(string(rejected[0].Payload), `"unchanged_dimensions":["tree","config","environment","decision"]`) {
		t.Fatalf("retry rejection = %+v", rejected)
	}
}

func TestEachStateDimensionUnlocksWithoutResettingBudget(t *testing.T) {
	for _, dimension := range []string{"tree", "config", "environment", "decision"} {
		t.Run(dimension, func(t *testing.T) {
			repo := t.TempDir()
			initGitRepo(t, repo)
			f := retryEngineFlow(0, "required.out")
			var calls atomic.Int32
			r := &runner.FakeRunner{Scripts: map[string]runner.Script{"execute/agent": {}}, OnStart: func(_, _, _, _ string) error {
				calls.Add(1)
				return nil
			}}
			e, s := newEngineCfg(t, r, func(cfg *Config) {
				cfg.Flows = map[string]flow.Flow{f.Name: f}
				cfg.Workspace = &fakeWS{dir: repo}
				cfg.RetryPolicy = retryEnginePolicy(2, 1, 1, failure.ClassValidation)
			})
			id := prepareRetryEngineBranch(t, e, "change "+dimension)
			if err := e.StartIssue(context.Background(), id); err == nil {
				t.Fatal("missing artifact unexpectedly succeeded")
			}
			switch dimension {
			case "tree":
				if err := os.WriteFile(filepath.Join(repo, "repair"), []byte("repair\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				for _, args := range [][]string{{"add", "repair"}, {"commit", "-qm", "repair"}} {
					if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
						t.Fatalf("git %v: %v: %s", args, err, out)
					}
				}
			case "config":
				changed := e.cfg.Flows[f.Name]
				changed.Stages[0].Parallel = true
				e.cfg.Flows[f.Name] = changed
			case "environment":
				e.cfg.CacheRoot = t.TempDir()
			case "decision":
				if _, err := s.InsertDecision(store.DecisionRow{
					IssueID: id, Stage: "execute", Question: "Use repaired state?", Status: "answered",
					Response: levers.FreeformResponse("approved"), CreatedAt: time.Now().UTC().Add(time.Second),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.RetryStage(context.Background(), id); err == nil {
				t.Fatal("second missing-artifact run unexpectedly succeeded")
			}
			if calls.Load() != 2 {
				t.Fatalf("runner calls after %s change = %d, want 2", dimension, calls.Load())
			}
			authorized := retryEvents(t, s, id, core.EvRetryAuthorized)
			if len(authorized) != 1 || !strings.Contains(string(authorized[0].Payload), `"changed_dimensions":["`+dimension+`"]`) ||
				!strings.Contains(string(authorized[0].Payload), `"shared_used":1`) {
				t.Fatalf("authorization after %s change = %+v", dimension, authorized)
			}
		})
	}
}

func TestModelResampleIsExplicitAndUsesNewerDurableDecision(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	f := retryEngineFlow(0, "required.out")
	var calls atomic.Int32
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{"execute/agent": {}}, OnStart: func(_, _, _, _ string) error {
		calls.Add(1)
		return nil
	}}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.Workspace = &fakeWS{dir: repo}
		cfg.RetryPolicy = retryEnginePolicy(2, 1, 1, failure.ClassValidation)
	})
	id := prepareRetryEngineBranch(t, e, "model resample")
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("missing artifact unexpectedly succeeded")
	}
	if err := e.RetryStageWithKind(context.Background(), id, retry.KindModelResample, 999); err == nil ||
		!strings.Contains(err.Error(), string(retry.ReasonKindNotAllowed)) {
		t.Fatalf("missing decision retry error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid model resample started runner; calls=%d", calls.Load())
	}
	decisionID, err := s.InsertDecision(store.DecisionRow{
		IssueID: id, Stage: "execute", Question: "Try a new sample?", Status: "answered",
		Response: levers.FreeformResponse("new sample"), CreatedAt: time.Now().UTC().Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RetryStageWithKind(context.Background(), id, retry.KindModelResample, decisionID); err == nil {
		t.Fatal("missing artifact after model resample unexpectedly succeeded")
	}
	if calls.Load() != 2 {
		t.Fatalf("valid model resample runner calls = %d, want 2", calls.Load())
	}
	authorized := retryEvents(t, s, id, core.EvRetryAuthorized)
	if len(authorized) != 1 || !strings.Contains(string(authorized[0].Payload), `"retry_kind":"model-resample"`) ||
		!strings.Contains(string(authorized[0].Payload), `"model_resample_used":1`) {
		t.Fatalf("model-resample authorization = %+v", authorized)
	}
}

func TestMissingEvidenceFailsClosedWithoutStartingRunner(t *testing.T) {
	f := retryEngineFlow(0)
	r := &retryCountingRunner{result: runner.Result{
		FailureClass: runner.FailureExecution, Err: errors.New("execution failed"),
	}}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.RetryPolicy = retryEnginePolicy(2, 1, 1, failure.ClassExecution)
	})
	id, err := e.CreateIssue("missing evidence", "", f.Name, levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("failing stage unexpectedly succeeded")
	}
	if err := e.RetryStage(context.Background(), id); err == nil || !strings.Contains(err.Error(), string(retry.ReasonFingerprintUnavailable)) {
		t.Fatalf("missing-evidence retry error = %v", err)
	}
	if r.calls.Load() != 1 || len(retryEvents(t, s, id, core.EvRetryRejected)) != 1 {
		t.Fatalf("missing-evidence calls=%d rejected=%d", r.calls.Load(), len(retryEvents(t, s, id, core.EvRetryRejected)))
	}
}

func TestSpecializedLifecycleRetriesRemainModelFree(t *testing.T) {
	e, s := newEngineCfg(t, &retryCountingRunner{}, nil)
	id, err := e.CreateIssue("cleanup", "", "default", levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.issues[id].terminal = true
	e.mu.Unlock()
	if err := s.SetIssueIntegration(store.IssueIntegration{IssueID: id, State: store.IntegrationCleanupNeeded}); err != nil {
		t.Fatal(err)
	}
	if err := e.RetryStage(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(retryEvents(t, s, id, core.EvRetryAuthorized)) != 0 || len(retryEvents(t, s, id, core.EvRetryRejected)) != 0 ||
		len(retryEvents(t, s, id, core.EvCleanupCompleted)) != 1 {
		t.Fatalf("specialized cleanup events were routed through generic retry gate")
	}
}
