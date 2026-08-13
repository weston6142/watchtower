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
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/retry"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/store"
)

type retryCountingRunner struct {
	calls  atomic.Int32
	result runner.Result
}

type rotatingRetryWorkspace struct {
	paths []string
	next  int
}

func (w *rotatingRetryWorkspace) Acquire(string) (string, func() error, error) {
	path := w.paths[w.next]
	w.next++
	return path, func() error { return nil }, nil
}

func (w *rotatingRetryWorkspace) Name() string { return "rotating retry workspace" }

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

func TestRunnerDurableFailureClassesWaitForMeaningfulStateChange(t *testing.T) {
	for _, class := range []runner.FailureClass{
		runner.FailureAuthentication,
		runner.FailureAuthorization,
		runner.FailureResumeIdentity,
		runner.FailureConfiguration,
	} {
		t.Run(string(class), func(t *testing.T) {
			repo := t.TempDir()
			initGitRepo(t, repo)
			f := retryEngineFlow(1)
			r := &retryCountingRunner{result: runner.Result{
				FailureClass: class, Err: errors.New("runner durable state is invalid"),
			}}
			e, s := newEngineCfg(t, r, func(cfg *Config) {
				cfg.Flows = map[string]flow.Flow{f.Name: f}
				cfg.Workspace = &fakeWS{dir: repo}
				cfg.RetryPolicy = retryEnginePolicy(2, 1, 1, failure.ClassExecution)
			})
			id := prepareRetryEngineBranch(t, e, string(class)+" failure")

			if err := e.StartIssue(context.Background(), id); err == nil {
				t.Fatalf("%s failure unexpectedly succeeded", class)
			}
			if got := r.calls.Load(); got != 1 {
				t.Fatalf("unchanged %s failure runner calls = %d, want 1", class, got)
			}
			rejections := retryEvents(t, s, id, core.EvRetryRejected)
			if len(rejections) != 1 || !strings.Contains(string(rejections[0].Payload), string(retry.ReasonStateUnchanged)) {
				t.Fatalf("%s retry rejection = %+v, want state_unchanged", class, rejections)
			}
		})
	}
}

func TestBoundaryVerificationFailureCanRetryAfterTreeChange(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	f := retryEngineFlow(0)
	r := &retryCountingRunner{}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.Workspace = &fakeWS{dir: repo}
		cfg.RetryPolicy = retryEnginePolicy(2, 1, 1, failure.ClassExecution)
	})
	id := prepareRetryEngineBranch(t, e, "verification boundary")
	head := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	e.mu.Lock()
	is := e.issues[id]
	is.wsPath, is.branch, is.baseRef = repo, "issue/"+id, head
	e.mu.Unlock()

	primary := errors.New("verification receipt is stale")
	if err := e.recordBoundaryFailure(context.Background(), id, f.Stages[0].Name, 1,
		failure.SiteVerification, failure.ClassValidation, failure.RetryAfterStateChange,
		failure.StateVerification, primary); !errors.Is(err, primary) {
		t.Fatalf("record boundary failure = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "diff"), []byte("repaired\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	is.terminal = true
	e.mu.Unlock()
	if err := e.RetryStage(context.Background(), id); err != nil {
		t.Fatalf("retry repaired verification boundary = %v", err)
	}
	if got := r.calls.Load(); got != 1 || len(retryEvents(t, s, id, core.EvRetryAuthorized)) != 1 {
		t.Fatalf("repaired boundary retry calls=%d authorized=%d, want 1 each",
			got, len(retryEvents(t, s, id, core.EvRetryAuthorized)))
	}
}

func TestDeterministicRetryDoesNotTreatNewLeasePathAsTreeChange(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	secondWorktree := filepath.Join(t.TempDir(), "second-worktree")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "--detach", secondWorktree, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("create second worktree: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", secondWorktree).Run()
	})

	f := retryEngineFlow(0, "required.out")
	var calls atomic.Int32
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{"execute/agent": {}}, OnStart: func(_, _, _, _ string) error {
		calls.Add(1)
		return nil
	}}
	e, _ := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.Workspace = &rotatingRetryWorkspace{paths: []string{repo, secondWorktree}}
		cfg.RetryPolicy = retryEnginePolicy(2, 1, 1, failure.ClassValidation)
	})
	id, err := e.CreateIssue("stable tree across leases", "", f.Name, levers.Matrix{}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("missing artifact unexpectedly succeeded")
	}

	err = e.RetryStage(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), string(retry.ReasonStateUnchanged)) {
		t.Fatalf("retry from equivalent lease = %v, want unchanged-state rejection", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("equivalent lease path started runner; calls=%d", calls.Load())
	}
}

