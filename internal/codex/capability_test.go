package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/capability"
	capruntime "github.com/weston6142/watchtower/internal/capability/runtime"
	"github.com/weston6142/watchtower/internal/pkgs"
	"github.com/weston6142/watchtower/internal/repocfg"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/runner/conformance"
)

func TestCodexCapabilityConformance(t *testing.T) {
	backend := capruntime.NewPlatformBackend()
	adapter := &CodeRunner{Backend: backend, Packages: map[string]pkgs.Package{"executor": {Name: "executor"}}}
	if err := conformance.Verify(context.Background(), adapter, capabilityRequests(t.TempDir())); err != nil {
		t.Fatal(err)
	}
}

func TestProviderUsesGatewayInsteadOfLegacyTools(t *testing.T) {
	got := buildInvocation(repocfg.CodexProfile{
		Model: "model", Effort: "high", FeatureOverrides: map[string]bool{"unified_exec": true},
	}, turnDescriptor{
		Workdir: t.TempDir(), Kind: turnInitial, PackagePrompt: "legacy Bash Read Write",
		Prompt: "task", GatewayEndpoint: "http://127.0.0.1/private", GatewayTools: []string{"mcp__watchtower__workspace_read"},
	})
	joined := strings.Join(got.Argv, "\n")
	for _, want := range []string{`sandbox_mode="read-only"`, "tools.web_search=false", "features.shell_tool=false", "mcp_servers.watchtower.url", "enabled_tools", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--strict-config"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Codex invocation missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "danger-full-access") {
		t.Fatalf("Codex retained unrestricted sandbox authority: %s", joined)
	}
	if strings.Contains(joined, "features.unified_exec=true") {
		t.Fatalf("Codex profile re-enabled unmanaged native execution: %s", joined)
	}
	if strings.Contains(strings.Join(got.RedactedArgv, "\n"), "http://127.0.0.1/private") {
		t.Fatalf("Codex recorded the private gateway endpoint: %v", got.RedactedArgv)
	}
}

func TestProviderDenialTerminatesCompleteProcessTree(t *testing.T) {
	root := t.TempDir()
	sideEffect := filepath.Join(root, "forbidden-side-effect")
	bin := filepath.Join(t.TempDir(), "codex-denial-stub")
	script := `#!/bin/sh
set -eu
(sleep 1; touch "$SIDE_EFFECT") &
printf '%s\n' '{"type":"thread.started","thread_id":"denial"}'
printf '%s\n' '{"type":"item.started","item":{"type":"command_execution","command":"touch forbidden-side-effect"}}'
sleep 10
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter := testRunner(bin)
	adapter.Backend = capruntime.NewPlatformBackend()
	adapter.ExtraEnv = []string{"SIDE_EFFECT=" + sideEffect}
	result := <-adapter.Run(context.Background(), testStageRequest(adapter, "GH-68", "execute", "executor", root), make(chan runner.Ask))
	var policy *capability.PolicyError
	if !errors.As(result.Err, &policy) || policy.Reason != capability.ReasonRuntimeDenied {
		t.Fatalf("result = %+v", result)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(sideEffect); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provider descendant survived denial: %v", err)
	}
	found := false
	for _, record := range result.RuntimeAudit {
		found = found || record.Reason == capability.ReasonRuntimeDenied
	}
	if !found {
		t.Fatalf("runtime denial audit missing: %+v", result.RuntimeAudit)
	}
}

func TestProviderTurnsKeepContractIdentity(t *testing.T) {
	root := t.TempDir()
	request := capabilityRequests(root)[0]
	marker := filepath.Join(root, "started")
	adapter := &CodeRunner{
		Bin: "/usr/bin/touch", Backend: capruntime.NewPlatformBackend(),
		Packages: map[string]pkgs.Package{"executor": {Name: "executor"}},
	}
	plan, err := adapter.Preflight(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	plan.ContractID = "different"
	result := <-adapter.Run(context.Background(), runner.StageRequest{
		IssueID: request.IssueID, Stage: request.Stage, Agent: request.Agent, Workdir: root,
		Contract: request.Contract, Plan: plan,
	}, make(chan runner.Ask))
	if result.Err == nil {
		t.Fatal("mismatched provider plan was accepted")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provider launched before identity rejection: %v", err)
	}
}

func capabilityRequests(root string) []runner.PreflightRequest {
	profiles := []string{"artifact", "inspect", "implementation", "review", "librarian", "final-review", "conflict-resolution"}
	requests := make([]runner.PreflightRequest, 0, len(profiles))
	for _, profile := range profiles {
		contract := sealCodexTestContract(capability.Contract{
			Version: capability.ContractVersion, EnginePolicyVersion: capability.EnginePolicyVersion,
			IssueID: "GH-68", Stage: "execute", AttemptID: "attempt", Profile: profile,
			WorkspaceRoot: root, Operations: []capability.OperationClass{capability.OpWorkspaceRead},
		})
		requests = append(requests, runner.PreflightRequest{IssueID: "GH-68", Stage: "execute", Agent: "executor", Workdir: root, Contract: contract})
	}
	return requests
}
