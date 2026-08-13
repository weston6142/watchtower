package recoverymatrix

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
	"github.com/weston6142/watchtower/internal/store"
	"gopkg.in/yaml.v3"
)

var requiredCrossCuttingKinds = []ScenarioKind{
	ScenarioUnchangedRetryRefusal,
	ScenarioChangedStateRecovery,
	ScenarioPlannerCapabilityLoss,
	ScenarioConfigurationDrift,
	ScenarioCapabilityViolation,
	ScenarioDecisionEscalation,
	ScenarioArtifactIdentity,
	ScenarioFinalizationRecovery,
}

func LoadManifest(path string) (Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode coverage manifest: %w", err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return Manifest{}, fmt.Errorf("decode trailing YAML document: %w", err)
		}
		return Manifest{}, fmt.Errorf("coverage manifest contains a trailing YAML document")
	}
	return manifest, nil
}

// ResolveProduction derives the manifest reconciliation inventory from the
// supplied production flow and the packages that own the other taxonomies.
func ResolveProduction(productionFlow flow.Flow) ProductionInventory {
	failureFamilies := make([]string, 0, len(failure.CanonicalSites()))
	for _, site := range failure.CanonicalSites() {
		failureFamilies = append(failureFamilies, string(site))
	}
	durableBoundaries := make([]string, 0, len(stagelifecycle.DurableBoundaries()))
	for _, boundary := range stagelifecycle.DurableBoundaries() {
		durableBoundaries = append(durableBoundaries, string(boundary))
	}
	return ProductionInventory{
		Stages:                 productionFlow.StageNames(),
		FailureFamilies:        failureFamilies,
		DurableBoundaries:      durableBoundaries,
		FinalizationBoundaries: store.FinalizationBoundaries(),
	}
}

