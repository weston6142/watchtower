package recoverymatrix_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/recoverymatrix"
)

type fakeMatrixFactory struct {
	executors []*fakeMatrixExecutor
	build     func(recoverymatrix.Scenario) *fakeMatrixExecutor
	cleanup   func() error
}

func (f *fakeMatrixFactory) New(_ context.Context, scenario recoverymatrix.Scenario) (recoverymatrix.ScenarioExecutor, func() error, error) {
	executor := f.build(scenario)
	f.executors = append(f.executors, executor)
	cleanup := f.cleanup
	if cleanup == nil {
		cleanup = func() error { return nil }
	}
	return executor, cleanup, nil
}

type fakeMatrixExecutor struct {
	scenario    recoverymatrix.Scenario
	observation recoverymatrix.Observation
	err         error
	verifyErr   error
	panicValue  any
	wait        bool
	block       <-chan struct{}
	terminate   func()
}

func (e *fakeMatrixExecutor) Execute(ctx context.Context, scenario recoverymatrix.Scenario) (recoverymatrix.Observation, error) {
	e.scenario = scenario
	if e.panicValue != nil {
		panic(e.panicValue)
	}
	if e.wait {
		<-ctx.Done()
		return recoverymatrix.Observation{}, ctx.Err()
	}
	if e.block != nil {
		<-e.block
	}
	return e.observation, e.err
}

func (e *fakeMatrixExecutor) VerifyConsumed() error { return e.verifyErr }

func (e *fakeMatrixExecutor) Terminate(context.Context) error {
	if e.terminate != nil {
		e.terminate()
	}
	return nil
}

func TestRunnerCreatesFreshEnvironmentPerScenario(t *testing.T) {
	inventory := []recoverymatrix.Scenario{
		matrixScenario("failure/brainstorm/runner", 101),
		matrixScenario("failure/spec/runner", 202),
	}
	factory := &fakeMatrixFactory{build: func(scenario recoverymatrix.Scenario) *fakeMatrixExecutor {
		return &fakeMatrixExecutor{observation: expectedObservation(scenario)}
	}}
	summary := recoverymatrix.Run(context.Background(), inventory, factory, recoverymatrix.RunOptions{
		ManifestIdentity: "sha256:manifest", Revision: "abcdef", ScenarioTimeout: time.Second,
	})
	if summary.Executed != 2 || summary.Passed != 2 || summary.Failed != 0 || summary.Partial || summary.Filtered {
		t.Fatalf("summary = %+v", summary)
	}
	if len(factory.executors) != 2 || factory.executors[0] == factory.executors[1] {
		t.Fatal("runner reused an environment")
	}
	for index, executor := range factory.executors {
		if executor.scenario.Seed != inventory[index].Seed || len(executor.observation.Effects) != 0 {
			t.Fatalf("executor %d received scenario %+v", index, executor.scenario)
		}
	}
}

func TestRunnerRejectsPartialAndInfrastructureFailures(t *testing.T) {
	base := matrixScenario("failure/brainstorm/runner", 101)
	t.Run("filter", func(t *testing.T) {
		factory := &fakeMatrixFactory{build: func(scenario recoverymatrix.Scenario) *fakeMatrixExecutor {
			return &fakeMatrixExecutor{observation: expectedObservation(scenario)}
		}}
		summary := recoverymatrix.Run(context.Background(), []recoverymatrix.Scenario{base, matrixScenario("failure/spec/runner", 202)}, factory, recoverymatrix.RunOptions{ScenarioID: base.ID})
		if !summary.Filtered || !summary.Partial || summary.Executed != 1 {
			t.Fatalf("filtered summary = %+v", summary)
		}
	})

	tests := []struct {
		name  string
		build func(recoverymatrix.Scenario) *fakeMatrixExecutor
		check func(recoverymatrix.RunSummary) bool
	}{
		{"panic", func(recoverymatrix.Scenario) *fakeMatrixExecutor { return &fakeMatrixExecutor{panicValue: "boom"} }, func(s recoverymatrix.RunSummary) bool { return s.Panics == 1 }},
		{"timeout", func(recoverymatrix.Scenario) *fakeMatrixExecutor { return &fakeMatrixExecutor{wait: true} }, func(s recoverymatrix.RunSummary) bool { return s.Timeouts == 1 }},
		{"unexpected call", func(recoverymatrix.Scenario) *fakeMatrixExecutor {
			return &fakeMatrixExecutor{err: recoverymatrix.InfrastructureError{Kind: recoverymatrix.InfrastructureUnexpectedCall, Err: errors.New("provider called")}}
		}, func(s recoverymatrix.RunSummary) bool { return s.UnexpectedCalls == 1 }},
		{"unconsumed", func(scenario recoverymatrix.Scenario) *fakeMatrixExecutor {
			return &fakeMatrixExecutor{observation: expectedObservation(scenario), verifyErr: errors.New("one response remains")}
		}, func(s recoverymatrix.RunSummary) bool { return s.UnconsumedScripts == 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factory := &fakeMatrixFactory{build: test.build}
			summary := recoverymatrix.Run(context.Background(), []recoverymatrix.Scenario{base}, factory, recoverymatrix.RunOptions{ScenarioTimeout: 10 * time.Millisecond})
			if summary.Failed != 1 || len(summary.Results) != 1 || summary.Results[0].FailureClass != recoverymatrix.FailureHarnessInfrastructure || !test.check(summary) {
				t.Fatalf("infrastructure summary = %+v", summary)
			}
		})
	}
}

