package plannerartifact

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type authorityTestRegistry struct {
	mu        sync.Mutex
	status    string
	digest    []byte
	manifest  []byte
	sections  []byte
	found     bool
	failRead  bool
	failWrite bool
}

func (r *authorityTestRegistry) LoadPlannerArtifact(string, string, int, string) (string, []byte, []byte, []byte, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failRead {
		r.failRead = false
		return "", nil, nil, nil, false, errors.New("injected registry read failure")
	}
	return r.status, append([]byte(nil), r.digest...), append([]byte(nil), r.manifest...), append([]byte(nil), r.sections...), r.found, nil
}

func (r *authorityTestRegistry) CreatePlannerArtifact(_ string, _ string, _ int, _ string, status string, digest, manifest, sections []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failWrite {
		r.failWrite = false
		return errors.New("injected registry write failure")
	}
	r.status, r.digest, r.manifest, r.sections, r.found = status, append([]byte(nil), digest...), append([]byte(nil), manifest...), append([]byte(nil), sections...), true
	return nil
}

func (r *authorityTestRegistry) UpdatePlannerArtifact(issue, stage string, attempt int, worktree, status string, digest, manifest, sections []byte) error {
	return r.CreatePlannerArtifact(issue, stage, attempt, worktree, status, digest, manifest, sections)
}

func (r *authorityTestRegistry) ExpirePlannerArtifact(string, string, int, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = "expired"
	return nil
}

func TestPlannerArtifactAuthorityBindingAndRetry(t *testing.T) {
	workdir := t.TempDir()
	registry := &authorityTestRegistry{}
	binding := Binding{IssueID: "GH-62", Stage: "plan", Attempt: 1, Worktree: workdir}
	authority, err := CreateOrLoad(registry, binding)
	if err != nil {
		t.Fatal(err)
	}
	manifest := authorityTestManifest()
	first := WriteRequest{Manifest: manifest, Key: "goal", Markdown: "first", Globs: manifest.Sections[0].Globs}
	if err := authority.Apply(first); err != nil {
		t.Fatal(err)
	}
	retry, err := CreateOrLoad(registry, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := retry.Apply(first); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	second := WriteRequest{Manifest: manifest, Key: "architecture", Markdown: "second", Globs: manifest.Sections[1].Globs}
	if err := retry.Apply(second); err != nil {
		t.Fatal(err)
	}
	plan, err := os.ReadFile(filepath.Join(workdir, "plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(plan), "key=goal") != 2 || strings.Count(string(plan), "key=architecture") != 2 {
		t.Fatalf("durable retry duplicated anchors: %s", plan)
	}
	if err := retry.VerifyBinding(Binding{IssueID: "GH-62", Stage: "plan", Attempt: 2, Worktree: workdir}); err == nil {
		t.Fatal("changed attempt authorized the existing record")
	}
}

func TestPlannerArtifactAuthorityPrivateSessionAndRegistryFailure(t *testing.T) {
	workdir := t.TempDir()
	registry := &authorityTestRegistry{}
	authority, err := CreateOrLoad(registry, Binding{IssueID: "GH-62", Stage: "plan", Attempt: 1, Worktree: workdir})
	if err != nil {
		t.Fatal(err)
	}
	request := WriteRequest{Manifest: authorityTestManifest(), Key: "goal", Markdown: "first", Globs: []string{"internal/goal/**"}}
	registry.failWrite = true
	if err := authority.Apply(request); err == nil {
		t.Fatal("registry write failure reported validation")
	}
	if err := authority.Apply(request); err != nil {
		t.Fatalf("retry after registry failure: %v", err)
	}
	registry.failRead = true
	if err := authority.Apply(WriteRequest{Manifest: request.Manifest, Key: "architecture", Markdown: "second", Globs: []string{"internal/architecture/**"}}); err == nil {
		t.Fatal("registry read failure authorized an apply")
	}
}

func authorityTestManifest() Manifest {
	return Manifest{Sections: []ManifestEntry{
		{Key: "goal", Globs: []string{"internal/goal/**"}},
		{Key: "architecture", Globs: []string{"internal/architecture/**"}},
		{Key: "technology-stack", Globs: []string{"internal/technology/**"}},
		{Key: "execution-contract", Globs: []string{"internal/contract/**"}},
		{Key: "file-structure", Globs: []string{"internal/files/**"}},
		{Key: "task-0001", Globs: []string{"internal/task/**"}},
		{Key: "verification", Globs: []string{"internal/verification/**"}},
	}}
}
