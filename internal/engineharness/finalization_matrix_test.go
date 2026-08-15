package engineharness

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/recoverymatrix"
)

func TestFinalizationBoundaryMatrix(t *testing.T) {
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
		if scenario.Kind == recoverymatrix.ScenarioFinalization {
			scenarios = append(scenarios, scenario)
		}
	}
	if len(scenarios) != 6 {
		t.Fatalf("finalization inventory = %d, want 6", len(scenarios))
	}
	summary := recoverymatrix.Run(context.Background(), scenarios, NewFactory(production), recoverymatrix.RunOptions{
		ManifestIdentity: "test/finalization-matrix",
		Revision:         "test-revision",
		ScenarioTimeout:  15 * time.Second,
	})
	if summary.Executed != 6 || summary.Skipped != 0 || summary.Failed != 0 || summary.Timeouts != 0 || summary.MissingResults != 0 {
		t.Fatalf("finalization matrix summary = %+v", summary)
	}
}
