package plannerartifact

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInitializeCreatesFreshArtifacts(t *testing.T) {
	dir := t.TempDir()
	session, err := Initialize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	planInfo, err := os.Stat(filepath.Join(dir, "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !planInfo.Mode().IsRegular() || planInfo.Mode().Perm() != 0o644 {
		t.Fatalf("plan.md mode/type = %s, want regular 0644", planInfo.Mode())
	}
	touchsetInfo, err := os.Stat(filepath.Join(dir, "touchset.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !touchsetInfo.Mode().IsRegular() || touchsetInfo.Mode().Perm() != 0o644 {
		t.Fatalf("touchset.json mode/type = %s, want regular 0644", touchsetInfo.Mode())
	}

	if got, err := os.ReadFile(filepath.Join(dir, "plan.md")); err != nil {
		t.Fatal(err)
	} else if string(got) != "# Implementation Plan\n\n" {
		t.Fatalf("plan.md = %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "touchset.json")); err != nil {
		t.Fatal(err)
	} else if string(got) != `{"globs":[]}` {
		t.Fatalf("touchset.json = %q", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := []string{"plan.md", "touchset.json"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("worktree entries = %v, want %v", names, want)
	}
}

func TestInitializeAdoptsValidSectionedArtifactsWithoutRewrite(t *testing.T) {
	dir := t.TempDir()
	manifest := completeManifest()
	plan := sectionedPlan(manifest)
	touchset := manifestTouchset(manifest)
	if err := os.WriteFile(filepath.Join(dir, "plan.md"), []byte(plan), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "touchset.json"), touchset, 0o600); err != nil {
		t.Fatal(err)
	}

	beforePlan, err := os.ReadFile(filepath.Join(dir, "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	beforeTouchset, err := os.ReadFile(filepath.Join(dir, "touchset.json"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := Initialize(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	afterPlan, err := os.ReadFile(filepath.Join(dir, "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	afterTouchset, err := os.ReadFile(filepath.Join(dir, "touchset.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterPlan, beforePlan) || !reflect.DeepEqual(afterTouchset, beforeTouchset) {
		t.Fatalf("adoption rewrote artifacts: plan changed=%v touchset changed=%v",
			!reflect.DeepEqual(afterPlan, beforePlan), !reflect.DeepEqual(afterTouchset, beforeTouchset))
	}
	if got := mustMode(t, filepath.Join(dir, "plan.md")); got != 0o640 {
		t.Fatalf("adopted plan mode = %#o, want 0640", got)
	}
	if got := mustMode(t, filepath.Join(dir, "touchset.json")); got != 0o600 {
		t.Fatalf("adopted touchset mode = %#o, want 0600", got)
	}
}

func TestInitializeRejectsMalformedStartingArtifactsWithoutOverwrite(t *testing.T) {
	manifest := completeManifest()
	cases := []struct {
		name     string
		makePlan func(t *testing.T, path string)
		makeSet  func(t *testing.T, path string)
		artifact string
	}{
		{
			name: "legacy plan",
			makePlan: func(t *testing.T, path string) {
				writeFile(t, path, "# Legacy plan\n", 0o644)
			},
			makeSet:  func(t *testing.T, path string) { writeFile(t, path, `{"globs":[]}`, 0o644) },
			artifact: "plan.md",
		},
		{
			name:     "malformed touchset",
			makePlan: func(t *testing.T, path string) { writeFile(t, path, "# Implementation Plan\n\n", 0o644) },
			makeSet:  func(t *testing.T, path string) { writeFile(t, path, `{}`, 0o644) },
			artifact: "touchset.json",
		},
		{
			name: "duplicate anchor",
			makePlan: func(t *testing.T, path string) {
				writeFile(t, path, sectionedPlanWithKeys([]string{"goal", "goal"}), 0o644)
			},
			makeSet:  func(t *testing.T, path string) { writeFile(t, path, `{"globs":[]}`, 0o644) },
			artifact: "plan.md",
		},
		{
			name: "unterminated anchor",
			makePlan: func(t *testing.T, path string) {
				writeFile(t, path, "# Implementation Plan\n\n<!-- watchtower-section: key=goal -->\ncontent\n", 0o644)
			},
			makeSet:  func(t *testing.T, path string) { writeFile(t, path, `{"globs":[]}`, 0o644) },
			artifact: "plan.md",
		},
		{
			name: "out of order anchors",
			makePlan: func(t *testing.T, path string) {
				writeFile(t, path, sectionedPlanWithKeys([]string{"architecture", "goal"}), 0o644)
			},
			makeSet:  func(t *testing.T, path string) { writeFile(t, path, `{"globs":[]}`, 0o644) },
			artifact: "plan.md",
		},
		{
			name: "symlink target",
			makePlan: func(t *testing.T, path string) {
				target := filepath.Join(filepath.Dir(path), "plan-target.md")
				writeFile(t, target, "# Implementation Plan\n\n", 0o644)
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
			makeSet:  func(t *testing.T, path string) { writeFile(t, path, `{"globs":[]}`, 0o644) },
			artifact: "plan.md",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			planPath := filepath.Join(dir, "plan.md")
			touchsetPath := filepath.Join(dir, "touchset.json")
			tc.makePlan(t, planPath)
			tc.makeSet(t, touchsetPath)
			beforePlan, err := os.ReadFile(planPath)
			if err != nil && tc.name != "symlink target" {
				t.Fatal(err)
			}
			beforeTouchset, err := os.ReadFile(touchsetPath)
			if err != nil {
				t.Fatal(err)
			}

			_, err = Initialize(dir)
			if err == nil {
				t.Fatal("Initialize succeeded for malformed starting artifacts")
			}
			if !strings.Contains(err.Error(), "malformed-starting-artifact") ||
				!strings.Contains(err.Error(), tc.artifact) {
				t.Fatalf("error = %v, want malformed scope and %s", err, tc.artifact)
			}
			afterTouchset, readErr := os.ReadFile(touchsetPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !reflect.DeepEqual(afterTouchset, beforeTouchset) {
				t.Fatal("touchset changed after rejected initialization")
			}
			if tc.name != "symlink target" {
				afterPlan, readErr := os.ReadFile(planPath)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !reflect.DeepEqual(afterPlan, beforePlan) {
					t.Fatal("plan changed after rejected initialization")
				}
			} else if info, statErr := os.Lstat(planPath); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("symlink target was replaced: info=%v err=%v", info, statErr)
			}
		})
	}
	_ = manifest
}

func TestValidateManifestRules(t *testing.T) {
	if err := ValidateManifest(completeManifest()); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Manifest)
	}{
		{name: "missing required key", edit: func(m *Manifest) { m.Sections = m.Sections[:len(m.Sections)-1] }},
		{name: "duplicate key", edit: func(m *Manifest) { m.Sections[1].Key = m.Sections[0].Key }},
		{name: "unsafe glob", edit: func(m *Manifest) { m.Sections[0].Globs = []string{"../escape/**"} }},
		{name: "invalid key", edit: func(m *Manifest) { m.Sections[0].Key = "Goal" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := completeManifest()
			tc.edit(&manifest)
			if err := ValidateManifest(manifest); err == nil {
				t.Fatal("ValidateManifest accepted invalid manifest")
			}
		})
	}
}

func TestValidateCompleteValidationScopes(t *testing.T) {
	manifest := completeManifest()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "plan.md"), sectionedPlan(manifest), 0o644)
	writeFile(t, filepath.Join(dir, "touchset.json"), string(manifestTouchset(manifest)), 0o644)
	session, err := Initialize(dir)
	if err != nil {
		t.Fatal(err)
	}
	session.manifest = &manifest
	if err := session.ValidateComplete(); err != nil {
		t.Fatalf("valid pair rejected: %v", err)
	}

	cases := []struct {
		name     string
		artifact string
		content  string
		want     string
	}{
		{name: "invalid plan", artifact: "plan.md", content: "# broken\n", want: "plan.md"},
		{name: "invalid touchset", artifact: "touchset.json", content: `{}`, want: "touchset.json"},
		{name: "invalid pair union", artifact: "touchset.json", content: `{"globs":["internal/gh40/goal/**"]}`, want: "pair"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, filepath.Join(dir, tc.artifact), tc.content, 0o644)
			err := session.ValidateComplete()
			if err == nil {
				t.Fatal("ValidateComplete accepted invalid artifacts")
			}
			var diagnostic *DiagnosticError
			if !errors.As(err, &diagnostic) {
				t.Fatalf("error = %T %v, want DiagnosticError", err, err)
			}
			if diagnostic.Scope != ScopeFinalValidation || diagnostic.Artifact != tc.want {
				t.Fatalf("diagnostic = %+v, want final-validation artifact %s", diagnostic, tc.want)
			}
			writeFile(t, filepath.Join(dir, "plan.md"), sectionedPlan(manifest), 0o644)
			writeFile(t, filepath.Join(dir, "touchset.json"), string(manifestTouchset(manifest)), 0o644)
		})
	}
}

func completeManifest() Manifest {
	return Manifest{Sections: []ManifestEntry{
		{Key: "goal", Globs: []string{"internal/gh40/goal/**"}},
		{Key: "architecture", Globs: []string{"internal/gh40/architecture/**"}},
		{Key: "technology-stack", Globs: []string{"internal/gh40/technology/**"}},
		{Key: "execution-contract", Globs: []string{"internal/gh40/contract/**"}},
		{Key: "file-structure", Globs: []string{"internal/gh40/files/**"}},
		{Key: "task-0001", Globs: []string{"internal/gh40/task-0001/**"}},
		{Key: "verification", Globs: []string{"internal/gh40/verification/**"}},
	}}
}

func sectionedPlan(manifest Manifest) string {
	var builder strings.Builder
	builder.WriteString("# Implementation Plan\n\n")
	for _, entry := range manifest.Sections {
		builder.WriteString("<!-- watchtower-section: key=")
		builder.WriteString(entry.Key)
		builder.WriteString(" -->\nsection ")
		builder.WriteString(entry.Key)
		builder.WriteString("\n<!-- watchtower-section-end: key=")
		builder.WriteString(entry.Key)
		builder.WriteString(" -->\n")
	}
	return builder.String()
}

func sectionedPlanWithKeys(keys []string) string {
	var builder strings.Builder
	builder.WriteString("# Implementation Plan\n\n")
	for _, key := range keys {
		builder.WriteString("<!-- watchtower-section: key=")
		builder.WriteString(key)
		builder.WriteString(" -->\ncontent\n<!-- watchtower-section-end: key=")
		builder.WriteString(key)
		builder.WriteString(" -->\n")
	}
	return builder.String()
}

func manifestTouchset(manifest Manifest) []byte {
	var globs []string
	for _, entry := range manifest.Sections {
		globs = append(globs, entry.Globs...)
	}
	b, err := json.Marshal(struct {
		Globs []string `json:"globs"`
	}{Globs: globs})
	if err != nil {
		panic(err)
	}
	return b
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
