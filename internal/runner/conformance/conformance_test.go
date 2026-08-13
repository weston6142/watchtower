package conformance

import (
	"errors"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	capruntime "github.com/weston6142/watchtower/internal/capability/runtime"
	"github.com/weston6142/watchtower/internal/runner"
)

type incompleteBackend struct{}

func (incompleteBackend) Preflight(contract capability.CompiledContract) (capability.EnforcementPlan, error) {
	return capability.EnforcementPlan{
		ContractID: contract.ContractID, Provider: "incomplete", Implementation: "test", Version: "1", PlanID: "invalid",
		Controls: []capability.ControlProof{{Control: capability.ControlNetwork, Proven: true}},
	}, nil
}

func (incompleteBackend) Wrap(capruntime.ProcessRequest) (capruntime.ProcessRequest, error) {
	return capruntime.ProcessRequest{}, errors.New("not used")
}

func TestProviderPreflightRejectsIncompleteControlsBeforeLaunch(t *testing.T) {
	request := testPreflightRequest(t.TempDir(), "implementation", []capability.OperationClass{
		capability.OpWorkspaceRead, capability.OpWorkspaceMutate, capability.OpLocalProcess,
	})
	_, err := Preflight(request, incompleteBackend{}, "test-provider", "adapter")
	var policy *capability.PolicyError
	if !errors.As(err, &policy) || policy.Reason != capability.ReasonProviderUnsupported {
		t.Fatalf("preflight error = %v", err)
	}
}

func TestGatewayToolsComeOnlyFromContract(t *testing.T) {
	contract := testPreflightRequest(t.TempDir(), "implementation", []capability.OperationClass{
		capability.OpVCSCommit, capability.OpWorkspaceRead,
	}).Contract
	got := GatewayTools(contract)
	want := []string{"mcp__watchtower__vcs_commit", "mcp__watchtower__workspace_read"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("gateway tools = %v, want %v", got, want)
	}
	if !IsGatewayTool(contract, "mcp__watchtower__workspace_read") {
		t.Fatal("contract tool was rejected")
	}
	for _, name := range []string{"mcp__watchtower__workspace_destroy", "mcp__watchtower__workspace_read_extra", "mcp__other__workspace_read"} {
		if IsGatewayTool(contract, name) {
			t.Fatalf("unregistered gateway tool %q was accepted", name)
		}
	}
}

func testPreflightRequest(root, profile string, operations []capability.OperationClass) runner.PreflightRequest {
	contract := capability.CompiledContract{
		ContractID: "contract-" + profile, AuthorityDigest: "authority",
		Contract: capability.Contract{
			Version: capability.ContractVersion, EnginePolicyVersion: capability.EnginePolicyVersion,
			IssueID: "GH-test", Stage: "execute", AttemptID: "attempt", Profile: profile,
			WorkspaceRoot: root, Operations: operations,
		},
	}
	return runner.PreflightRequest{IssueID: "GH-test", Stage: "execute", Agent: "executor", Workdir: root, Contract: contract}
}
