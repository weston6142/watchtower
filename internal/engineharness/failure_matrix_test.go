package engineharness

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/recoverymatrix"
)

func loadFailureMatrix(t *testing.T) (flow.Flow, recoverymatrix.ValidatedManifest, []recoverymatrix.Scenario) {
	t.Helper()
	productionFlow := shippedHarnessFlow(t)
	manifest, err := recoverymatrix.LoadManifest(filepath.Join("testdata", "failure-recovery-matrix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	validated, err := recoverymatrix.ValidateManifest(manifest, recoverymatrix.ResolveProduction(productionFlow))
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := recoverymatrix.Compile(validated)
	if err != nil {
		t.Fatal(err)
	}
	var failures []recoverymatrix.Scenario
	for _, scenario := range inventory {
		if scenario.Kind == recoverymatrix.ScenarioFailure {
			failures = append(failures, scenario)
		}
	}
	return productionFlow, validated, failures
}

func TestFailureFamilyMatrix(t *testing.T) {
	productionFlow, _, failures := loadFailureMatrix(t)
	if len(failures) == 0 {
		t.Fatal("failure matrix compiled no scenarios")
	}
	factory := NewFactory(productionFlow)
	summary := recoverymatrix.Run(context.Background(), failures, factory, recoverymatrix.RunOptions{
		ManifestIdentity: "test/failure-matrix",
		Revision:         "test-revision",
		ScenarioTimeout:  5 * time.Second,
	})
	if summary.Executed != summary.Compiled || summary.Skipped != 0 || summary.Failed != 0 {
		t.Fatalf("failure matrix summary = %+v", summary)
	}
	for _, result := range summary.Results {
		if result.Status != recoverymatrix.ResultPassed {
			t.Fatalf("scenario %s result = %+v", result.ScenarioID, result)
		}
	}
}

func TestFailureMatrixNegativeSelfTests(t *testing.T) {
	productionFlow, _, failures := loadFailureMatrix(t)
	factory := NewFactory(productionFlow)
	if len(failures) == 0 {
		t.Fatal("failure matrix compiled no scenarios")
	}
	scenario := failures[0]
	scenario.Expected.PublicOutcome = "intentionally-perturbed"
	actual := recoverymatrix.Observation{
		PublicOutcome:            "merged",
		DurableState:             "merged",
		NormalizedClassification: "success",
		ArtifactIdentities:       []string{"synthetic/artifact"},
	}
	if err := recoverymatrix.EvaluateContract(scenario, actual); err == nil {
		t.Fatal("perturbed public outcome unexpectedly satisfied the contract")
	}
	_ = factory
}
