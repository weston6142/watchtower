package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/capability"
	capruntime "github.com/weston6142/watchtower/internal/capability/runtime"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/runner/conformance"
)

func TestClaudeCapabilityConformance(t *testing.T) {
	backend := capruntime.NewPlatformBackend()
	adapter := &CodeRunner{Backend: backend, Packages: map[string]pkgs.Package{"executor": {Name: "executor"}}}
	if err := conformance.Verify(context.Background(), adapter, claudeCapabilityRequests(t.TempDir())); err != nil {
		t.Fatal(err)
	}
}

func TestProviderUsesGatewayInsteadOfLegacyTools(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(t.TempDir(), "claude-argv-stub")
	script := `#!/bin/sh
set -eu
joined="$*"
case "$joined" in
  *--mcp-config*watchtower*--strict-mcp-config*--bare*--tools*mcp__watchtower__workspace_read*) ;;
  *) exit 41 ;;
esac
case "$joined" in *legacy-escalation*) exit 42 ;; esac
read _task
printf '%s\n' '{"type":"system","subtype":"init","session_id":"gateway"}'
printf '%s\n' '{"type":"result","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter := &CodeRunner{
		Bin: bin, Backend: capruntime.NewPlatformBackend(),
		Packages: map[string]pkgs.Package{"executor": {Name: "executor", AllowedTools: []string{"Bash", "Read", "legacy-escalation"}}},
	}
	request := claudeCapabilityRequests(root)[0]
	plan, err := adapter.Preflight(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	result := <-adapter.Run(context.Background(), runner.StageRequest{
		IssueID: request.IssueID, Stage: request.Stage, Agent: request.Agent, Workdir: root,
		Contract: request.Contract, Plan: plan,
	}, make(chan runner.Ask))
	if result.Err != nil {
		t.Fatal(result.Err)
	}
}

func TestProviderDenialTerminatesCompleteProcessTree(t *testing.T) {
	root := t.TempDir()
	sideEffect := filepath.Join(root, "forbidden-side-effect")
	bin := filepath.Join(t.TempDir(), "claude-denial-stub")
	script := `#!/bin/sh
set -eu
read _task
(sleep 1; touch "$SIDE_EFFECT") &
printf '%s\n' '{"type":"system","subtype":"init","session_id":"denial"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"touch forbidden-side-effect"}}]}}'
sleep 10
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter := &CodeRunner{
		Bin: bin, Backend: capruntime.NewPlatformBackend(), ExtraEnv: []string{"SIDE_EFFECT=" + sideEffect},
		Packages: map[string]pkgs.Package{"executor": {Name: "executor"}},
	}
	request := claudeCapabilityRequests(root)[0]
	plan, err := adapter.Preflight(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	result := <-adapter.Run(context.Background(), runner.StageRequest{
		IssueID: request.IssueID, Stage: request.Stage, Agent: request.Agent, Workdir: root,
		Contract: request.Contract, Plan: plan,
	}, make(chan runner.Ask))
	var policy *capability.PolicyError
	if !errors.As(result.Err, &policy) || policy.Reason != capability.ReasonRuntimeDenied {
		t.Fatalf("result = %+v", result)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(sideEffect); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provider descendant survived denial: %v", err)
	}
}

func claudeCapabilityRequests(root string) []runner.PreflightRequest {
	profiles := []string{"artifact", "inspect", "implementation", "review", "librarian", "final-review", "conflict-resolution"}
	requests := make([]runner.PreflightRequest, 0, len(profiles))
	for _, profile := range profiles {
		contract := capability.CompiledContract{
			ContractID: "contract-" + profile, AuthorityDigest: "authority-" + profile,
			Contract: capability.Contract{
				Version: capability.ContractVersion, EnginePolicyVersion: capability.EnginePolicyVersion,
				IssueID: "GH-68", Stage: "execute", AttemptID: "attempt", Profile: profile,
				WorkspaceRoot: root, Operations: []capability.OperationClass{capability.OpWorkspaceRead},
			},
		}
		requests = append(requests, runner.PreflightRequest{IssueID: "GH-68", Stage: "execute", Agent: "executor", Workdir: root, Contract: contract})
	}
	return requests
}