func ValidateManifest(manifest Manifest, production ProductionInventory) (ValidatedManifest, error) {
	if manifest.SchemaVersion != SchemaVersion {
		return ValidatedManifest{}, fmt.Errorf("coverage manifest schema version %d is not supported", manifest.SchemaVersion)
	}
	if err := validateSetEquality("stage", manifest.Stages, production.Stages); err != nil {
		return ValidatedManifest{}, err
	}
	if err := validateSetEquality("failure family", manifest.FailureFamilies, production.FailureFamilies); err != nil {
		return ValidatedManifest{}, err
	}
	if err := validateSetEquality("durable boundary", manifest.DurableBoundaries, production.DurableBoundaries); err != nil {
		return ValidatedManifest{}, err
	}
	if err := validateSetEquality("finalization boundary", manifest.FinalizationBoundaries, production.FinalizationBoundaries); err != nil {
		return ValidatedManifest{}, err
	}

	stages := toSet(production.Stages)
	failureFamilies := toSet(production.FailureFamilies)
	durableBoundaries := toSet(production.DurableBoundaries)
	finalizationBoundaries := toSet(production.FinalizationBoundaries)

	applicability := make(map[string]ApplicabilityDecision, len(manifest.Applicability))
	for _, decision := range manifest.Applicability {
		if _, ok := stages[decision.Stage]; !ok {
			return ValidatedManifest{}, fmt.Errorf("applicability decision has unknown stage reference %q", decision.Stage)
		}
		if _, ok := failureFamilies[decision.FailureFamily]; !ok {
			return ValidatedManifest{}, fmt.Errorf("applicability decision has unknown failure family reference %q", decision.FailureFamily)
		}
		key := decision.Stage + "\x00" + decision.FailureFamily
		if _, ok := applicability[key]; ok {
			return ValidatedManifest{}, fmt.Errorf("duplicate applicability decision for stage %q and failure family %q", decision.Stage, decision.FailureFamily)
		}
		if !decision.Applicable && strings.TrimSpace(decision.Rationale) == "" {
			return ValidatedManifest{}, fmt.Errorf("excluded applicability decision for stage %q and failure family %q has blank rationale", decision.Stage, decision.FailureFamily)
		}
		applicability[key] = decision
	}
	for _, stage := range production.Stages {
		for _, family := range production.FailureFamilies {
			key := stage + "\x00" + family
			if _, ok := applicability[key]; !ok {
				return ValidatedManifest{}, fmt.Errorf("missing applicability decision for stage %q and failure family %q", stage, family)
			}
		}
	}

	scenarioIDs := make(map[string]struct{}, len(manifest.Scenarios))
	failureScenarios := make(map[string]struct{})
	restartScenarios := make(map[string]struct{})
	finalizationScenarios := make(map[string]struct{})
	crossCuttingScenarios := make(map[ScenarioKind]struct{})
	for _, scenario := range manifest.Scenarios {
		if strings.TrimSpace(scenario.ID) == "" {
			return ValidatedManifest{}, fmt.Errorf("scenario has a blank semantic ID")
		}
		if _, ok := scenarioIDs[scenario.ID]; ok {
			return ValidatedManifest{}, fmt.Errorf("duplicate scenario ID %q", scenario.ID)
		}
		scenarioIDs[scenario.ID] = struct{}{}
		if err := validateReferences(scenario, stages, failureFamilies, durableBoundaries, finalizationBoundaries); err != nil {
			return ValidatedManifest{}, fmt.Errorf("scenario %q: %w", scenario.ID, err)
		}
		if err := validateScenarioDeclaration(scenario); err != nil {
			return ValidatedManifest{}, fmt.Errorf("scenario %q: %w", scenario.ID, err)
		}

		switch scenario.Kind {
		case ScenarioFailure:
			wantID := "failure/" + scenario.References.Stage + "/" + scenario.References.FailureFamily
			if scenario.ID != wantID {
				return ValidatedManifest{}, fmt.Errorf("failure scenario ID %q does not match semantic ID %q", scenario.ID, wantID)
			}
			key := scenario.References.Stage + "\x00" + scenario.References.FailureFamily
			if !applicability[key].Applicable {
				return ValidatedManifest{}, fmt.Errorf("failure scenario %q contradicts its excluded applicability decision", scenario.ID)
			}
			if _, ok := failureScenarios[key]; ok {
				return ValidatedManifest{}, fmt.Errorf("duplicate failure scenario for stage %q and failure family %q", scenario.References.Stage, scenario.References.FailureFamily)
			}
			failureScenarios[key] = struct{}{}
		case ScenarioRestart:
			wantID := "restart/" + scenario.References.Stage + "/" + scenario.References.DurableBoundary
			if scenario.ID != wantID {
				return ValidatedManifest{}, fmt.Errorf("restart scenario ID %q does not match semantic ID %q", scenario.ID, wantID)
			}
			key := scenario.References.Stage + "\x00" + scenario.References.DurableBoundary
			if _, ok := restartScenarios[key]; ok {
				return ValidatedManifest{}, fmt.Errorf("duplicate restart scenario for stage %q and durable boundary %q", scenario.References.Stage, scenario.References.DurableBoundary)
			}
			restartScenarios[key] = struct{}{}
		case ScenarioFinalization:
			wantID := "finalization/" + scenario.References.FinalizationBoundary
			if scenario.ID != wantID {
				return ValidatedManifest{}, fmt.Errorf("finalization scenario ID %q does not match semantic ID %q", scenario.ID, wantID)
			}
			if _, ok := finalizationScenarios[scenario.References.FinalizationBoundary]; ok {
				return ValidatedManifest{}, fmt.Errorf("duplicate finalization scenario for boundary %q", scenario.References.FinalizationBoundary)
			}
			finalizationScenarios[scenario.References.FinalizationBoundary] = struct{}{}
		default:
			if !isCrossCuttingKind(scenario.Kind) {
				return ValidatedManifest{}, fmt.Errorf("unknown scenario kind %q", scenario.Kind)
			}
			wantID := "cross-cutting/" + string(scenario.Kind)
			if scenario.ID != wantID {
				return ValidatedManifest{}, fmt.Errorf("cross-cutting scenario ID %q does not match semantic ID %q", scenario.ID, wantID)
			}
			if _, ok := crossCuttingScenarios[scenario.Kind]; ok {
				return ValidatedManifest{}, fmt.Errorf("duplicate cross-cutting scenario kind %q", scenario.Kind)
			}
			crossCuttingScenarios[scenario.Kind] = struct{}{}
		}
	}

	for _, decision := range manifest.Applicability {
		if !decision.Applicable {
			continue
		}
		key := decision.Stage + "\x00" + decision.FailureFamily
		if _, ok := failureScenarios[key]; !ok {
			return ValidatedManifest{}, fmt.Errorf("applicable stage %q and failure family %q has no failure scenario", decision.Stage, decision.FailureFamily)
		}
	}
	for _, stage := range production.Stages {
		for _, boundary := range production.DurableBoundaries {
			key := stage + "\x00" + boundary
			if _, ok := restartScenarios[key]; !ok {
				return ValidatedManifest{}, fmt.Errorf("stage %q durable boundary %q has no restart scenario", stage, boundary)
			}
		}
	}
	for _, boundary := range production.FinalizationBoundaries {
		if _, ok := finalizationScenarios[boundary]; !ok {
			return ValidatedManifest{}, fmt.Errorf("finalization boundary %q has no finalization scenario", boundary)
		}
	}
	for _, kind := range requiredCrossCuttingKinds {
		if _, ok := crossCuttingScenarios[kind]; !ok {
			return ValidatedManifest{}, fmt.Errorf("missing cross-cutting scenario kind %q", kind)
		}
	}

	return ValidatedManifest{manifest: cloneManifest(manifest)}, nil
}

