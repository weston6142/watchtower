package pkgs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDir(t *testing.T) {
	m, err := LoadDir(filepath.Join("testdata", "packages"))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := m["executor"]
	if !ok {
		t.Fatalf("executor missing: %v", m)
	}
	if len(p.AllowedTools) != 4 || p.AllowedTools[0] != "Bash" {
		t.Fatalf("tools: %v", p.AllowedTools)
	}
	if p.Prompt == "" || p.Name != "executor" {
		t.Fatalf("bad package: %+v", p)
	}
	if p.Identity.Name != "Executor" || p.Identity.Color != "green" || p.Identity.Symbol != "⚙" {
		t.Fatalf("identity: %+v", p.Identity)
	}
	if !strings.HasPrefix(p.Prompt, "# Shared include: decision-protocol\n") ||
		!strings.Contains(p.Prompt, "# Package prompt: executor\n") {
		t.Fatalf("composed prompt: %q", p.Prompt)
	}
}

func TestLoadDirRejectsUnsafeIncludes(t *testing.T) {
	cases := []struct {
		name    string
		include string
		setup   func(root string)
	}{
		{name: "parent traversal", include: "../secret"},
		{name: "absolute", include: "/tmp/secret"},
		{name: "missing", include: "missing"},
		{name: "duplicate", include: "safe\n  - safe", setup: func(root string) {
			writeTestFile(t, filepath.Join(root, "shared", "safe.md"), "safe")
		}},
		{name: "escaping symlink", include: "escape", setup: func(root string) {
			outside := filepath.Join(root, "outside.md")
			writeTestFile(t, outside, "secret")
			if err := os.MkdirAll(filepath.Join(root, "shared"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "shared", "escape.md")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.setup != nil {
				tc.setup(root)
			}
			writeTestFile(t, filepath.Join(root, "packages", "agent", "prompt.md"), "agent")
			writeTestFile(t, filepath.Join(root, "packages", "agent", "package.yaml"),
				"identity:\n  name: Test Agent\n  color: gray\n  symbol: ▣\nincludes:\n  - "+tc.include+"\n")
			if _, err := LoadDir(filepath.Join(root, "packages")); err == nil {
				t.Fatal("unsafe include was accepted")
			}
		})
	}
}

func TestLegacyAllowedToolsCanOnlyRestrict(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "packages", "agent", "prompt.md"), "agent")
	base := "identity:\n  name: Test Agent\n  color: gray\n  symbol: X\n"

	writeTestFile(t, filepath.Join(root, "packages", "agent", "package.yaml"), base+
		"allowed_tools: [Read, Glob, Grep]\n")
	loaded, err := LoadDir(filepath.Join(root, "packages"))
	if err != nil {
		t.Fatal(err)
	}
	restrictions := loaded["agent"].LegacyRestrictions
	if !restrictions.Declared || restrictions.DenyWorkspaceRead || !restrictions.DenyWorkspaceMutate || !restrictions.DenyLocalProcess {
		t.Fatalf("read-only legacy restrictions = %+v", restrictions)
	}

	writeTestFile(t, filepath.Join(root, "packages", "agent", "package.yaml"), base+
		"allowed_tools: [Read, Publish]\n")
	if _, err := LoadDir(filepath.Join(root, "packages")); err == nil || !strings.Contains(err.Error(), "Publish") {
		t.Fatalf("unknown legacy tool error = %v", err)
	}

	writeTestFile(t, filepath.Join(root, "packages", "agent", "package.yaml"), base)
	loaded, err = LoadDir(filepath.Join(root, "packages"))
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["agent"].LegacyRestrictions; got.Declared || got.DenyWorkspaceRead || got.DenyWorkspaceMutate || got.DenyLocalProcess {
		t.Fatalf("absent legacy declaration restricted authority: %+v", got)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
