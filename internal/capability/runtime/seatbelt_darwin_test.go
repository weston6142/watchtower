//go:build darwin

package runtime

import (
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
