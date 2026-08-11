package conformance

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/capability"
	capruntime "github.com/weston6142/watchtower/internal/capability/runtime"
	"github.com/weston6142/watchtower/internal/runner"
)

const AdapterVersion = "1"

// Preflight proves that the platform containment backend covers the complete
// canonical contract, then binds those proofs to a provider adapter identity.
// It never launches or contacts the provider.
func Preflight(request runner.PreflightRequest, backend capruntime.Backend, provider, implementation string) (capability.EnforcementPlan, error) {
	if request.IssueID == "" || request.Stage == "" || request.Agent == "" || request.Workdir == "" ||
		request.Contract.ContractID == "" || request.Contract.Contract.IssueID != request.IssueID ||
		request.Contract.Contract.Stage != request.Stage {
		return capability.EnforcementPlan{}, policyError(provider, "preflight request identity mismatch")
	}
	if backend == nil {
		return capability.EnforcementPlan{}, policyError(provider, "containment backend is unavailable")
	}
	backendPlan, err := backend.Preflight(request.Contract)
	if err != nil {
		return capability.EnforcementPlan{}, err
	}
	if err := runner.ValidateEnforcementPlan(request.Contract, backendPlan); err != nil {
		return capability.EnforcementPlan{}, err
	}
	return runner.NewEnforcementPlan(request.Contract, provider, implementation, AdapterVersion, backendPlan.Controls)
}

// GatewayTools returns only provider-neutral mediated operations represented
// by the immutable contract. Package tool declarations never enter this map.
func GatewayTools(contract capability.CompiledContract) []string {
	tools := make([]string, 0, len(contract.Contract.Operations))
	for _, operation := range contract.Contract.Operations {
		tools = append(tools, "mcp__watchtower__"+strings.ReplaceAll(string(operation), "-", "_"))
	}
	sort.Strings(tools)
	return tools
}

func IsGatewayTool(name string) bool {
	return strings.HasPrefix(name, "mcp__watchtower__")
}

func RuntimeDenial(request runner.StageRequest, provider string, call runner.ToolCall) (*capability.PolicyError, capability.AuditRecord) {
	err := &capability.PolicyError{
		Phase: "runtime", Reason: capability.ReasonRuntimeDenied, Provider: provider,
		Diagnostic: "unmediated provider tool request",
	}
	record := capability.AuditRecord{
		Attempt: capability.AttemptIdentity{
			IssueID: request.IssueID, Stage: request.Stage, AttemptID: request.Contract.Contract.AttemptID,
		},
		ContractID: request.Contract.ContractID, Phase: "runtime", Outcome: "denied",
		Reason: capability.ReasonRuntimeDenied, Provider: provider,
	}
	_ = call // Raw provider payloads intentionally do not enter audit evidence.
	return err, record
}

func ValidateRequest(request runner.StageRequest, provider string) error {
	if err := runner.ValidateStageRequest(request); err != nil {
		return err
	}
	if request.Plan.Provider != provider {
		return policyError(provider, fmt.Sprintf("enforcement plan belongs to %s", request.Plan.Provider))
	}
	return nil
}

// Verify runs the provider-neutral black-box preflight contract used by every
// real adapter. It deliberately observes only public plans and stable errors.
func Verify(ctx context.Context, adapter runner.Runner, requests []runner.PreflightRequest) error {
	if adapter == nil {
		return fmt.Errorf("adapter is unavailable")
	}
	for _, request := range requests {
		plan, err := adapter.Preflight(ctx, request)
		if err != nil {
			return fmt.Errorf("profile %s: %w", request.Contract.Contract.Profile, err)
		}
		if err := runner.ValidateEnforcementPlan(request.Contract, plan); err != nil {
			return fmt.Errorf("profile %s: %w", request.Contract.Contract.Profile, err)
		}
	}
	return nil
}

func StartSession(ctx context.Context, request runner.StageRequest, backend capruntime.Backend, audit func(capability.AuditRecord)) (*capruntime.Session, error) {
	return capruntime.Start(ctx, capruntime.StartRequest{
		Contract: request.Contract, Plan: request.Plan, Worktree: request.Workdir,
		Backend: backend, Audit: audit,
	})
}

func policyError(provider, diagnostic string) error {
	return &capability.PolicyError{
		Phase: "preflight", Reason: capability.ReasonProviderUnsupported,
		Provider: provider, Diagnostic: diagnostic,
	}
}
