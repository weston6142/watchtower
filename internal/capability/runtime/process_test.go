package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	capruntime "github.com/weston6142/watchtower/internal/capability/runtime"
	"github.com/weston6142/watchtower/internal/flow"
)

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