func TestRunnerEnforcesDeadlineAroundUncooperativeExecutor(t *testing.T) {
	release := make(chan struct{})
	terminated := make(chan struct{})
	cleaned := make(chan struct{})
	scenario := matrixScenario("failure/brainstorm/runner", 101)
	factory := &fakeMatrixFactory{
		build: func(scenario recoverymatrix.Scenario) *fakeMatrixExecutor {
			return &fakeMatrixExecutor{
				observation: expectedObservation(scenario), block: release,
				terminate: func() {
					close(release)
					close(terminated)
				},
			}
		},
		cleanup: func() error {
			select {
			case <-terminated:
			default:
				return errors.New("cleanup preceded termination")
			}
			close(cleaned)
			return nil
		},
	}
	started := time.Now()
	summary := recoverymatrix.Run(context.Background(), []recoverymatrix.Scenario{scenario}, factory, recoverymatrix.RunOptions{
		ScenarioTimeout: 10 * time.Millisecond,
	})
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("runner exceeded enforced deadline: %v", elapsed)
	}
	if summary.Timeouts != 1 || summary.Failed != 1 || summary.Passed != 0 {
		t.Fatalf("timeout summary = %+v", summary)
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("cleanup did not run before the timed-out scenario returned")
	}
}

func TestOracleReportsObservableContractMismatch(t *testing.T) {
	scenario := matrixScenario("failure/brainstorm/runner", 101)
	base := expectedObservation(scenario)
	tests := []struct {
		name   string
		field  string
		mutate func(*recoverymatrix.Observation)
	}{
		{"public outcome", "public outcome", func(value *recoverymatrix.Observation) { value.PublicOutcome = "blocked" }},
		{"durable state", "durable state", func(value *recoverymatrix.Observation) { value.DurableState = "failed" }},
		{"classification", "classification", func(value *recoverymatrix.Observation) { value.NormalizedClassification = "other" }},
		{"artifact identity", "artifact identities", func(value *recoverymatrix.Observation) { value.ArtifactIdentities = []string{"sha256:other"} }},
		{"forbidden effect", "effects", func(value *recoverymatrix.Observation) { value.Effects = []string{"publish:origin"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := base
			actual.ArtifactIdentities = append([]string(nil), base.ArtifactIdentities...)
			test.mutate(&actual)
			err := recoverymatrix.EvaluateContract(scenario, actual)
			if err == nil || !strings.Contains(err.Error(), scenario.ID) || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("oracle error = %v", err)
			}
		})
	}
}

type recordingEffectSink struct {
	deny       marshal.EffectKind
	admitted   []marshal.Effect
	completed  []marshal.Effect
	completion []error
}

func (s *recordingEffectSink) Admit(effect marshal.Effect) error {
	if effect.Kind == s.deny {
		return errors.New("effect denied")
	}
	s.admitted = append(s.admitted, effect)
	return nil
}

func (s *recordingEffectSink) Complete(effect marshal.Effect, err error) {
	s.completed = append(s.completed, effect)
	s.completion = append(s.completion, err)
}

func TestTrainEffectsFailClosed(t *testing.T) {
	denied := &recordingEffectSink{deny: marshal.EffectLand}
	train := &marshal.Train{Repo: filepath.Join(t.TempDir(), "missing"), Effects: denied}
	if err := train.Land(context.Background(), "GH-1", "issue/GH-1"); err == nil || !strings.Contains(err.Error(), "effect denied") {
		t.Fatalf("denied land error = %v", err)
	}
	if len(denied.completed) != 0 {
		t.Fatal("denied effect was executed")
	}

	repo, branch := effectRepo(t)
	recorded := &recordingEffectSink{}
	train = &marshal.Train{Repo: repo, Effects: recorded}
	if err := train.Land(context.Background(), "GH-1", branch); err != nil {
		t.Fatal(err)
	}
	if err := train.DeleteBranch(branch); err != nil {
		t.Fatal(err)
	}
	if len(recorded.admitted) != 2 || len(recorded.completed) != 2 {
		t.Fatalf("effects admitted=%+v completed=%+v", recorded.admitted, recorded.completed)
	}
	for _, err := range recorded.completion {
		if err != nil {
			t.Fatalf("successful effect recorded error %v", err)
		}
	}
}

func matrixScenario(id string, seed int64) recoverymatrix.Scenario {
	return recoverymatrix.Scenario{
		ID: id, Kind: recoverymatrix.ScenarioFailure, Seed: seed,
		InitialInputs: map[string]string{"issue": "GH-69"}, RecoveryInputs: map[string]string{"state": "same"},
		Expected: recoverymatrix.ExpectedContract{
			PublicOutcome: "complete", DurableState: "merged", NormalizedClassification: "none",
			ArtifactIdentities: []string{"sha256:artifact"}, RecoveryBehavior: "none",
		},
	}
}

func expectedObservation(scenario recoverymatrix.Scenario) recoverymatrix.Observation {
	return recoverymatrix.Observation{
		PublicOutcome: scenario.Expected.PublicOutcome, DurableState: scenario.Expected.DurableState,
		NormalizedClassification: scenario.Expected.NormalizedClassification,
		ArtifactIdentities:       append([]string(nil), scenario.Expected.ArtifactIdentities...),
	}
}

func effectRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) {
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "matrix@example.invalid")
	git("config", "user.name", "Matrix")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "base.txt")
	git("commit", "-qm", "base")
	git("checkout", "-qb", "issue/GH-1")
	if err := os.WriteFile(filepath.Join(repo, "issue.txt"), []byte("issue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "issue.txt")
	git("commit", "-qm", "issue")
	git("checkout", "-q", "main")
	return repo, "issue/GH-1"
}