func TestFailureRecordAppendFaultInvalidatesPriorRetryContext(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	f := retryEngineFlow(0)
	r := &retryCountingRunner{result: runner.Result{
		FailureClass: runner.FailureExecution, Err: errors.New("transient execution failed"),
	}}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.Workspace = &fakeWS{dir: repo}
		cfg.RetryPolicy = retryEnginePolicy(2, 1, 1, failure.ClassExecution)
	})
	id := prepareRetryEngineBranch(t, e, "failed retry evidence")
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("transiently failing stage unexpectedly succeeded")
	}

	s.FailNextFailureAppendForTest()
	if err := e.RetryStage(context.Background(), id); err == nil {
		t.Fatal("retry with injected failure-record fault unexpectedly succeeded")
	}
	if got := r.calls.Load(); got != 2 {
		t.Fatalf("runner calls after injected append fault = %d, want 2", got)
	}

	err := e.RetryStage(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), string(retry.ReasonInvalidContext)) {
		t.Fatalf("retry after missing durable evidence = %v, want invalid context", err)
	}
	if got := r.calls.Load(); got != 2 {
		t.Fatalf("retry after missing durable evidence started runner; calls=%d", got)
	}
}

func TestMultiAgentRetryFailurePreservesOneConsumedAllowance(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	f := flow.Flow{Name: "retry", Stages: []flow.Stage{{
		Name: "execute", Agents: []flow.AgentRef{{Package: "agent-a"}, {Package: "agent-b"}},
		Workspace: "worktree", Gate: flow.GateAuto, Completion: flow.CompletionAll,
	}}}
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{
		"execute/agent-a": {Fail: true},
		"execute/agent-b": {Fail: true},
	}}
	var calls atomic.Int32
	r.OnStart = func(_, _, _, _ string) error {
		calls.Add(1)
		return nil
	}
	e, _ := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.Workspace = &fakeWS{dir: repo}
		cfg.RetryPolicy = retryEnginePolicy(1, 1, 1, failure.ClassExecution)
	})
	id := prepareRetryEngineBranch(t, e, "multi-agent allowance")
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("multi-agent failure unexpectedly succeeded")
	}
	changed := e.cfg.Flows[f.Name]
	changed.Stages[0].Parallel = true
	e.cfg.Flows[f.Name] = changed
	if err := e.RetryStage(context.Background(), id); err == nil {
		t.Fatal("multi-agent retry unexpectedly succeeded")
	}
	if calls.Load() != 4 {
		t.Fatalf("runner calls after one retry = %d, want 4", calls.Load())
	}
	if err := e.RetryStage(context.Background(), id); err == nil || !strings.Contains(err.Error(), string(retry.ReasonCapExhausted)) {
		t.Fatalf("second multi-agent retry = %v, want exhausted allowance", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("exhausted multi-agent retry started runner; calls=%d", calls.Load())
	}
}

