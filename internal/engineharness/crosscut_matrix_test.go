package engineharness

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/recoverymatrix"
)

func loadCrossCuttingMatrix(t *testing.T) []recoverymatrix.Scenario {
	t.Helper()
	production := shippedHarnessFlow(t)
	manifest, err := recoverymatrix.LoadManifest(filepath.Join("testdata", "failure-recovery-matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	validated, err := recoverymatrix.ValidateManifest(manifest, recoverymatrix.ResolveProduction(production))
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := recoverymatrix.Compile(validated)
	if err != nil {
		t.Fatal(err)
	}
	var scenarios []recoverymatrix.Scenario
	for _, scenario := range inventory {
		switch scenario.Kind {
		case recoverymatrix.ScenarioUnchangedRetryRefusal,
			recoverymatrix.ScenarioChangedStateRecovery,
			recoverymatrix.ScenarioPlannerCapabilityLoss,
			recoverymatrix.ScenarioConfigurationDrift,
			recoverymatrix.ScenarioCapabilityViolation,
			recoverymatrix.ScenarioDecisionEscalation,
			recoverymatrix.ScenarioArtifactIdentity,
			recoverymatrix.ScenarioFinalizationRecovery:
			scenarios = append(scenarios, scenario)
		}
	}
	return scenarios
}

func TestCrossCuttingMatrix(t *testing.T) {
	production := shippedHarnessFlow(t)
	scenarios := loadCrossCuttingMatrix(t)
	if len(scenarios) != 8 {
		t.Fatalf("cross-cutting inventory = %d, want 8", len(scenarios))
	}
	summary := recoverymatrix.Run(context.Background(), scenarios, NewFactory(production), recoverymatrix.RunOptions{
		ManifestIdentity: "test/cross-cutting-matrix",
		Revision:         "test-revision",
		ScenarioTimeout:  15 * time.Second,
	})
	if summary.Executed != 8 || summary.Skipped != 0 || summary.Failed != 0 || summary.Timeouts != 0 || summary.MissingResults != 0 {
		t.Fatalf("cross-cutting matrix summary = %+v", summary)
	}
}
