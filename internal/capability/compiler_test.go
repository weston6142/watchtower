package capability

import (
	"errors"
	"reflect"
	"testing"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/pkgs"
)

func TestCompileProfileMatrixProducesDeterministicAuthority(t *testing.T) {
	profiles := []flow.CapabilityProfile{
		flow.ProfileArtifact, flow.ProfileInspect, flow.ProfileImplementation,
		flow.ProfileReview, flow.ProfileLibrarian, flow.ProfileFinalReview,
		flow.ProfileConflictResolution,
	}
	for _, profile := range profiles {
		t.Run(string(profile), func(t *testing.T) {
			input := compileInput(profile)
			first, err := Compile(input)
			if err != nil {
				t.Fatal(err)
			}
			input.MaterializedInputs = reverse(input.MaterializedInputs)
			input.ReadableRepositoryPaths = reverse(input.ReadableRepositoryPaths)
			input.ApprovedTouchset = reverse(input.ApprovedTouchset)
			input.Outputs = reverseOutputs(input.Outputs)
			second, err := Compile(input)
			if err != nil {
				t.Fatal(err)
			}
			if first.ContractID != second.ContractID || first.AuthorityDigest != second.AuthorityDigest ||
				!reflect.DeepEqual(first.Contract, second.Contract) {
				t.Fatalf("equivalent input order changed contract:\n%+v\n%+v", first, second)
			}
			if first.Contract.Version != ContractVersion || first.Contract.EnginePolicyVersion != EnginePolicyVersion {
				t.Fatalf("versions = %+v", first.Contract)
			}
		})
	}
}

func TestCompileReadonlyOverridesMutationProfiles(t *testing.T) {
	input := compileInput(flow.ProfileImplementation)
	input.Readonly = true
	compiled, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Contract.Writes) != 0 || containsOperation(compiled.Contract.Operations, OpWorkspaceMutate) ||
		containsOperation(compiled.Contract.Operations, OpVCSCommit) {
		t.Fatalf("readonly contract retained mutation authority: %+v", compiled.Contract)
	}

	input.Outputs = []RequiredOutput{{Path: "result.md", Owner: OwnerAgent}}
	assertCompileReason(t, input, ReasonContractInvalid)
}

func TestCompileInspectProfileRejectsAgentOwnedOutputs(t *testing.T) {
	input := compileInput(flow.ProfileInspect)
	input.Outputs = []RequiredOutput{{Path: "report.md", Owner: OwnerAgent}}
	assertCompileReason(t, input, ReasonContractInvalid)
}

func TestCompileArtifactAndLibrarianScopes(t *testing.T) {
	artifact := compileInput(flow.ProfileArtifact)
	artifact.MaterializedInputs = []string{"ISSUE.md", "STAGE.md", "decisions.md", "spec.md"}
	artifact.Outputs = []RequiredOutput{{Path: "plan.md", Owner: OwnerAgent}, {Path: "verification.json", Owner: OwnerEngine}}
	compiled, err := Compile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(compiled.Contract.Reads, []string{"ISSUE.md", "STAGE.md", "decisions.md", "plan.md", "spec.md", "verification.json"}) {
		t.Fatalf("artifact reads = %v", compiled.Contract.Reads)
	}
	if len(compiled.Contract.Writes) != 1 || compiled.Contract.Writes[0].Path != "plan.md" {
		t.Fatalf("artifact writes = %+v", compiled.Contract.Writes)
	}

	librarian := compileInput(flow.ProfileLibrarian)
	librarian.DocumentationPaths = []string{"docs/**"}
	librarian.ApprovedTouchset = []string{"docs/guildhall/**", "internal/**"}
	compiled, err = Compile(librarian)
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Contract.Writes) != 1 || compiled.Contract.Writes[0].Path != "docs/guildhall/**" {
		t.Fatalf("librarian intersection = %+v", compiled.Contract.Writes)
	}
}

func TestCompileLibrarianIntersectionCannotBroadenApprovedTouchset(t *testing.T) {
	input := compileInput(flow.ProfileLibrarian)
	input.ApprovedTouchset = []string{"docs/*.md"}
	input.DocumentationPaths = []string{"docs/private/**"}
	assertCompileReason(t, input, ReasonContractInvalid)
}

func TestCompileRejectsUnapprovedOrEngineOnlyAuthority(t *testing.T) {
	input := compileInput(flow.ProfileImplementation)
	input.Approval.Approved = false
	assertCompileReason(t, input, ReasonContractInvalid)

	input = compileInput(flow.ProfileImplementation)
	input.RequestedOperations = []string{"push"}
	assertCompileReason(t, input, ReasonContractInvalid)

	input = compileInput(flow.ProfileArtifact)
	input.Outputs = []RequiredOutput{{Path: "verification.json", Owner: OwnerAgent}}
	assertCompileReason(t, input, ReasonContractInvalid)

	input = compileInput(flow.ProfileImplementation)
	input.LegacyRestrictions = pkgs.LegacyRestrictions{Declared: true, DenyWorkspaceMutate: true}
	input.Outputs = []RequiredOutput{{Path: "result.md", Owner: OwnerAgent}}
	assertCompileReason(t, input, ReasonContractInvalid)
}

func TestCompileRetryKeepsAuthorityDigest(t *testing.T) {
	input := compileInput(flow.ProfileReview)
	first, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	input.AttemptID = "attempt-2"
	second, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContractID == second.ContractID {
		t.Fatal("retry reused contract identity")
	}
	if first.AuthorityDigest != second.AuthorityDigest {
		t.Fatalf("retry authority digest changed: %s != %s", first.AuthorityDigest, second.AuthorityDigest)
	}
}

func compileInput(profile flow.CapabilityProfile) CompileInput {
	return CompileInput{
		IssueID: "GH-68", Stage: "execute", AttemptID: "attempt-1",
		Profile: profile, WorkspaceRoot: "/tmp/worktree",
		Repository:              RepositoryIdentity{Branch: "issue/GH-68", BaseCommit: "base", StartCommit: "head", Tree: "tree"},
		MaterializedInputs:      []string{"STAGE.md", "ISSUE.md", "decisions.md"},
		ReadableRepositoryPaths: []string{"internal/z.go", "internal/a.go", "README.md"},
		Outputs:                 []RequiredOutput{{Path: "result.md", Owner: OwnerEngine}},
		Approval:                ApprovalBinding{Approved: true, TouchsetDigest: "touchset-sha", DecisionID: "decision-1", ArtifactID: "artifact-1"},
		ApprovedTouchset:        []string{"internal/z.go", "internal/**"},
		DocumentationPaths:      []string{"internal/**"},
		ConflictPaths:           []string{"internal/z.go"},
	}
}

func reverse[T any](in []T) []T {
	out := append([]T(nil), in...)
	for left, right := 0, len(out)-1; left < right; left, right = left+1, right-1 {
		out[left], out[right] = out[right], out[left]
	}
	return out
}

func reverseOutputs(in []RequiredOutput) []RequiredOutput { return reverse(in) }

func assertCompileReason(t *testing.T, input CompileInput, want FailureReason) {
	t.Helper()
	_, err := Compile(input)
	var policyErr *PolicyError
	if !errors.As(err, &policyErr) || policyErr.Reason != want {
		t.Fatalf("error = %v, want policy reason %q", err, want)
	}
}
