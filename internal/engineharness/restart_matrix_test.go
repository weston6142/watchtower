package engineharness

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/recoverymatrix"
)

func loadRestartMatrix(t *testing.T) (recoverymatrix.ValidatedManifest, []recoverymatrix.Scenario) {
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
	restarts := make([]recoverymatrix.Scenario, 0, 48)
	for _, scenario := range inventory {
		if scenario.Kind == recoverymatrix.ScenarioRestart {
			restarts = append(restarts, scenario)
		}
	}
	return validated, restarts
}

func TestStageBoundaryRestartMatrix(t *testing.T) {
	production := shippedHarnessFlow(t)
	_, restarts := loadRestartMatrix(t)
	if len(restarts) != 48 {
		t.Fatalf("restart inventory = %d, want 48", len(restarts))
	}
	summary := recoverymatrix.Run(context.Background(), restarts, NewFactory(production), recoverymatrix.RunOptions{
		ManifestIdentity: "test/restart-matrix",
		Revision:         "test-revision",
		ScenarioTimeout:  15 * time.Second,
	})
	if summary.Executed != 48 || summary.Skipped != 0 || summary.Failed != 0 || summary.Timeouts != 0 || summary.MissingResults != 0 {
		t.Fatalf("restart matrix summary = %+v", summary)
	}
}

func TestRestartRejectsRuntimeStateLeak(t *testing.T) {
	production := shippedHarnessFlow(t)
	_, restarts := loadRestartMatrix(t)
	scenario := restarts[0]
	scenario.DriverID = "runtime/closure"
	if _, cleanup, err := NewFactory(production).New(context.Background(), scenario); err == nil {
		if cleanup != nil {
			_ = cleanup()
		}
		t.Fatal("runtime-bearing driver was accepted")
	}
}
