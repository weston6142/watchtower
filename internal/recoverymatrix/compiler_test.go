package recoverymatrix_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/recoverymatrix"
)

func TestCompilerUsesSemanticStableIDs(t *testing.T) {
	manifest := loadShippedManifest(t)
	validated, err := recoverymatrix.ValidateManifest(manifest, productionInventory(t))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := recoverymatrix.Compile(validated)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled) != len(manifest.Scenarios) {
		t.Fatalf("compiled scenarios = %d, want %d", len(compiled), len(manifest.Scenarios))
	}
	ids := make([]string, len(compiled))
	for index, scenario := range compiled {
		ids[index] = scenario.ID
		if scenario.Seed == 0 || len(scenario.InitialInputs) == 0 || len(scenario.RecoveryInputs) == 0 {
			t.Fatalf("scenario %q lost deterministic inputs", scenario.ID)
		}
		if scenario.Expected.PublicOutcome == "" || scenario.Expected.DurableState == "" || scenario.Expected.NormalizedClassification == "" || len(scenario.Expected.ArtifactIdentities) == 0 || scenario.Expected.RecoveryBehavior == "" {
			t.Fatalf("scenario %q lost its observable contract", scenario.ID)
		}
	}
	if !slices.IsSorted(ids) {
		t.Fatalf("compiled IDs are not lexical: %v", ids)
	}

	reordered := cloneManifest(t, manifest)
	slices.Reverse(reordered.Scenarios)
	validatedReordered, err := recoverymatrix.ValidateManifest(reordered, productionInventory(t))
	if err != nil {
		t.Fatal(err)
	}
	recompiled, err := recoverymatrix.Compile(validatedReordered)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(compiled, recompiled) {
		t.Fatal("declaration order changed compiled inventory")
	}

	manifest.Scenarios[0].InitialInputs["mutated"] = "caller"
	if _, ok := compiled[0].InitialInputs["mutated"]; ok {
		t.Fatal("compiled scenario aliases manifest input maps")
	}
}

func TestCompilerRejectsIncompleteScenarioContracts(t *testing.T) {
	if _, err := recoverymatrix.Compile(recoverymatrix.ValidatedManifest{}); err == nil || !strings.Contains(err.Error(), "validated") {
		t.Fatalf("zero validated manifest error = %v", err)
	}

	base := loadShippedManifest(t)
	tests := []struct {
		name   string
		want   string
		mutate func(*recoverymatrix.Manifest)
	}{
		{"nonsemantic ID", "does not match semantic ID", func(manifest *recoverymatrix.Manifest) { manifest.Scenarios[0].ID = "failure/by-index/0" }},
		{"duplicate ID", "duplicate scenario ID", func(manifest *recoverymatrix.Manifest) {
			manifest.Scenarios = append(manifest.Scenarios, manifest.Scenarios[0])
		}},
		{"missing reference", "requires stage and failure family references", func(manifest *recoverymatrix.Manifest) { manifest.Scenarios[0].References.Stage = "" }},
		{"empty seed", "deterministic seed", func(manifest *recoverymatrix.Manifest) { manifest.Scenarios[0].Seed = 0 }},
		{"empty inputs", "initial inputs", func(manifest *recoverymatrix.Manifest) { manifest.Scenarios[0].InitialInputs = nil }},
		{"empty expected contract", "expected contract", func(manifest *recoverymatrix.Manifest) { manifest.Scenarios[0].Expected.PublicOutcome = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := cloneManifest(t, base)
			test.mutate(&manifest)
			if _, err := recoverymatrix.ValidateManifest(manifest, productionInventory(t)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}
