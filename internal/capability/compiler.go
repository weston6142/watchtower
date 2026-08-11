package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/touchset"
)

type CompileInput struct {
	IssueID                 string
	Stage                   string
	AttemptID               string
	Profile                 flow.CapabilityProfile
	WorkspaceRoot           string
	Readonly                bool
	Repository              RepositoryIdentity
	MaterializedInputs      []string
	ReadableRepositoryPaths []string
	Outputs                 []RequiredOutput
	Approval                ApprovalBinding
	ApprovedTouchset        []string
	DocumentationPaths      []string
	ConflictPaths           []string
	LegacyRestrictions      pkgs.LegacyRestrictions
	PlannerArtifactRequired bool
	RequestedOperations     []string
}

var allMutations = []MutationClass{
	MutationCreate, MutationDelete, MutationLink, MutationMetadata, MutationModify, MutationRename,
}

func Compile(input CompileInput) (CompiledContract, error) {
	if input.IssueID == "" || input.Stage == "" || input.AttemptID == "" {
		return CompiledContract{}, invalid("issue, stage, and attempt identity are required")
	}
	if input.WorkspaceRoot == "" || !filepath.IsAbs(input.WorkspaceRoot) {
		return CompiledContract{}, invalid("workspace root must be absolute")
	}
	if !knownProfile(input.Profile) {
		return CompiledContract{}, invalid("unknown capability profile %q", input.Profile)
	}

	outputs, err := canonicalOutputs(input.Outputs)
	if err != nil {
		return CompiledContract{}, err
	}
	contract := Contract{
		Version: ContractVersion, EnginePolicyVersion: EnginePolicyVersion,
		IssueID: input.IssueID, Stage: input.Stage, AttemptID: input.AttemptID,
		Profile: string(input.Profile), WorkspaceRoot: filepath.Clean(input.WorkspaceRoot),
		Repository: input.Repository, Outputs: outputs,
	}

	if input.Profile == flow.ProfileArtifact {
		contract.Reads, err = canonicalPaths(append(append([]string(nil), input.MaterializedInputs...), outputPaths(outputs)...))
		if err != nil {
			return CompiledContract{}, err
		}
		contract.Operations = []OperationClass{OpWorkspaceRead}
		contract.Writes = outputGrants(outputs)
	} else {
		contract.Reads, err = canonicalPaths(input.ReadableRepositoryPaths)
		if err != nil {
			return CompiledContract{}, err
		}
		contract.Operations = []OperationClass{OpLocalProcess, OpVCSRead, OpWorkspaceRead}
	}

	if profileNeedsApproval(input.Profile) {
		if !input.Approval.Approved || input.Approval.TouchsetDigest == "" || input.Approval.DecisionID == "" || input.Approval.ArtifactID == "" {
			return CompiledContract{}, invalid("profile %q requires approval-bound touchset authority", input.Profile)
		}
		approved, canonicalErr := touchset.CanonicalGlobs(input.ApprovedTouchset)
		if canonicalErr != nil || len(approved) == 0 {
			return CompiledContract{}, invalid("approved touchset is missing or unsafe")
		}
		input.ApprovedTouchset = approved
		binding := input.Approval
		contract.Approval = &binding
	}

	switch input.Profile {
	case flow.ProfileImplementation, flow.ProfileReview, flow.ProfileFinalReview:
		contract.Writes = grants(input.ApprovedTouchset)
	case flow.ProfileLibrarian:
		intersection, intersectionErr := intersectGlobs(input.ApprovedTouchset, input.DocumentationPaths)
		if intersectionErr != nil || len(intersection) == 0 {
			return CompiledContract{}, invalid("librarian scope has no approved documentation intersection")
		}
		contract.Writes = grants(intersection)
	case flow.ProfileConflictResolution:
		paths, conflictErr := approvedConflictPaths(input.ApprovedTouchset, input.ConflictPaths)
		if conflictErr != nil || len(paths) == 0 {
			return CompiledContract{}, invalid("conflict scope is missing or outside approved touchset")
		}
		contract.Writes = grants(paths)
	}

	if len(contract.Writes) > 0 {
		contract.Operations = append(contract.Operations, OpWorkspaceMutate)
		if input.Profile != flow.ProfileArtifact {
			contract.Operations = append(contract.Operations, OpVCSCommit)
		}
	}
	if input.PlannerArtifactRequired {
		if input.Profile != flow.ProfileArtifact {
			return CompiledContract{}, invalid("planner artifact apply is valid only for artifact profile")
		}
		contract.Operations = append(contract.Operations, OpPlannerArtifactApply)
	}

	if err := validateRequestedOperations(input.RequestedOperations, contract.Operations); err != nil {
		return CompiledContract{}, err
	}
	applyLegacyRestrictions(&contract, input.LegacyRestrictions)
	if input.Readonly {
		contract.Writes = nil
		contract.Operations = removeOperations(contract.Operations, OpWorkspaceMutate, OpVCSCommit, OpPlannerArtifactApply)
	}
	if hasAgentOutputs(outputs) && (!containsOperation(contract.Operations, OpWorkspaceMutate) || input.Readonly) {
		return CompiledContract{}, invalid("agent-owned output requires workspace mutation authority")
	}

	canonicalizeContract(&contract)
	contractBytes, err := json.Marshal(contract)
	if err != nil {
		return CompiledContract{}, fmt.Errorf("marshal capability contract: %w", err)
	}
	authority := contract
	authority.AttemptID = ""
	authorityBytes, err := json.Marshal(authority)
	if err != nil {
		return CompiledContract{}, fmt.Errorf("marshal capability authority: %w", err)
	}
	return CompiledContract{
		Contract: contract, ContractID: digest(contractBytes), AuthorityDigest: digest(authorityBytes),
	}, nil
}