func ManifestIdentity(validated ValidatedManifest) (string, error) {
	canonical := cloneManifest(validated.manifest)
	if canonical.SchemaVersion == 0 {
		return "", fmt.Errorf("manifest has not been validated")
	}
	sort.Strings(canonical.Stages)
	sort.Strings(canonical.FailureFamilies)
	sort.Strings(canonical.DurableBoundaries)
	sort.Strings(canonical.FinalizationBoundaries)
	sort.Slice(canonical.Applicability, func(left, right int) bool {
		return applicabilityKey(canonical.Applicability[left]) < applicabilityKey(canonical.Applicability[right])
	})
	for index := range canonical.Scenarios {
		sort.Strings(canonical.Scenarios[index].AllowedEffects)
		sort.Strings(canonical.Scenarios[index].Expected.ArtifactIdentities)
	}
	sort.Slice(canonical.Scenarios, func(left, right int) bool {
		return canonical.Scenarios[left].ID < canonical.Scenarios[right].ID
	})
	body, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshal canonical coverage manifest: %w", err)
	}
	digest := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func validateSetEquality(name string, declared, production []string) error {
	declaredSet := make(map[string]struct{}, len(declared))
	productionSet := make(map[string]struct{}, len(production))
	for _, value := range production {
		if _, ok := productionSet[value]; ok {
			return fmt.Errorf("duplicate production %s %q", name, value)
		}
		productionSet[value] = struct{}{}
	}
	for _, value := range declared {
		if _, ok := declaredSet[value]; ok {
			return fmt.Errorf("duplicate %s declaration %q", name, value)
		}
		declaredSet[value] = struct{}{}
		if _, ok := productionSet[value]; !ok {
			return fmt.Errorf("unknown %s declaration %q", name, value)
		}
	}
	for _, value := range production {
		if _, ok := declaredSet[value]; !ok {
			return fmt.Errorf("missing production %s %q", name, value)
		}
	}
	return nil
}