func TestRetryFailureAtNewSiteGetsNewStableFingerprint(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	f := retryEngineFlow(0, "required.out")
	f.Stages[0].MergeBarrier = true
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{"execute/agent": {}}}
	e, s := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.Workspace = &fakeWS{dir: repo}
		cfg.RetryPolicy = retryEnginePolicy(2, 1, 1, failure.ClassValidation)
	})
	id := prepareRetryEngineBranch(t, e, "new failure site")
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("missing artifact unexpectedly succeeded")
	}
	before, err := s.FailureHistory(context.Background(), id)
	if err != nil || len(before) != 1 || before[0].FailureSite != failure.SiteArtifact {
		t.Fatalf("initial failure history = %+v, err=%v", before, err)
	}

	r.Scripts["execute/agent"] = runner.Script{Artifacts: map[string]string{"required.out": "repaired\n"}}
	e.cfg.Train = &marshal.Train{Repo: repo, TestCmd: []string{"false"}}
	if err := e.RetryStage(context.Background(), id); err == nil || !strings.Contains(err.Error(), "verification") {
		t.Fatalf("retry failure = %v, want verification failure", err)
	}
	after, err := s.FailureHistory(context.Background(), id)
	if err != nil || len(after) != 2 || after[1].FailureSite != failure.SiteVerification {
		t.Fatalf("retry failure history = %+v, err=%v", after, err)
	}
	if after[0].Fingerprint == after[1].Fingerprint {
		t.Fatalf("artifact and verification failures shared fingerprint %s", after[0].Fingerprint)
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

func TestExplicitRetryObserversCanReenterEngine(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	f := retryEngineFlow(0, "required.out")
	r := &runner.FakeRunner{Scripts: map[string]runner.Script{"execute/agent": {}}}
	e, _ := newEngineCfg(t, r, func(cfg *Config) {
		cfg.Flows = map[string]flow.Flow{f.Name: f}
		cfg.Workspace = &fakeWS{dir: repo}
		cfg.RetryPolicy = retryEnginePolicy(2, 1, 1, failure.ClassValidation)
	})
	id := prepareRetryEngineBranch(t, e, "reentrant retry observer")
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("missing artifact unexpectedly succeeded")
	}
	changed := e.cfg.Flows[f.Name]
	changed.Stages[0].Parallel = true
	e.cfg.Flows[f.Name] = changed

	observed := make(chan struct{}, 1)
	e.cfg.Observers = append(e.cfg.Observers, func(event core.Event) {
		if event.Type == core.EvRetryAuthorized {
			_ = e.PendingDecisions()
			observed <- struct{}{}
		}
	})
	retried := make(chan error, 1)
	go func() { retried <- e.RetryStage(context.Background(), id) }()

	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("retry authorization observer deadlocked while reentering engine")
	}
	select {
	case err := <-retried:
		if err == nil {
			t.Fatal("missing artifact after retry unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("explicit retry did not return after observer completed")
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
			switch dimension {
			case "tree":
				if err := os.WriteFile(filepath.Join(repo, "repair"), []byte("repair again\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "config":
				changed := e.cfg.Flows[f.Name]
				changed.Stages[0].Parallel = false
				e.cfg.Flows[f.Name] = changed
			case "environment":
				e.cfg.CacheRoot = t.TempDir()
			case "decision":
				if _, err := s.InsertDecision(store.DecisionRow{
					IssueID: id, Stage: "execute", Question: "Use another repaired state?", Status: "answered",
					Response: levers.FreeformResponse("approved again"), CreatedAt: time.Now().UTC().Add(2 * time.Second),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.RetryStage(context.Background(), id); err == nil || !strings.Contains(err.Error(), string(retry.ReasonCapExhausted)) {
				t.Fatalf("second retry after %s change = %v, want exhausted allowance", dimension, err)
			}
			if calls.Load() != 2 {
				t.Fatalf("runner calls after exhausted %s retry = %d, want 2", dimension, calls.Load())
			}
		})
	}
}

func TestDeclaredArtifactChangeUpdatesRetryTreeState(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	f := retryEngineFlow(0, "required.out", "repair.out")
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
	id := prepareRetryEngineBranch(t, e, "declared artifact repair")
	if err := e.StartIssue(context.Background(), id); err == nil {
		t.Fatal("missing artifacts unexpectedly succeeded")
	}
	if err := os.WriteFile(filepath.Join(repo, "repair.out"), []byte("repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.RetryStage(context.Background(), id); err == nil {
		t.Fatal("remaining missing artifact unexpectedly succeeded")
	}
	if calls.Load() != 2 {
		t.Fatalf("declared artifact repair started runner %d times, want 2", calls.Load())
	}
	authorized := retryEvents(t, s, id, core.EvRetryAuthorized)
	if len(authorized) != 1 || !strings.Contains(string(authorized[0].Payload), `"changed_dimensions":["tree"]`) {
		t.Fatalf("declared artifact authorization = %+v", authorized)
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
	decisionID, err := s.InsertDecision(store.DecisionRow{
		IssueID: id, Stage: "execute", Question: "Try a new sample?", Status: "pending",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
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
	if err := s.AnswerDecision(decisionID, levers.FreeformResponse("new sample"), "answered"); err != nil {
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
		!strings.Contains(string(authorized[0].Payload), `"model_resample_used":1`) ||
		!strings.Contains(string(authorized[0].Payload), `"decision_identity":"sha256:`) {
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
	rejected := retryEvents(t, s, id, core.EvRetryRejected)
	if r.calls.Load() != 1 || len(rejected) != 1 {
		t.Fatalf("missing-evidence calls=%d rejected=%d", r.calls.Load(), len(rejected))
	}
	payload := string(rejected[0].Payload)
	for _, want := range []string{
		`"failure_class":"execution"`, `"failure_fingerprint":"sha256:`,
		`"shared_cap":2`, `"unavailable_dimensions":["tree"]`, `"policy_id":"test"`,
	} {
		if !strings.Contains(payload, want) {
			t.Fatalf("missing-evidence rejection omitted %q: %s", want, payload)
		}
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
