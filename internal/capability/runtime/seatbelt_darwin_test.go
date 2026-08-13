//go:build darwin

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
)

func TestSeatbeltProfileDefaultsToDenyAndOmitsLifecycleAuthority(t *testing.T) {
	request := ProcessRequest{
		Path: "/usr/bin/printf",
		Contract: capability.CompiledContract{Contract: capability.Contract{
			WorkspaceRoot: "/private/tmp/worktree", Reads: []string{"README.md"},
			Writes: []capability.PathGrant{{Path: "src/**", Mutations: []capability.MutationClass{capability.MutationModify}}},
		}},
	}
	profile, err := seatbeltProfile(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, "(deny default)") || !strings.Contains(profile, "(deny network*)") || strings.Contains(profile, ".git") || strings.Contains(profile, "merge") {
		t.Fatalf("unsafe profile:\n%s", profile)
	}
}

func TestSeatbeltProviderProfileKeepsTransportSeparateFromAgentProcesses(t *testing.T) {
	request := ProcessRequest{
		Path: "/usr/bin/printf",
		Mode: ModeProviderTransport,
		Contract: capability.CompiledContract{Contract: capability.Contract{
			WorkspaceRoot: "/private/tmp/worktree", Reads: []string{"STAGE.md"},
		}},
	}
	profile, err := seatbeltProfile(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, "(allow network*)") || strings.Contains(profile, "(deny network*)") {
		t.Fatalf("provider transport is not separated from network-denied agent processes:\n%s", profile)
	}
	for _, forbidden := range []string{"/private/tmp/worktree", "STAGE.md"} {
		if strings.Contains(profile, forbidden) {
			t.Fatalf("provider transport inherited workspace grant %q:\n%s", forbidden, profile)
		}
	}
}

func TestSeatbeltProviderProfileAllowsDeclaredScriptInterpreter(t *testing.T) {
	script := filepath.Join(t.TempDir(), "provider-stub")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile, err := seatbeltProfile(ProcessRequest{
		Path: script, Mode: ModeProviderTransport, ProviderReads: []string{script}, ProviderExecutables: []string{script, "/bin/sh"},
		Contract: capability.CompiledContract{Contract: capability.Contract{WorkspaceRoot: t.TempDir()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, executable := range []string{script, "/bin/sh"} {
		if !strings.Contains(profile, `(allow process-exec* (literal "`+executable+`"))`) {
			t.Fatalf("provider profile omitted executable %s:\n%s", executable, profile)
		}
	}
	if strings.Contains(profile, `(allow process-exec (subpath "/bin"))`) {
		t.Fatalf("provider profile granted arbitrary /bin execution:\n%s", profile)
	}
}
