package plannerartifact

import (
	"bytes"
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

func TestPlannerArtifactAuthorityDoesNotEchoUntrustedRequestKey(t *testing.T) {
	workdir := t.TempDir()
	authority, err := CreateOrLoad(&authorityTestRegistry{}, Binding{IssueID: "GH-72", Stage: "plan", Attempt: 1, Worktree: workdir})
	if err != nil {
		t.Fatal(err)
	}
	secret := "request-body-not-for-diagnostics"
	err = authority.Apply(WriteRequest{
		Manifest: authorityTestManifest(),
		Key:      secret,
		Markdown: "final reviewed content",
		Globs:    []string{"internal/goal/**"},
	})
	if err == nil {
		t.Fatal("invalid request unexpectedly applied")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("authority error echoed untrusted request key: %v", err)
	}
}

func TestPlannerArtifactAuthorityReloadFailsClosedWhenPairDiffersFromDurableState(t *testing.T) {
	workdir := t.TempDir()
	registry := &authorityTestRegistry{}
	binding := Binding{IssueID: "GH-72", Stage: "plan", Attempt: 1, Worktree: workdir}
	authority, err := CreateOrLoad(registry, binding)
	if err != nil {
		t.Fatal(err)
	}
	manifest := authorityTestManifest()
	if err := authority.Apply(WriteRequest{Manifest: manifest, Key: "goal", Markdown: "durable content", Globs: manifest.Sections[0].Globs}); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(workdir, "plan.md")
	plan, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	plan = bytes.Replace(plan, []byte("durable content"), []byte("tampered content"), 1)
	if err := os.WriteFile(planPath, plan, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateOrLoad(registry, binding); err == nil || ErrorClassOf(err) != ErrorAuthorityState {
		t.Fatalf("reload after durable-pair tampering = %v, want authority_state_unavailable", err)
	}
}

func TestAuthorityFirstApplyPublishesFinalSectionAndDurablePrefix(t *testing.T) {
	workdir := t.TempDir()
	registry := &authorityTestRegistry{}
	authority, err := CreateOrLoad(registry, Binding{IssueID: "GH-72", Stage: "plan", Attempt: 1, Worktree: workdir})
	if err != nil {
		t.Fatal(err)
	}

	manifest := authorityTestManifest()
	request := WriteRequest{
		Manifest: manifest,
		Key:      "goal",
		Markdown: "The final reviewed planner goal crosses the daemon boundary.",
		Globs:    manifest.Sections[0].Globs,
	}
	if err := authority.Apply(request); err != nil {
		t.Fatalf("first final apply: %v", err)
	}

	plan := mustReadAuthorityFile(t, workdir, "plan.md")
	if !strings.Contains(string(plan), request.Markdown) {
		t.Fatalf("plan omitted final section: %q", plan)
	}
	touchset := mustReadAuthorityFile(t, workdir, "touchset.json")
	if !strings.Contains(string(touchset), manifest.Sections[0].Globs[0]) {
		t.Fatalf("touchset omitted canonical delta: %q", touchset)
	}
	status, _, storedManifest, storedSections, found, err := registry.LoadPlannerArtifact("GH-72", "plan", 1, workdir)
	if err != nil || !found || status != "active" {
		t.Fatalf("durable authority row = status %q found %v err %v", status, found, err)
	}
	if !strings.Contains(string(storedManifest), `"goal"`) || !strings.Contains(string(storedSections), request.Markdown) {
		t.Fatalf("durable prefix omitted first section: manifest=%s sections=%s", storedManifest, storedSections)
	}
}

func TestAuthorityRetryRetainsPrefixAndRejectsReplacedHandle(t *testing.T) {
	workdir := t.TempDir()
	registry := &authorityTestRegistry{}
	binding := Binding{IssueID: "GH-72", Stage: "plan", Attempt: 1, Worktree: workdir}
	first, err := CreateOrLoad(registry, binding)
	if err != nil {
		t.Fatal(err)
	}
	manifest := authorityTestManifest()
	goal := WriteRequest{Manifest: manifest, Key: "goal", Markdown: "final goal", Globs: manifest.Sections[0].Globs}
	if err := first.Apply(goal); err != nil {
		t.Fatal(err)
	}

	retry, err := CreateOrLoad(registry, binding)
	if err != nil {
		t.Fatal(err)
	}
	architecture := WriteRequest{Manifest: manifest, Key: "architecture", Markdown: "final architecture", Globs: manifest.Sections[1].Globs}
	if err := first.Apply(architecture); err == nil {
		t.Fatal("replaced capability remained authorized")
	}
	if err := retry.Apply(architecture); err != nil {
		t.Fatalf("retry lost validated prefix: %v", err)
	}
	plan := string(mustReadAuthorityFile(t, workdir, "plan.md"))
	if strings.Count(plan, "key=goal") != 2 || strings.Count(plan, "key=architecture") != 2 {
		t.Fatalf("retry did not retain exactly one prefix and next section: %q", plan)
	}
}

func TestAuthorityRejectsPrivateSessionAndDescriptorAuthority(t *testing.T) {
	workdir := t.TempDir()
	authority, err := CreateOrLoad(&authorityTestRegistry{}, Binding{IssueID: "GH-72", Stage: "plan", Attempt: 1, Worktree: workdir})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.ApplyPlannerArtifact("WATCHTOWER_PLANNER_SESSION=private"); err == nil || !strings.Contains(err.Error(), "private_session_rejected") {
		t.Fatalf("private authority error = %v, want private_session_rejected", err)
	}
	if err := authority.ApplyWithCapability("WATCHTOWER_PLANNER_SESSION=private", WriteRequest{}); err == nil || !strings.Contains(err.Error(), "private_session_rejected") {
		t.Fatalf("private capability error = %v, want private_session_rejected", err)
	}
	if err := ApplyFromFD(-1, WriteRequest{}); err == nil || !strings.Contains(err.Error(), "descriptor_non_authoritative") {
		t.Fatalf("descriptor authority error = %v, want descriptor_non_authoritative", err)
	}
}

func TestAuthorityRejectsInvalidSectionBeforeMutation(t *testing.T) {
	cases := []struct {
		name     string
		markdown string
		globs    []string
	}{
		{name: "empty markdown", markdown: "   "},
		{name: "placeholder", markdown: "[transport probe]"},
		{name: "empty delta", markdown: "final", globs: []string{}},
		{name: "traversal delta", markdown: "final", globs: []string{"../synthetic/**"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workdir := t.TempDir()
			registry := &authorityTestRegistry{}
			authority, err := CreateOrLoad(registry, Binding{IssueID: "GH-72", Stage: "plan", Attempt: 1, Worktree: workdir})
			if err != nil {
				t.Fatal(err)
			}
			manifest := authorityTestManifest()
			if tc.globs != nil {
				manifest.Sections[0].Globs = tc.globs
			}
			beforePlan := mustReadAuthorityFile(t, workdir, "plan.md")
			beforeTouchset := mustReadAuthorityFile(t, workdir, "touchset.json")
			err = authority.Apply(WriteRequest{Manifest: manifest, Key: "goal", Markdown: tc.markdown, Globs: manifest.Sections[0].Globs})
			if err == nil || !strings.Contains(err.Error(), "invalid_section") {
				t.Fatalf("invalid section error = %v, want invalid_section", err)
			}
			if !bytes.Equal(beforePlan, mustReadAuthorityFile(t, workdir, "plan.md")) ||
				!bytes.Equal(beforeTouchset, mustReadAuthorityFile(t, workdir, "touchset.json")) {
				t.Fatal("rejected first apply changed the artifact pair")
			}
		})
	}
}

func mustReadAuthorityFile(t *testing.T, workdir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workdir, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
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
