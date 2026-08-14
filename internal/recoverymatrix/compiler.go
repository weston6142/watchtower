package recoverymatrix

import (
	"fmt"
	"sort"
)

// Compile converts a validated manifest into a stable, deeply copied inventory.
func Compile(validated ValidatedManifest) ([]Scenario, error) {
	if validated.manifest.SchemaVersion == 0 {
		return nil, fmt.Errorf("manifest has not been validated")
	}

	compiled := make([]Scenario, 0, len(validated.manifest.Scenarios))
	seen := make(map[string]struct{}, len(validated.manifest.Scenarios))
	for _, declaration := range validated.manifest.Scenarios {
		want, err := semanticScenarioID(declaration)
		if err != nil {
			return nil, fmt.Errorf("scenario %q: %w", declaration.ID, err)
		}
		if declaration.ID != want {
			return nil, fmt.Errorf("scenario ID %q does not match semantic ID %q", declaration.ID, want)
		}
		if _, exists := seen[declaration.ID]; exists {
			return nil, fmt.Errorf("duplicate generated scenario ID %q", declaration.ID)
		}
		seen[declaration.ID] = struct{}{}
		compiled = append(compiled, Scenario{
			ID:             declaration.ID,
			Kind:           declaration.Kind,
			References:     declaration.References,
			DriverID:       declaration.DriverID,
			Seed:           declaration.Seed,
			InitialInputs:  cloneMap(declaration.InitialInputs),
			RecoveryInputs: cloneMap(declaration.RecoveryInputs),
			AllowedEffects: append([]string(nil), declaration.AllowedEffects...),
			Expected: ExpectedContract{
				PublicOutcome:            declaration.Expected.PublicOutcome,
				DurableState:             declaration.Expected.DurableState,
				NormalizedClassification: declaration.Expected.NormalizedClassification,
				ArtifactIdentities:       append([]string(nil), declaration.Expected.ArtifactIdentities...),
				RecoveryBehavior:         declaration.Expected.RecoveryBehavior,
			},
		})
	}

	sort.Slice(compiled, func(left, right int) bool { return compiled[left].ID < compiled[right].ID })
	return compiled, nil
}

func semanticScenarioID(declaration ScenarioDeclaration) (string, error) {
	switch declaration.Kind {
	case ScenarioFailure:
		if declaration.References.Stage == "" || declaration.References.FailureFamily == "" {
			return "", fmt.Errorf("failure scenario requires stage and failure family references")
		}
		return "failure/" + declaration.References.Stage + "/" + declaration.References.FailureFamily, nil
	case ScenarioRestart:
		if declaration.References.Stage == "" || declaration.References.DurableBoundary == "" {
			return "", fmt.Errorf("restart scenario requires stage and durable boundary references")
		}
		return "restart/" + declaration.References.Stage + "/" + declaration.References.DurableBoundary, nil
	case ScenarioFinalization:
		if declaration.References.FinalizationBoundary == "" {
			return "", fmt.Errorf("finalization scenario requires a finalization boundary reference")
		}
		return "finalization/" + declaration.References.FinalizationBoundary, nil
	default:
		if !isCrossCuttingKind(declaration.Kind) {
			return "", fmt.Errorf("unknown scenario kind %q", declaration.Kind)
		}
		return "cross-cutting/" + string(declaration.Kind), nil
	}
}
