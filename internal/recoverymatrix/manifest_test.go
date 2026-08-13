package recoverymatrix_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/recoverymatrix"
)

const shippedManifestPath = "../engineharness/testdata/failure-recovery-matrix.yaml"

func TestManifestValidationRejectsIncompleteCoverage(t *testing.T) {
	t.Run("strict YAML decoding", func(t *testing.T) {
		body, err := os.ReadFile(shippedManifestPath)
		if err != nil {
			t.Fatal(err)
		}

		unknown := writeManifest(t, append(append([]byte(nil), body...), []byte("\nunknown_field: forbidden\n")...))
		if _, err := recoverymatrix.LoadManifest(unknown); err == nil || !strings.Contains(err.Error(), "unknown_field") {
			t.Fatalf("unknown-field error = %v", err)
		}

		trailing := writeManifest(t, append(append([]byte(nil), body...), []byte("\n---\nschema_version: 1\n")...))
		if _, err := recoverymatrix.LoadManifest(trailing); err == nil || !strings.Contains(err.Error(), "trailing YAML document") {
			t.Fatalf("trailing-document error = %v", err)
		}
	})

	production := productionInventory(t)
	base := loadShippedManifest(t)
	t.Run("duplicate production inventory fails closed", func(t *testing.T) {
		duplicate := production
		duplicate.Stages = append(append([]string(nil), production.Stages...), production.Stages[0])
		if _, err := recoverymatrix.ValidateManifest(base, duplicate); err == nil || !strings.Contains(err.Error(), "duplicate production stage") {
			t.Fatalf("duplicate-production error = %v", err)
		}
	})

	tests := []struct {
		name   string
		want   string
		mutate func(*recoverymatrix.Manifest)
	}{
		{
			name: "duplicate stage declaration",
			want: "duplicate stage",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.Stages = append(manifest.Stages, manifest.Stages[0])
			},
		},
		{
			name: "unknown stage declaration",
			want: "unknown stage",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.Stages[0] = "future-stage"
			},
		},
		{
			name: "missing production stage",
			want: "missing production stage",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.Stages = manifest.Stages[1:]
			},
		},
		{
			name: "duplicate failure-family declaration",
			want: "duplicate failure family",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.FailureFamilies = append(manifest.FailureFamilies, manifest.FailureFamilies[0])
			},
		},
		{
			name: "unknown failure-family declaration",
			want: "unknown failure family",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.FailureFamilies[0] = "future-failure"
			},
		},
		{
			name: "missing production failure-family",
			want: "missing production failure family",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.FailureFamilies = manifest.FailureFamilies[1:]
			},
		},
		{
			name: "duplicate durable-boundary declaration",
			want: "duplicate durable boundary",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.DurableBoundaries = append(manifest.DurableBoundaries, manifest.DurableBoundaries[0])
			},
		},
		{
			name: "unknown durable-boundary declaration",
			want: "unknown durable boundary",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.DurableBoundaries[0] = "future-boundary"
			},
		},
		{
			name: "missing production durable boundary",
			want: "missing production durable boundary",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.DurableBoundaries = manifest.DurableBoundaries[1:]
			},
		},
		{
			name: "duplicate finalization-boundary declaration",
			want: "duplicate finalization boundary",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.FinalizationBoundaries = append(manifest.FinalizationBoundaries, manifest.FinalizationBoundaries[0])
			},
		},
		{
			name: "unknown finalization-boundary declaration",
			want: "unknown finalization boundary",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.FinalizationBoundaries[0] = "future-finalization"
			},
		},
		{
			name: "missing production finalization boundary",
			want: "missing production finalization boundary",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.FinalizationBoundaries = manifest.FinalizationBoundaries[1:]
			},
		},
		{
			name: "absent applicability slot",
			want: "missing applicability decision",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.Applicability = manifest.Applicability[1:]
			},
		},
		{
			name: "duplicate applicability slot",
			want: "duplicate applicability decision",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.Applicability = append(manifest.Applicability, manifest.Applicability[0])
			},
		},
		{
			name: "applicable slot without scenario",
			want: "has no failure scenario",
			mutate: func(manifest *recoverymatrix.Manifest) {
				for index, scenario := range manifest.Scenarios {
					if scenario.Kind == recoverymatrix.ScenarioFailure {
						manifest.Scenarios = append(manifest.Scenarios[:index], manifest.Scenarios[index+1:]...)
						return
					}
				}
			},
		},
		{
			name: "excluded slot without rationale",
			want: "blank rationale",
			mutate: func(manifest *recoverymatrix.Manifest) {
				for index := range manifest.Applicability {
					if !manifest.Applicability[index].Applicable {
						manifest.Applicability[index].Rationale = ""
						return
					}
				}
			},
		},
		{
			name: "excluded slot with scenario",
			want: "excluded applicability decision",
			mutate: func(manifest *recoverymatrix.Manifest) {
				scenario := manifest.Scenarios[0]
				scenario.ID = "failure/brainstorm/planner"
				scenario.References.FailureFamily = "planner"
				manifest.Scenarios = append(manifest.Scenarios, scenario)
			},
		},
		{
			name: "uncovered durable boundary",
			want: "has no restart scenario",
			mutate: func(manifest *recoverymatrix.Manifest) {
				for index, scenario := range manifest.Scenarios {
					if scenario.Kind == recoverymatrix.ScenarioRestart {
						manifest.Scenarios = append(manifest.Scenarios[:index], manifest.Scenarios[index+1:]...)
						return
					}
				}
			},
		},
		{
			name: "uncovered finalization boundary",
			want: "has no finalization scenario",
			mutate: func(manifest *recoverymatrix.Manifest) {
				for index, scenario := range manifest.Scenarios {
					if scenario.Kind == recoverymatrix.ScenarioFinalization {
						manifest.Scenarios = append(manifest.Scenarios[:index], manifest.Scenarios[index+1:]...)
						return
					}
				}
			},
		},
		{
			name: "missing required cross-cutting kind",
			want: "missing cross-cutting scenario",
			mutate: func(manifest *recoverymatrix.Manifest) {
				for index, scenario := range manifest.Scenarios {
					if strings.HasPrefix(scenario.ID, "cross-cutting/") {
						manifest.Scenarios = append(manifest.Scenarios[:index], manifest.Scenarios[index+1:]...)
						return
					}
				}
			},
		},
		{
			name: "duplicate scenario ID",
			want: "duplicate scenario",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.Scenarios = append(manifest.Scenarios, manifest.Scenarios[0])
			},
		},
		{
			name: "unresolvable scenario reference",
			want: "unknown stage reference",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.Scenarios[0].References.Stage = "unresolvable-stage"
			},
		},
		{
			name: "missing required scenario reference",
			want: "requires stage and failure family references",
			mutate: func(manifest *recoverymatrix.Manifest) {
				manifest.Scenarios[0].References.Stage = ""
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := cloneManifest(t, base)
			test.mutate(&manifest)
			_, err := recoverymatrix.ValidateManifest(manifest, production)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestShippedManifestMatchesProduction(t *testing.T) {
	manifest := loadShippedManifest(t)
	validated, err := recoverymatrix.ValidateManifest(manifest, productionInventory(t))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := recoverymatrix.ManifestIdentity(validated)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(identity, "sha256:") || len(identity) != len("sha256:")+64 {
		t.Fatalf("manifest identity = %q", identity)
	}

	if got := len(manifest.Stages); got != 8 {
		t.Fatalf("stage declarations = %d, want 8", got)
	}
	if got := len(manifest.FailureFamilies); got != 11 {
		t.Fatalf("failure-family declarations = %d, want 11", got)
	}
	if got := len(manifest.Applicability); got != 88 {
		t.Fatalf("applicability decisions = %d, want 88", got)
	}
	if got := len(manifest.DurableBoundaries); got != 6 {
		t.Fatalf("durable-boundary declarations = %d, want 6", got)
	}
	if got := len(manifest.FinalizationBoundaries); got != 6 {
		t.Fatalf("finalization-boundary declarations = %d, want 6", got)
	}
	crossCutting := 0
	for _, scenario := range manifest.Scenarios {
		if strings.HasPrefix(scenario.ID, "cross-cutting/") {
			crossCutting++
		}
	}
	if crossCutting != 8 {
		t.Fatalf("cross-cutting scenario kinds = %d, want 8", crossCutting)
	}
}

func TestManifestIdentityIsSemantic(t *testing.T) {
	production := productionInventory(t)
	original := loadShippedManifest(t)
	originalSnapshot := cloneManifest(t, original)
	validatedOriginal, err := recoverymatrix.ValidateManifest(original, production)
	if err != nil {
		t.Fatal(err)
	}
	originalIdentity, err := recoverymatrix.ManifestIdentity(validatedOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, originalSnapshot) {
		t.Fatal("validation or identity mutated the caller's manifest")
	}

	reordered := cloneManifest(t, original)
	slices.Reverse(reordered.Stages)
	slices.Reverse(reordered.FailureFamilies)
	slices.Reverse(reordered.DurableBoundaries)
	slices.Reverse(reordered.FinalizationBoundaries)
	slices.Reverse(reordered.Applicability)
	slices.Reverse(reordered.Scenarios)
	for index := range reordered.Scenarios {
		slices.Reverse(reordered.Scenarios[index].AllowedEffects)
		slices.Reverse(reordered.Scenarios[index].Expected.ArtifactIdentities)
	}
	validatedReordered, err := recoverymatrix.ValidateManifest(reordered, production)
	if err != nil {
		t.Fatal(err)
	}
	reorderedIdentity, err := recoverymatrix.ManifestIdentity(validatedReordered)
	if err != nil {
		t.Fatal(err)
	}
	if reorderedIdentity != originalIdentity {
		t.Fatalf("reordered identity = %q, want %q", reorderedIdentity, originalIdentity)
	}

	changed := cloneManifest(t, original)
	changedRationale := false
	for index := range changed.Applicability {
		if !changed.Applicability[index].Applicable {
			changed.Applicability[index].Rationale += " with a changed semantic reason"
			changedRationale = true
			break
		}
	}
	if !changedRationale {
		t.Fatal("shipped manifest has no exclusion rationale to change")
	}
	validatedChanged, err := recoverymatrix.ValidateManifest(changed, production)
	if err != nil {
		t.Fatal(err)
	}
	changedIdentity, err := recoverymatrix.ManifestIdentity(validatedChanged)
	if err != nil {
		t.Fatal(err)
	}
	if changedIdentity == originalIdentity {
		t.Fatalf("changed rationale retained identity %q", changedIdentity)
	}
}

func productionInventory(t *testing.T) recoverymatrix.ProductionInventory {
	t.Helper()
	productionFlow, err := flow.Load("../scaffold/defaults/flows/default.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return recoverymatrix.ResolveProduction(productionFlow)
}

func loadShippedManifest(t *testing.T) recoverymatrix.Manifest {
	t.Helper()
	manifest, err := recoverymatrix.LoadManifest(shippedManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func cloneManifest(t *testing.T, manifest recoverymatrix.Manifest) recoverymatrix.Manifest {
	t.Helper()
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var clone recoverymatrix.Manifest
	if err := json.Unmarshal(body, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func writeManifest(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
