package engineharness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/marshal"
	"github.com/weston6142/watchtower/internal/recoverymatrix"
)

func TestMain(m *testing.M) {
	if WorkerRequested() {
		if err := RunWorker(os.Stdin, os.Stdout); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func shippedHarnessFlow(t *testing.T) flow.Flow {
	t.Helper()
	flowPath := filepath.Join("..", "scaffold", "defaults", "flows", "default.yaml")
	loaded, err := flow.Load(flowPath)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestEffectObservationRetainsDuplicateAdmissions(t *testing.T) {
	recorder := &effectRecorder{}
	for range 2 {
		if err := recorder.Admit(marshal.Effect{Kind: marshal.EffectLand, Target: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	got := recorder.kinds()
	if len(got) != 2 || got[0] != "land" || got[1] != "land" {
		t.Fatalf("observed effects = %v", got)
	}
}

func TestDeterministicEnvironmentDrivesShippedEngine(t *testing.T) {
	productionFlow := shippedHarnessFlow(t)
	factory := NewFactory(productionFlow)
	scenario := recoverymatrix.Scenario{ID: "success/default", DriverID: "synthetic/deterministic", Seed: 690007}
	executor, cleanup, err := factory.New(context.Background(), scenario)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })

	observation, err := executor.Execute(context.Background(), scenario)
	if err != nil {
		t.Fatal(err)
	}
	if observation.PublicOutcome != "merged" {
		t.Fatalf("public outcome = %q, want merged", observation.PublicOutcome)
	}
	if len(observation.DiagnosticCheckpoints) != len(productionFlow.Stages) {
		t.Fatalf("observed stages = %v, want %d stages", observation.DiagnosticCheckpoints, len(productionFlow.Stages))
	}
	for index, stage := range productionFlow.Stages {
		if observation.DiagnosticCheckpoints[index] != stage.Name {
			t.Fatalf("stage %d = %q, want %q", index, observation.DiagnosticCheckpoints[index], stage.Name)
		}
	}
	if strings.Contains(observation.NormalizedClassification, "timestamp") || strings.Contains(observation.NormalizedClassification, "/tmp") {
		t.Fatalf("normalized classification contains incidental data: %q", observation.NormalizedClassification)
	}
	if err := executor.VerifyConsumed(); err != nil {
		t.Fatal(err)
	}
}

func TestDeterministicEnvironmentIsOfflineAndSynthetic(t *testing.T) {
	productionFlow := shippedHarnessFlow(t)
	factory := NewFactory(productionFlow)
	scenario := recoverymatrix.Scenario{ID: "offline", DriverID: "synthetic/deterministic", Seed: 690008}
	executor, cleanup, err := factory.New(context.Background(), scenario)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })

	observation, err := executor.Execute(context.Background(), scenario)
	if err != nil {
		t.Fatal(err)
	}
	for _, effect := range observation.Effects {
		if strings.Contains(effect, "publish") || strings.Contains(effect, "remote") {
			t.Fatalf("offline fixture attempted network publication: %q", effect)
		}
	}
	if err := executor.VerifyConsumed(); err != nil {
		t.Fatal(err)
	}
}

func TestScenarioTimeoutKillsWorkerBeforeCleanup(t *testing.T) {
	temporaryRoot := t.TempDir()
	t.Setenv("TMPDIR", temporaryRoot)
	productionFlow := shippedHarnessFlow(t)
	scenario := recoverymatrix.Scenario{ID: "timeout/default", DriverID: "synthetic/deterministic", Seed: 690009}
	summary := recoverymatrix.Run(context.Background(), []recoverymatrix.Scenario{scenario}, NewFactory(productionFlow), recoverymatrix.RunOptions{
		ScenarioTimeout: 5 * time.Millisecond,
	})
	if summary.Timeouts != 1 || summary.Failed != 1 || summary.Passed != 0 {
		t.Fatalf("timeout summary = %+v", summary)
	}
	entries, err := os.ReadDir(temporaryRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("scenario isolation roots remain after timeout: %v", entries)
	}
}
