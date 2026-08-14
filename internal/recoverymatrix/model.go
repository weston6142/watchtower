package recoverymatrix

const SchemaVersion = 1

type ScenarioKind string

const (
	ScenarioFailure      ScenarioKind = "failure"
	ScenarioRestart      ScenarioKind = "restart"
	ScenarioFinalization ScenarioKind = "finalization"

	ScenarioUnchangedRetryRefusal ScenarioKind = "unchanged-retry-refusal"
	ScenarioChangedStateRecovery  ScenarioKind = "changed-state-recovery"
	ScenarioPlannerCapabilityLoss ScenarioKind = "planner-capability-loss"
	ScenarioConfigurationDrift    ScenarioKind = "configuration-drift"
	ScenarioCapabilityViolation   ScenarioKind = "capability-violation"
	ScenarioDecisionEscalation    ScenarioKind = "decision-escalation"
	ScenarioArtifactIdentity      ScenarioKind = "artifact-identity"
	ScenarioFinalizationRecovery  ScenarioKind = "finalization-recovery"
)

type Manifest struct {
	SchemaVersion          int                     `json:"schema_version" yaml:"schema_version"`
	Stages                 []string                `json:"stages" yaml:"stages"`
	FailureFamilies        []string                `json:"failure_families" yaml:"failure_families"`
	DurableBoundaries      []string                `json:"durable_boundaries" yaml:"durable_boundaries"`
	FinalizationBoundaries []string                `json:"finalization_boundaries" yaml:"finalization_boundaries"`
	Applicability          []ApplicabilityDecision `json:"applicability" yaml:"applicability"`
	Scenarios              []ScenarioDeclaration   `json:"scenarios" yaml:"scenarios"`
}

type ApplicabilityDecision struct {
	Stage         string `json:"stage" yaml:"stage"`
	FailureFamily string `json:"failure_family" yaml:"failure_family"`
	Applicable    bool   `json:"applicable" yaml:"applicable"`
	Rationale     string `json:"rationale" yaml:"rationale"`
}

type ScenarioDeclaration struct {
	ID             string               `json:"id" yaml:"id"`
	Kind           ScenarioKind         `json:"kind" yaml:"kind"`
	References     ProductionReferences `json:"references" yaml:"references"`
	DriverID       string               `json:"driver_id" yaml:"driver_id"`
	Seed           int64                `json:"seed" yaml:"seed"`
	InitialInputs  map[string]string    `json:"initial_inputs" yaml:"initial_inputs"`
	RecoveryInputs map[string]string    `json:"recovery_inputs" yaml:"recovery_inputs"`
	AllowedEffects []string             `json:"allowed_effects" yaml:"allowed_effects"`
	Expected       ExpectedContract     `json:"expected" yaml:"expected"`
}

type ProductionReferences struct {
	Stage                string `json:"stage" yaml:"stage"`
	FailureFamily        string `json:"failure_family" yaml:"failure_family"`
	DurableBoundary      string `json:"durable_boundary" yaml:"durable_boundary"`
	FinalizationBoundary string `json:"finalization_boundary" yaml:"finalization_boundary"`
}

type ExpectedContract struct {
	PublicOutcome            string   `json:"public_outcome" yaml:"public_outcome"`
	DurableState             string   `json:"durable_state" yaml:"durable_state"`
	NormalizedClassification string   `json:"normalized_classification" yaml:"normalized_classification"`
	ArtifactIdentities       []string `json:"artifact_identities" yaml:"artifact_identities"`
	RecoveryBehavior         string   `json:"recovery_behavior" yaml:"recovery_behavior"`
}

type ProductionInventory struct {
	Stages                 []string
	FailureFamilies        []string
	DurableBoundaries      []string
	FinalizationBoundaries []string
}

// ValidatedManifest is the only manifest form accepted by executable matrix
// stages. Its contents can be constructed only by ValidateManifest.
type ValidatedManifest struct {
	manifest Manifest
}

// Scenario is an immutable executable copy of a validated manifest declaration.
type Scenario struct {
	ID             string
	Kind           ScenarioKind
	References     ProductionReferences
	DriverID       string
	Seed           int64
	InitialInputs  map[string]string
	RecoveryInputs map[string]string
	AllowedEffects []string
	Expected       ExpectedContract
}

type FailureClass string

const (
	FailureMatrixDefinition      FailureClass = "matrix-definition"
	FailureHarnessInfrastructure FailureClass = "harness-infrastructure"
	FailureWorkflowContract      FailureClass = "workflow-contract"
)

type ResultStatus string

const (
	ResultPassed  ResultStatus = "passed"
	ResultFailed  ResultStatus = "failed"
	ResultSkipped ResultStatus = "skipped"
)

type Observation struct {
	PublicOutcome            string   `json:"public_outcome"`
	DurableState             string   `json:"durable_state"`
	NormalizedClassification string   `json:"normalized_classification"`
	ArtifactIdentities       []string `json:"artifact_identities"`
	Effects                  []string `json:"effects"`
	DiagnosticCheckpoints    []string `json:"diagnostic_checkpoints,omitempty"`
}

type ScenarioResult struct {
	ScenarioID   string       `json:"scenario_id"`
	Status       ResultStatus `json:"status"`
	FailureClass FailureClass `json:"failure_class,omitempty"`
	Failure      string       `json:"failure,omitempty"`
	Observation  Observation  `json:"observation"`
}

type RunSummary struct {
	ManifestIdentity  string           `json:"manifest_identity"`
	Revision          string           `json:"revision"`
	Compiled          int              `json:"compiled"`
	Executed          int              `json:"executed"`
	Passed            int              `json:"passed"`
	Failed            int              `json:"failed"`
	Skipped           int              `json:"skipped"`
	Filtered          bool             `json:"filtered"`
	Partial           bool             `json:"partial"`
	Panics            int              `json:"panics"`
	Timeouts          int              `json:"timeouts"`
	UnexpectedCalls   int              `json:"unexpected_calls"`
	UnconsumedScripts int              `json:"unconsumed_scripts"`
	MissingResults    int              `json:"missing_results"`
	Results           []ScenarioResult `json:"results"`
}

type RepositoryVerification struct {
	Revision string `json:"revision"`
	Passed   bool   `json:"passed"`
}

type ReceiptRun struct {
	Compiled int `json:"compiled"`
	Executed int `json:"executed"`
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
	Skipped  int `json:"skipped"`
}

type CompletionReceipt struct {
	ManifestIdentity             string        `json:"manifest_identity"`
	ProductionRevision           string        `json:"production_revision"`
	Runs                         [2]ReceiptRun `json:"runs"`
	MatrixResult                 string        `json:"matrix_result"`
	DeterminismResult            string        `json:"determinism_result"`
	RepositoryVerificationResult string        `json:"repository_verification_result"`
}
