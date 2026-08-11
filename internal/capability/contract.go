package capability

import (
	"fmt"
	"strings"
)

const ContractVersion = 1
const EnginePolicyVersion = "1"

type OperationClass string

const (
	OpWorkspaceRead        OperationClass = "workspace-read"
	OpWorkspaceMutate      OperationClass = "workspace-mutate"
	OpLocalProcess         OperationClass = "local-process"
	OpVCSRead              OperationClass = "vcs-read"
	OpVCSCommit            OperationClass = "vcs-commit"
	OpPlannerArtifactApply OperationClass = "planner-artifact-apply"
)

type MutationClass string

const (
	MutationCreate   MutationClass = "create"
	MutationModify   MutationClass = "modify"
	MutationDelete   MutationClass = "delete"
	MutationRename   MutationClass = "rename"
	MutationMetadata MutationClass = "metadata"
	MutationLink     MutationClass = "link"
)

type OutputOwner string

const (
	OwnerAgent  OutputOwner = "agent"
	OwnerEngine OutputOwner = "engine"
)

type FailureReason string

const (
	ReasonContractInvalid     FailureReason = "capability_contract_invalid"
	ReasonProviderUnsupported FailureReason = "capability_provider_unsupported"
	ReasonRuntimeDenied       FailureReason = "capability_runtime_denied"
	ReasonPostStageViolation  FailureReason = "capability_post_stage_violation"
)

type RepositoryIdentity struct {
	IssueID     string `json:"issue_id,omitempty"`
	Canonical   string `json:"canonical,omitempty"`
	Branch      string `json:"branch"`
	BaseCommit  string `json:"base_commit"`
	StartCommit string `json:"start_commit"`
	Tree        string `json:"tree"`
}

type ApprovalBinding struct {
	Approved       bool   `json:"approved"`
	TouchsetDigest string `json:"touchset_digest"`
	DecisionID     string `json:"decision_id"`
	ArtifactID     string `json:"artifact_id"`
	AttemptID      string `json:"attempt_id,omitempty"`
}

type PathGrant struct {
	Path      string          `json:"path"`
	Mutations []MutationClass `json:"mutations"`
}

type RequiredOutput struct {
	Path  string      `json:"path"`
	Owner OutputOwner `json:"owner"`
}

type Contract struct {
	Version             int                `json:"version"`
	EnginePolicyVersion string             `json:"engine_policy_version"`
	IssueID             string             `json:"issue_id"`
	Stage               string             `json:"stage"`
	AttemptID           string             `json:"attempt_id"`
	Profile             string             `json:"profile"`
	WorkspaceRoot       string             `json:"workspace_root"`
	Repository          RepositoryIdentity `json:"repository"`
	Reads               []string           `json:"reads"`
	Writes              []PathGrant        `json:"writes"`
	Operations          []OperationClass   `json:"operations"`
	Outputs             []RequiredOutput   `json:"outputs"`
	Approval            *ApprovalBinding   `json:"approval,omitempty"`
}

type CompiledContract struct {
	Contract        Contract `json:"contract"`
	ContractID      string   `json:"contract_id"`
	AuthorityDigest string   `json:"authority_digest"`
}

type EnforcementControl string

const (
	ControlWorkspaceRead  EnforcementControl = "workspace-read"
	ControlWorkspaceWrite EnforcementControl = "workspace-write"
	ControlDescendants    EnforcementControl = "descendants"
	ControlNetwork        EnforcementControl = "network"
	ControlGitCommonDir   EnforcementControl = "git-common-dir"
	ControlEngineState    EnforcementControl = "engine-state"
	ControlScratch        EnforcementControl = "scratch"
	ControlProcessGroup   EnforcementControl = "process-group"
)

type ControlProof struct {
	Control EnforcementControl `json:"control"`
	Proven  bool               `json:"proven"`
}

type EnforcementPlan struct {
	ContractID     string         `json:"contract_id"`
	Provider       string         `json:"provider"`
	Implementation string         `json:"implementation"`
	Version        string         `json:"version"`
	Controls       []ControlProof `json:"controls"`
	PlanID         string         `json:"plan_id"`
}

type AttemptIdentity struct {
	IssueID   string `json:"issue_id"`
	Stage     string `json:"stage"`
	AttemptID string `json:"attempt_id"`
}

type BaselineIdentity struct {
	Digest string `json:"digest"`
}

type ValidationResult struct {
	Passed       bool   `json:"passed"`
	ResultDigest string `json:"result_digest,omitempty"`
	DeltaDigest  string `json:"delta_digest,omitempty"`
}

type AuditRecord struct {
	Attempt        AttemptIdentity `json:"attempt"`
	ContractID     string          `json:"contract_id,omitempty"`
	Phase          string          `json:"phase"`
	Outcome        string          `json:"outcome"`
	Reason         FailureReason   `json:"reason,omitempty"`
	Provider       string          `json:"provider,omitempty"`
	Implementation string          `json:"implementation,omitempty"`
	Operation      OperationClass  `json:"operation,omitempty"`
	Paths          []string        `json:"paths,omitempty"`
	Diagnostic     string          `json:"diagnostic,omitempty"`
}

type AttemptRecord struct {
	Identity          AttemptIdentity  `json:"identity"`
	SchemaVersion     int              `json:"schema_version"`
	Contract          CompiledContract `json:"contract"`
	Plan              EnforcementPlan  `json:"plan,omitempty"`
	Baseline          BaselineIdentity `json:"baseline,omitempty"`
	Validation        ValidationResult `json:"validation,omitempty"`
	ImmutableResultID string           `json:"immutable_result_id,omitempty"`
}

// PolicyError contains stable, redacted policy facts only.
type PolicyError struct {
	Phase      string
	Reason     FailureReason
	Operation  OperationClass
	Paths      []string
	Provider   string
	Diagnostic string
}

func (e *PolicyError) Error() string {
	parts := []string{string(e.Reason)}
	if e.Phase != "" {
		parts = append(parts, "phase="+e.Phase)
	}
	if e.Operation != "" {
		parts = append(parts, "operation="+string(e.Operation))
	}
	if len(e.Paths) > 0 {
		parts = append(parts, "paths="+strings.Join(e.Paths, ","))
	}
	if e.Provider != "" {
		parts = append(parts, "provider="+e.Provider)
	}
	if e.Diagnostic != "" {
		parts = append(parts, "diagnostic="+e.Diagnostic)
	}
	return strings.Join(parts, " ")
}

func invalid(format string, args ...any) error {
	return &PolicyError{Phase: "compile", Reason: ReasonContractInvalid, Diagnostic: fmt.Sprintf(format, args...)}
}
