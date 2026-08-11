package runtime_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/flow"
)

func TestGatewayRejectsHardLinksAndEscapingSymlinks(t *testing.T) {
	workdir := t.TempDir()
	contract := compileRuntimeContract(t, capability.CompileInput{
		IssueID: "GH-68", Stage: "spec", AttemptID: "checkpoint-1", Profile: flow.ProfileArtifact,
		WorkspaceRoot: workdir,
		Outputs:       []capability.RequiredOutput{{Path: "target.txt", Owner: capability.OwnerAgent}, {Path: "link.txt", Owner: capability.OwnerAgent}},
	})
	session := startRuntimeSession(t, workdir, contract)
	defer session.Close()
	if err := session.WriteFile("target.txt", []byte("target\n"), capability.MutationCreate); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(workdir, "target.txt"), filepath.Join(workdir, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ReadFile("target.txt"); err == nil {
		t.Fatal("hard-linked file was disclosed")
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(workdir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ReadFile("link.txt"); err == nil {
		t.Fatal("escaping symlink was followed")
	}
}
