package engineharness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/recoverymatrix"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
	"github.com/weston6142/watchtower/internal/store"
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

func TestArtifactIdentityRecoveryPersistsRejectedAndReplacementArtifacts(t *testing.T) {
	production := shippedHarnessFlow(t)
	var scenario recoverymatrix.Scenario
	for _, candidate := range loadCrossCuttingMatrix(t) {
		if candidate.Kind == recoverymatrix.ScenarioArtifactIdentity {
			scenario = candidate
			break
		}
	}
	configured, cleanup, err := NewFactory(production).New(context.Background(), scenario)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := configured.Execute(context.Background(), scenario); err != nil {
		t.Fatal(err)
	}
	if err := configured.VerifyConsumed(); err != nil {
		t.Fatal(err)
	}
	executor := configured.(*executor)
	records, err := executor.store.StageLifecycleRecords(executor.issueID, scenario.References.Stage, "")
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{}
	for _, record := range records {
		if !record.Committed || record.Substate != stagelifecycle.ArtifactsArchived {
			continue
		}
		for _, artifact := range record.Artifacts {
			if artifact.Name != "artifact-identity.txt" {
				continue
			}
			body, err := os.ReadFile(filepath.Join(executor.dataDir, executor.issueID, filepath.FromSlash(artifact.Path)))
			if err != nil {
				t.Fatal(err)
			}
			contents[record.AttemptID] = string(body)
		}
	}
	seen := map[string]bool{}
	for _, content := range contents {
		seen[content] = true
	}
	if len(contents) != 2 || !seen[scenario.InitialInputs["state"]] || !seen[scenario.RecoveryInputs["state"]] {
		t.Fatalf("durable artifact attempts = %+v", contents)
	}
	reviews, err := executor.store.ArtifactReviewRows(executor.issueID)
	if err != nil {
		t.Fatal(err)
	}
	var identityReviews []store.DecisionRow
	for _, row := range reviews {
		if row.Stage == scenario.References.Stage {
			identityReviews = append(identityReviews, row)
		}
	}
	if len(identityReviews) != 2 || identityReviews[0].Response.Option == nil || *identityReviews[0].Response.Option != 1 ||
		identityReviews[1].Response.Option == nil || *identityReviews[1].Response.Option != 0 ||
		identityReviews[0].Review == nil || identityReviews[1].Review == nil ||
		identityReviews[0].Review.ArtifactVersion == identityReviews[1].Review.ArtifactVersion {
		t.Fatalf("artifact identity review history = %+v", identityReviews)
	}
}