func knownProfile(profile flow.CapabilityProfile) bool {
	switch profile {
	case flow.ProfileArtifact, flow.ProfileInspect, flow.ProfileImplementation, flow.ProfileReview,
		flow.ProfileLibrarian, flow.ProfileFinalReview, flow.ProfileConflictResolution:
		return true
	default:
		return false
	}
}

func profileNeedsApproval(profile flow.CapabilityProfile) bool {
	switch profile {
	case flow.ProfileImplementation, flow.ProfileReview, flow.ProfileLibrarian,
		flow.ProfileFinalReview, flow.ProfileConflictResolution:
		return true
	default:
		return false
	}
}

func canonicalPaths(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		canonical, err := touchset.CanonicalPath(value)
		if err != nil {
			return nil, invalid("unsafe path %q", value)
		}
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		result = append(result, canonical)
	}
	sort.Strings(result)
	return result, nil
}

func canonicalOutputs(values []RequiredOutput) ([]RequiredOutput, error) {
	seen := make(map[string]OutputOwner, len(values))
	result := make([]RequiredOutput, 0, len(values))
	for _, output := range values {
		canonical, err := touchset.CanonicalPath(output.Path)
		if err != nil {
			return nil, invalid("unsafe output path %q", output.Path)
		}
		if output.Owner != OwnerAgent && output.Owner != OwnerEngine {
			return nil, invalid("unknown output owner %q", output.Owner)
		}
		if canonical == "verification.json" && output.Owner != OwnerEngine {
			return nil, invalid("verification.json is engine-owned")
		}
		if owner, ok := seen[canonical]; ok {
			if owner != output.Owner {
				return nil, invalid("output %q has conflicting owners", canonical)
			}
			continue
		}
		seen[canonical] = output.Owner
		result = append(result, RequiredOutput{Path: canonical, Owner: output.Owner})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Path == result[j].Path {
			return result[i].Owner < result[j].Owner
		}
		return result[i].Path < result[j].Path
	})
	return result, nil
}

func outputPaths(outputs []RequiredOutput) []string {
	paths := make([]string, 0, len(outputs))
	for _, output := range outputs {
		paths = append(paths, output.Path)
	}
	return paths
}

func outputGrants(outputs []RequiredOutput) []PathGrant {
	paths := make([]string, 0, len(outputs))
	for _, output := range outputs {
		if output.Owner == OwnerAgent {
			paths = append(paths, output.Path)
		}
	}
	return grants(paths)
}

