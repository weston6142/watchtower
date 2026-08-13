package runtime_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	capruntime "github.com/weston6142/watchtower/internal/capability/runtime"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/runner"
)

type permissiveProcessBackend struct{}

func (permissiveProcessBackend) Preflight(contract capability.CompiledContract) (capability.EnforcementPlan, error) {
	controls := runner.RequiredControls(contract)
	proofs := make([]capability.ControlProof, 0, len(controls))
	for _, control := range controls {
		proofs = append(proofs, capability.ControlProof{Control: control, Proven: true})
	}
	return runner.NewEnforcementPlan(contract, "test", "permissive", "1", proofs)
}

func (permissiveProcessBackend) Wrap(request capruntime.ProcessRequest) (capruntime.ProcessRequest, error) {
	return request, nil
}

func TestSessionChildEnvironmentIsScrubbedAndScratchBound(t *testing.T) {
	workdir := t.TempDir()
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "inspect", AttemptID: "checkpoint-1", Profile: flow.ProfileInspect,
		WorkspaceRoot: workdir,
	})
	backend := capruntime.NewPlatformBackend()
	plan, err := backend.Preflight(contract)
	if err != nil {
		t.Fatal(err)
	}
	session, err := capruntime.Start(context.Background(), capruntime.StartRequest{
		Contract: contract, Plan: plan, Worktree: workdir, Backend: backend,
		Environment: []string{"TOKEN=private", "SSH_AUTH_SOCK=/private/socket", "SAFE=value", "TMPDIR=/untrusted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	body, err := session.Run(context.Background(), []string{"/usr/bin/env"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "TOKEN=") || strings.Contains(text, "SSH_AUTH_SOCK") || strings.Contains(text, "/untrusted") ||
		!strings.Contains(text, "SAFE=value") || !strings.Contains(text, "TMPDIR="+session.ScratchRoot()) {
		t.Fatalf("child environment=%q", text)
	}
}

func TestSessionRejectsShellExecutableAliases(t *testing.T) {
	workdir := t.TempDir()
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "inspect", AttemptID: "checkpoint-alias", Profile: flow.ProfileInspect,
		WorkspaceRoot: workdir,
	})
	backend := permissiveProcessBackend{}
	plan, err := backend.Preflight(contract)
	if err != nil {
		t.Fatal(err)
	}
	session, err := capruntime.Start(context.Background(), capruntime.StartRequest{
		Contract: contract, Plan: plan, Worktree: workdir, Backend: backend,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	alias := filepath.Join(t.TempDir(), "formatter")
	if err := os.Symlink("/bin/sh", alias); err != nil {
		t.Fatal(err)
	}
	if body, err := session.Run(context.Background(), []string{alias, "-c", "printf bypassed"}); err == nil || len(body) != 0 {
		t.Fatalf("shell alias body=%q err=%v", body, err)
	}
}