func validateReferences(scenario ScenarioDeclaration, stages, failureFamilies, durableBoundaries, finalizationBoundaries map[string]struct{}) error {
	if reference := scenario.References.Stage; reference != "" {
		if _, ok := stages[reference]; !ok {
			return fmt.Errorf("unknown stage reference %q", reference)
		}
	}
	if reference := scenario.References.FailureFamily; reference != "" {
		if _, ok := failureFamilies[reference]; !ok {
			return fmt.Errorf("unknown failure family reference %q", reference)
		}
	}
	if reference := scenario.References.DurableBoundary; reference != "" {
		if _, ok := durableBoundaries[reference]; !ok {
			return fmt.Errorf("unknown durable boundary reference %q", reference)
		}
	}
	if reference := scenario.References.FinalizationBoundary; reference != "" {
		if _, ok := finalizationBoundaries[reference]; !ok {
			return fmt.Errorf("unknown finalization boundary reference %q", reference)
		}
	}
	return nil
}

func validateScenarioDeclaration(scenario ScenarioDeclaration) error {
	switch scenario.Kind {
	case ScenarioFailure:
		if scenario.References.Stage == "" || scenario.References.FailureFamily == "" {
			return fmt.Errorf("failure scenario requires stage and failure family references")
		}
	case ScenarioRestart:
		if scenario.References.Stage == "" || scenario.References.DurableBoundary == "" {
			return fmt.Errorf("restart scenario requires stage and durable boundary references")
		}
	case ScenarioFinalization:
		if scenario.References.FinalizationBoundary == "" {
			return fmt.Errorf("finalization scenario requires a finalization boundary reference")
		}
	default:
		if scenario.References == (ProductionReferences{}) {
			return fmt.Errorf("cross-cutting scenario requires at least one production reference")
		}
	}
	if strings.TrimSpace(scenario.DriverID) == "" {
		return fmt.Errorf("driver ID is blank")
	}
	if scenario.Seed == 0 {
		return fmt.Errorf("deterministic seed is zero")
	}
	if len(scenario.InitialInputs) == 0 {
		return fmt.Errorf("declared initial inputs are empty")
	}
	if len(scenario.RecoveryInputs) == 0 {
		return fmt.Errorf("declared recovery inputs are empty")
	}
	if len(scenario.AllowedEffects) == 0 {
		return fmt.Errorf("allowed effects are empty")
	}
	if strings.TrimSpace(scenario.Expected.PublicOutcome) == "" ||
		strings.TrimSpace(scenario.Expected.DurableState) == "" ||
		strings.TrimSpace(scenario.Expected.NormalizedClassification) == "" ||
		len(scenario.Expected.ArtifactIdentities) == 0 ||
		strings.TrimSpace(scenario.Expected.RecoveryBehavior) == "" {
		return fmt.Errorf("expected contract is incomplete")
	}
	return nil
}

func isCrossCuttingKind(kind ScenarioKind) bool {
	for _, required := range requiredCrossCuttingKinds {
		if kind == required {
			return true
		}
	}
	return false
}

func applicabilityKey(decision ApplicabilityDecision) string {
	return decision.Stage + "\x00" + decision.FailureFamily
}

func toSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func cloneManifest(manifest Manifest) Manifest {
	clone := manifest
	clone.Stages = append([]string(nil), manifest.Stages...)
	clone.FailureFamilies = append([]string(nil), manifest.FailureFamilies...)
	clone.DurableBoundaries = append([]string(nil), manifest.DurableBoundaries...)
	clone.FinalizationBoundaries = append([]string(nil), manifest.FinalizationBoundaries...)
	clone.Applicability = append([]ApplicabilityDecision(nil), manifest.Applicability...)
	clone.Scenarios = make([]ScenarioDeclaration, len(manifest.Scenarios))
	for index, scenario := range manifest.Scenarios {
		clone.Scenarios[index] = scenario
		clone.Scenarios[index].InitialInputs = cloneMap(scenario.InitialInputs)
		clone.Scenarios[index].RecoveryInputs = cloneMap(scenario.RecoveryInputs)
		clone.Scenarios[index].AllowedEffects = append([]string(nil), scenario.AllowedEffects...)
		clone.Scenarios[index].Expected.ArtifactIdentities = append([]string(nil), scenario.Expected.ArtifactIdentities...)
	}
	return clone
}

func cloneMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}