func grants(paths []string) []PathGrant {
	result := make([]PathGrant, 0, len(paths))
	for _, path := range paths {
		result = append(result, PathGrant{Path: path, Mutations: append([]MutationClass(nil), allMutations...)})
	}
	return result
}

func intersectGlobs(approved, declared []string) ([]string, error) {
	docs, err := touchset.CanonicalGlobs(declared)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, approval := range approved {
		for _, doc := range docs {
			if !touchset.Overlap(touchset.Set{Globs: []string{approval}}, touchset.Set{Globs: []string{doc}}) {
				continue
			}
			approvalPrefix, docPrefix := touchset.PrefixOf(approval), touchset.PrefixOf(doc)
			if len(docPrefix) > len(approvalPrefix) {
				result = append(result, doc)
			} else {
				result = append(result, approval)
			}
		}
	}
	return uniqueSorted(result), nil
}

func approvedConflictPaths(approved, conflicts []string) ([]string, error) {
	paths, err := canonicalPaths(conflicts)
	if err != nil {
		return nil, err
	}
	for _, conflict := range paths {
		matched := false
		for _, approval := range approved {
			ok, matchErr := touchset.Match(approval, conflict)
			if matchErr != nil {
				return nil, matchErr
			}
			matched = matched || ok
		}
		if !matched {
			return nil, fmt.Errorf("conflict %q is outside touchset", conflict)
		}
	}
	return paths, nil
}

func validateRequestedOperations(requested []string, granted []OperationClass) error {
	for _, raw := range requested {
		operation := OperationClass(raw)
		if !grantableOperation(operation) || !containsOperation(granted, operation) {
			return invalid("operation %q is unknown, engine-only, or not granted by profile", raw)
		}
	}
	return nil
}

func grantableOperation(operation OperationClass) bool {
	switch operation {
	case OpWorkspaceRead, OpWorkspaceMutate, OpLocalProcess, OpVCSRead, OpVCSCommit, OpPlannerArtifactApply:
		return true
	default:
		return false
	}
}

func applyLegacyRestrictions(contract *Contract, restrictions pkgs.LegacyRestrictions) {
	if !restrictions.Declared {
		return
	}
	if restrictions.DenyWorkspaceRead {
		contract.Operations = removeOperations(contract.Operations, OpWorkspaceRead, OpVCSRead)
		contract.Reads = nil
	}
	if restrictions.DenyWorkspaceMutate {
		contract.Operations = removeOperations(contract.Operations, OpWorkspaceMutate, OpVCSCommit, OpPlannerArtifactApply)
		contract.Writes = nil
	}
	if restrictions.DenyLocalProcess {
		contract.Operations = removeOperations(contract.Operations, OpLocalProcess)
	}
}

func removeOperations(operations []OperationClass, removed ...OperationClass) []OperationClass {
	remove := make(map[OperationClass]bool, len(removed))
	for _, operation := range removed {
		remove[operation] = true
	}
	result := operations[:0]
	for _, operation := range operations {
		if !remove[operation] {
			result = append(result, operation)
		}
	}
	return result
}

func containsOperation(operations []OperationClass, want OperationClass) bool {
	for _, operation := range operations {
		if operation == want {
			return true
		}
	}
	return false
}

func hasAgentOutputs(outputs []RequiredOutput) bool {
	for _, output := range outputs {
		if output.Owner == OwnerAgent {
			return true
		}
	}
	return false
}

func canonicalizeContract(contract *Contract) {
	sort.Strings(contract.Reads)
	sort.Slice(contract.Writes, func(i, j int) bool { return contract.Writes[i].Path < contract.Writes[j].Path })
	for index := range contract.Writes {
		sort.Slice(contract.Writes[index].Mutations, func(i, j int) bool {
			return contract.Writes[index].Mutations[i] < contract.Writes[index].Mutations[j]
		})
	}
	sort.Slice(contract.Operations, func(i, j int) bool { return contract.Operations[i] < contract.Operations[j] })
	contract.Operations = uniqueOperations(contract.Operations)
}

func uniqueOperations(values []OperationClass) []OperationClass {
	if len(values) == 0 {
		return nil
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func uniqueSorted(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func safeDiagnostic(value string) string {
	return strings.TrimSpace(value)
}
