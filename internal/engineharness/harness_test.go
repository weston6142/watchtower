package engineharness

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/recoverymatrix"
)

func shippedHarnessFlow(t *testing.T) flow.Flow {
	t.Helper()
	flowPath := filepath.Join("..", "scaffold", "defaults", "flows", "default.yaml")
	loaded, err := flow.Load(flowPath)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestDeterministicEnvironmentDrivesShippedEngine(t *testing.T) {
	productionFlow := shippedHarnessFlow(t)
	factory := NewFactory(productionFlow)
	scenario := recoverymatrix.Scenario{ID: "success/default", Seed: 690007}
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
	executor, cleanup, err := factory.New(context.Background(), recoverymatrix.Scenario{ID: "offline", Seed: 690008})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cleanup() })

	observation, err := executor.Execute(context.Background(), recoverymatrix.Scenario{ID: "offline", Seed: 690008})
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
