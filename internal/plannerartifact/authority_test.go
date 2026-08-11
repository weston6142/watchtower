package plannerartifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type authorityTestRegistry struct {
	mu            sync.Mutex
	status        string
	digest        []byte
	manifest      []byte
	sections      []byte
	found         bool
	failRead      bool
	failWrite     bool
	priorAttempt  int
	priorStatus   string
	priorDigest   []byte
	priorManifest []byte
	priorSections []byte
	priorFound    bool
}

type scopedAuthorityRecord struct {
	status   string
	digest   []byte
	manifest []byte
	sections []byte
}

type scopedAuthorityTestRegistry struct {
	mu      sync.Mutex
	records map[int]scopedAuthorityRecord
}

func (r *scopedAuthorityTestRegistry) LoadPlannerArtifact(_ string, _ string, attempt int, _ string) (string, []byte, []byte, []byte, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, found := r.records[attempt]
	return record.status, append([]byte(nil), record.digest...), append([]byte(nil), record.manifest...), append([]byte(nil), record.sections...), found, nil
}

func (r *scopedAuthorityTestRegistry) LoadLatestPlannerArtifactBefore(_ string, _ string, beforeAttempt int, _ string) (int, string, []byte, []byte, []byte, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	latest := 0
	for attempt, record := range r.records {
		if attempt < beforeAttempt && attempt > latest && record.status == "active" {
			latest = attempt
		}
	}
	if latest == 0 {
		return 0, "", nil, nil, nil, false, nil
	}
	record := r.records[latest]
	return latest, record.status, append([]byte(nil), record.digest...), append([]byte(nil), record.manifest...), append([]byte(nil), record.sections...), true, nil
}

func (r *scopedAuthorityTestRegistry) CreatePlannerArtifact(_ string, _ string, attempt int, _ string, status string, digest, manifest, sections []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.records == nil {
		r.records = make(map[int]scopedAuthorityRecord)
	}
	r.records[attempt] = scopedAuthorityRecord{status: status, digest: append([]byte(nil), digest...), manifest: append([]byte(nil), manifest...), sections: append([]byte(nil), sections...)}
	return nil
}

func (r *scopedAuthorityTestRegistry) UpdatePlannerArtifact(issue, stage string, attempt int, worktree, status string, digest, manifest, sections []byte) error {
	return r.CreatePlannerArtifact(issue, stage, attempt, worktree, status, digest, manifest, sections)
}

func (r *scopedAuthorityTestRegistry) PromotePlannerArtifact(_ string, _ string, priorAttempt, attempt int, _ string, digest, manifest, sections []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	prior := r.records[priorAttempt]
	prior.status = "expired"
	r.records[priorAttempt] = prior
	r.records[attempt] = scopedAuthorityRecord{status: "active", digest: append([]byte(nil), digest...), manifest: append([]byte(nil), manifest...), sections: append([]byte(nil), sections...)}
	return nil
}

func (r *scopedAuthorityTestRegistry) ExpirePlannerArtifact(_ string, _ string, attempt int, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.records[attempt]
	record.status = "expired"
	r.records[attempt] = record
	return nil
}

func (r *authorityTestRegistry) LoadLatestPlannerArtifactBefore(string, string, int, string) (int, string, []byte, []byte, []byte, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.priorAttempt, r.priorStatus, append([]byte(nil), r.priorDigest...), append([]byte(nil), r.priorManifest...), append([]byte(nil), r.priorSections...), r.priorFound, nil
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

func (r *authorityTestRegistry) PromotePlannerArtifact(issue, stage string, _ int, attempt int, worktree string, digest, manifest, sections []byte) error {
	return r.CreatePlannerArtifact(issue, stage, attempt, worktree, "active", digest, manifest, sections)
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

func TestCreateOrLoadRestoresTamperedPlanFromExactDurableAuthority(t *testing.T) {
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
	reloaded, err := CreateOrLoad(registry, binding)
	if err != nil {
		t.Fatalf("reload after durable-pair tampering: %v", err)
	}
	if !bytes.Equal(mustReadAuthorityFile(t, workdir, "plan.md"), mustRenderAuthorityPlan(t, "durable content")) {
		t.Fatal("reload did not restore exact durable plan bytes")
	}
	accepted, err := reloaded.AcceptedSections()
	if err != nil || len(accepted) != 1 || accepted[0].Markdown != "durable content\n" {
		t.Fatalf("restored accepted prefix = %#v, err=%v", accepted, err)
	}
	if err := reloaded.Apply(WriteRequest{Manifest: manifest, Key: "architecture", Markdown: "usable after recovery", Globs: manifest.Sections[1].Globs}); err != nil {
		t.Fatalf("restored authority was not usable: %v", err)
	}
}

func TestCreateOrLoadRestoresMissingAndTamperedDurablePairs(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(t *testing.T, workdir string)
	}{
		{name: "missing plan", mutate: func(t *testing.T, workdir string) { t.Helper(); mustRemoveAuthorityFile(t, workdir, "plan.md") }},
		{name: "missing touchset", mutate: func(t *testing.T, workdir string) { t.Helper(); mustRemoveAuthorityFile(t, workdir, "touchset.json") }},
		{name: "both missing", mutate: func(t *testing.T, workdir string) {
			t.Helper()
			mustRemoveAuthorityFile(t, workdir, "plan.md")
			mustRemoveAuthorityFile(t, workdir, "touchset.json")
		}},
		{name: "tampered plan", mutate: func(t *testing.T, workdir string) {
			t.Helper()
			mustWriteAuthorityFile(t, workdir, "plan.md", []byte("tampered-plan-secret"))
		}},
		{name: "tampered touchset", mutate: func(t *testing.T, workdir string) {
			t.Helper()
			mustWriteAuthorityFile(t, workdir, "touchset.json", []byte(`{"globs":["tampered/**"]}`))
		}},
		{name: "both tampered", mutate: func(t *testing.T, workdir string) {
			t.Helper()
			mustWriteAuthorityFile(t, workdir, "plan.md", []byte("tampered-plan-secret"))
			mustWriteAuthorityFile(t, workdir, "touchset.json", []byte("tampered-touchset-secret"))
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			workdir := t.TempDir()
			registry := &authorityTestRegistry{}
			binding := Binding{IssueID: "GH-77", Stage: "plan", Attempt: 1, Worktree: workdir}
			authority, err := CreateOrLoad(registry, binding)
			if err != nil {
				t.Fatal(err)
			}
			manifest := authorityTestManifest()
			goal := WriteRequest{Manifest: manifest, Key: "goal", Markdown: "durable goal", Globs: manifest.Sections[0].Globs}
			if err := authority.Apply(goal); err != nil {
				t.Fatal(err)
			}
			wantPlan := append([]byte(nil), mustReadAuthorityFile(t, workdir, "plan.md")...)
			wantTouchset := append([]byte(nil), mustReadAuthorityFile(t, workdir, "touchset.json")...)
			mutation.mutate(t, workdir)

			reloaded, err := CreateOrLoad(registry, binding)
			if err != nil {
				t.Fatalf("restore durable pair: %v", err)
			}
			if !bytes.Equal(mustReadAuthorityFile(t, workdir, "plan.md"), wantPlan) || !bytes.Equal(mustReadAuthorityFile(t, workdir, "touchset.json"), wantTouchset) {
				t.Fatal("reload did not restore the exact durable pair")
			}
			accepted, err := reloaded.AcceptedSections()
			if err != nil || len(accepted) != 1 || accepted[0].Markdown != "durable goal\n" {
				t.Fatalf("accepted prefix after restore = %#v, err=%v", accepted, err)
			}
		})
	}
}

func TestCreateOrLoadRejectsCorruptDurableAuthorityWithoutWorkspaceAdoption(t *testing.T) {
	manifest := authorityTestManifest()
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	validSections, err := json.Marshal([]acceptedSection{{Key: "goal", Markdown: "durable-secret-marker\n", Globs: manifest.Sections[0].Globs}})
	if err != nil {
		t.Fatal(err)
	}
	validDigest := bytes.Repeat([]byte{0x42}, sha256.Size)

	noncanonicalManifest := authorityTestManifest()
	noncanonicalManifest.Sections[0].Globs = []string{"./internal/goal/**"}
	noncanonicalManifestBytes, _ := json.Marshal(noncanonicalManifest)
	duplicateManifest := authorityTestManifest()
	duplicateManifest.Sections[0].Globs = []string{"internal/goal/**", "internal/goal/**"}
	duplicateManifestBytes, _ := json.Marshal(duplicateManifest)
	wrongKeySections, _ := json.Marshal([]acceptedSection{{Key: "architecture", Markdown: "durable-secret-marker\n", Globs: manifest.Sections[1].Globs}})
	noncontiguousSections, _ := json.Marshal([]acceptedSection{
		{Key: "goal", Markdown: "durable-secret-marker\n", Globs: manifest.Sections[0].Globs},
		{Key: "technology-stack", Markdown: "durable-secret-marker\n", Globs: manifest.Sections[2].Globs},
	})
	mismatchedGlobSections, _ := json.Marshal([]acceptedSection{{Key: "goal", Markdown: "durable-secret-marker\n", Globs: []string{"internal/other/**"}}})
	noncanonicalGlobSections, _ := json.Marshal([]acceptedSection{{Key: "goal", Markdown: "durable-secret-marker\n", Globs: []string{"./internal/goal/**"}}})
	nonnormalizedMarkdownSections, _ := json.Marshal([]acceptedSection{{Key: "goal", Markdown: "durable-secret-marker", Globs: manifest.Sections[0].Globs}})
	placeholderMarkdownSections, _ := json.Marshal([]acceptedSection{{Key: "goal", Markdown: "[transport probe]\n", Globs: manifest.Sections[0].Globs}})
	invalidUTF8Sections := []byte("[{\"key\":\"goal\",\"markdown\":\"")
	invalidUTF8Sections = append(invalidUTF8Sections, 0xff)
	invalidUTF8Sections = append(invalidUTF8Sections, []byte("\",\"globs\":[\"internal/goal/**\"]}]")...)
	overLimitSections, _ := json.Marshal([]acceptedSection{{Key: "goal", Markdown: strings.Repeat("x", MaxOperationBytes) + "\n", Globs: manifest.Sections[0].Globs}})

	cases := []struct {
		name                       string
		status                     string
		digest, manifest, sections []byte
		priorAttempt               int
	}{
		{name: "missing digest", status: "active", manifest: manifestBytes, sections: validSections},
		{name: "short digest", status: "active", digest: []byte("short"), manifest: manifestBytes, sections: validSections},
		{name: "malformed manifest JSON", status: "active", digest: validDigest, manifest: []byte("durable-secret-marker{"), sections: validSections},
		{name: "malformed sections JSON", status: "active", digest: validDigest, manifest: manifestBytes, sections: []byte("durable-secret-marker[")},
		{name: "sections without manifest", status: "active", digest: validDigest, sections: validSections},
		{name: "wrong first section key", status: "active", digest: validDigest, manifest: manifestBytes, sections: wrongKeySections},
		{name: "noncontiguous sections", status: "active", digest: validDigest, manifest: manifestBytes, sections: noncontiguousSections},
		{name: "mismatched section globs", status: "active", digest: validDigest, manifest: manifestBytes, sections: mismatchedGlobSections},
		{name: "noncanonical manifest glob", status: "active", digest: validDigest, manifest: noncanonicalManifestBytes, sections: validSections},
		{name: "duplicate manifest glob", status: "active", digest: validDigest, manifest: duplicateManifestBytes, sections: validSections},
		{name: "noncanonical section glob", status: "active", digest: validDigest, manifest: manifestBytes, sections: noncanonicalGlobSections},
		{name: "nonnormalized markdown", status: "active", digest: validDigest, manifest: manifestBytes, sections: nonnormalizedMarkdownSections},
		{name: "placeholder markdown", status: "active", digest: validDigest, manifest: manifestBytes, sections: placeholderMarkdownSections},
		{name: "invalid UTF-8 markdown", status: "active", digest: validDigest, manifest: manifestBytes, sections: invalidUTF8Sections},
		{name: "over-limit markdown", status: "active", digest: validDigest, manifest: manifestBytes, sections: overLimitSections},
		{name: "inactive record", status: "expired", digest: validDigest, manifest: manifestBytes, sections: validSections},
		{name: "equal prior attempt", status: "active", digest: validDigest, manifest: manifestBytes, sections: validSections, priorAttempt: 2},
		{name: "future prior attempt", status: "active", digest: validDigest, manifest: manifestBytes, sections: validSections, priorAttempt: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workdir := t.TempDir()
			beforePlan := []byte(planRoot)
			beforeTouchset := []byte(`{"globs":[]}`)
			mustWriteAuthorityFile(t, workdir, "plan.md", beforePlan)
			mustWriteAuthorityFile(t, workdir, "touchset.json", beforeTouchset)
			registry := &authorityTestRegistry{}
			if tc.priorAttempt == 0 {
				registry.status, registry.digest, registry.manifest, registry.sections, registry.found = tc.status, tc.digest, tc.manifest, tc.sections, true
			} else {
				registry.priorAttempt, registry.priorStatus, registry.priorDigest, registry.priorManifest, registry.priorSections, registry.priorFound = tc.priorAttempt, tc.status, tc.digest, tc.manifest, tc.sections, true
			}
			authority, err := CreateOrLoad(registry, Binding{IssueID: "GH-77", Stage: "plan", Attempt: 2, Worktree: workdir}, RequireDurableRecovery())
			if authority != nil || err == nil || ErrorClassOf(err) != ErrorAuthorityState {
				t.Fatalf("corrupt reload = authority %#v err %v, want nil authority_state_unavailable", authority, err)
			}
			if strings.Contains(err.Error(), "durable-secret-marker") || strings.Contains(err.Error(), workdir) {
				t.Fatalf("safe error disclosed durable content or worktree: %v", err)
			}
			if !bytes.Equal(mustReadAuthorityFile(t, workdir, "plan.md"), beforePlan) || !bytes.Equal(mustReadAuthorityFile(t, workdir, "touchset.json"), beforeTouchset) {
				t.Fatal("corrupt durable state was adopted into the workspace")
			}
		})
	}
}

func TestCreateOrLoadPreservesFreshAndEmptyBehavior(t *testing.T) {
	t.Run("fresh default creates empty durable authority", func(t *testing.T) {
		workdir := t.TempDir()
		registry := &authorityTestRegistry{}
		authority, err := CreateOrLoad(registry, Binding{IssueID: "GH-77", Stage: "plan", Attempt: 1, Worktree: workdir})
		if err != nil || authority == nil {
			t.Fatalf("fresh authority = %#v err=%v", authority, err)
		}
		if !registry.found || len(registry.digest) != sha256.Size || string(registry.sections) != "[]" {
			t.Fatalf("fresh durable record = found=%v digest=%d sections=%q", registry.found, len(registry.digest), registry.sections)
		}
		if string(mustReadAuthorityFile(t, workdir, "plan.md")) != planRoot || string(mustReadAuthorityFile(t, workdir, "touchset.json")) != `{"globs":[]}` {
			t.Fatal("fresh authority did not create the empty pair")
		}
	})
	t.Run("required recovery without record creates nothing", func(t *testing.T) {
		workdir := t.TempDir()
		registry := &authorityTestRegistry{}
		authority, err := CreateOrLoad(registry, Binding{IssueID: "GH-77", Stage: "plan", Attempt: 1, Worktree: workdir}, RequireDurableRecovery())
		if authority != nil || err == nil || ErrorClassOf(err) != ErrorAuthorityState {
			t.Fatalf("required recovery = %#v err=%v", authority, err)
		}
		if registry.found {
			t.Fatal("required recovery created a durable row")
		}
		if _, err := os.Stat(filepath.Join(workdir, "plan.md")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("required recovery created plan: %v", err)
		}
		if _, err := os.Stat(filepath.Join(workdir, "touchset.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("required recovery created touchset: %v", err)
		}
	})
	t.Run("empty exact record retains empty pair", func(t *testing.T) {
		workdir := t.TempDir()
		registry := &authorityTestRegistry{status: "active", digest: bytes.Repeat([]byte{1}, sha256.Size), sections: []byte("[]"), found: true}
		authority, err := CreateOrLoad(registry, Binding{IssueID: "GH-77", Stage: "plan", Attempt: 1, Worktree: workdir}, RequireDurableRecovery())
		if err != nil || authority == nil {
			t.Fatalf("empty durable reload = %#v err=%v", authority, err)
		}
		if string(mustReadAuthorityFile(t, workdir, "plan.md")) != planRoot || string(mustReadAuthorityFile(t, workdir, "touchset.json")) != `{"globs":[]}` {
			t.Fatal("empty durable reload changed empty pair")
		}
	})
}

func TestAuthorityRecoverySerializesOldAuthorityOperations(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		name := "successful recovery stales old authority"
		if failSecond {
			name = "failed recovery preserves old authority"
		}
		t.Run(name, func(t *testing.T) {
			workdir := t.TempDir()
			registry := &authorityTestRegistry{}
			binding := Binding{IssueID: "GH-77", Stage: "plan", Attempt: 1, Worktree: workdir}
			old, err := CreateOrLoad(registry, binding)
			if err != nil {
				t.Fatal(err)
			}
			manifest := authorityTestManifest()
			if err := old.Apply(WriteRequest{Manifest: manifest, Key: "goal", Markdown: "serialized goal", Globs: manifest.Sections[0].Globs}); err != nil {
				t.Fatal(err)
			}
			oldDigest := append([]byte(nil), registry.digest...)
			canonicalWorktree := old.Binding().Worktree
			planPath := filepath.Join(canonicalWorktree, "plan.md")
			touchsetPath := filepath.Join(canonicalWorktree, "touchset.json")
			publishedPlan := make(chan struct{})
			continueRecovery := make(chan struct{})
			paused := false
			failed := false
			rename := func(oldPath, newPath string) error {
				if err := os.Rename(oldPath, newPath); err != nil {
					return err
				}
				if !paused && newPath == planPath {
					paused = true
					close(publishedPlan)
					<-continueRecovery
				}
				if failSecond && !failed && newPath == touchsetPath {
					failed = true
					return errors.New("injected second publish failure")
				}
				return nil
			}
			type reloadResult struct {
				authority *Authority
				err       error
			}
			reloaded := make(chan reloadResult, 1)
			go func() {
				authority, err := CreateOrLoad(registry, binding, withRecoveryRename(rename))
				reloaded <- reloadResult{authority: authority, err: err}
			}()
			select {
			case <-publishedPlan:
			case result := <-reloaded:
				t.Fatalf("recovery returned before publishing the first target: %v", result.err)
			case <-time.After(2 * time.Second):
				t.Fatal("recovery did not publish the first target")
			}

			applyDone := make(chan error, 1)
			go func() {
				applyDone <- old.Apply(WriteRequest{Manifest: manifest, Key: "architecture", Markdown: "serialized architecture", Globs: manifest.Sections[1].Globs})
			}()
			select {
			case err := <-applyDone:
				t.Fatalf("old authority completed while recovery was partial: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			close(continueRecovery)
			result := <-reloaded
			oldApplyErr := <-applyDone
			if failSecond {
				if result.authority != nil || result.err == nil || ErrorClassOf(result.err) != ErrorAuthorityState {
					t.Fatalf("failed recovery = %#v err=%v", result.authority, result.err)
				}
				if !bytes.Equal(registry.digest, oldDigest) {
					t.Fatal("failed recovery rotated durable digest")
				}
				if oldApplyErr != nil {
					t.Fatalf("old authority did not proceed after rollback: %v", oldApplyErr)
				}
				return
			}
			if result.err != nil || result.authority == nil {
				t.Fatalf("successful recovery = %#v err=%v", result.authority, result.err)
			}
			if oldApplyErr == nil || ErrorClassOf(oldApplyErr) != ErrorStaleCapability {
				t.Fatalf("old authority after rotation = %v", oldApplyErr)
			}
		})
	}
}

func TestAuthorityRecoverySerializesAcrossAttemptsAndStalesPriorManifest(t *testing.T) {
	workdir := t.TempDir()
	registry := &scopedAuthorityTestRegistry{records: make(map[int]scopedAuthorityRecord)}
	firstBinding := Binding{IssueID: "GH-77", Stage: "plan", Attempt: 1, Worktree: workdir}
	old, err := CreateOrLoad(registry, firstBinding)
	if err != nil {
		t.Fatal(err)
	}
	manifest := authorityTestManifest()
	if err := old.Apply(WriteRequest{Manifest: manifest, Key: "goal", Markdown: "durable goal", Globs: manifest.Sections[0].Globs}); err != nil {
		t.Fatal(err)
	}

	planPath := filepath.Join(old.Binding().Worktree, "plan.md")
	publishedPlan := make(chan struct{})
	continueRecovery := make(chan struct{})
	rename := func(oldPath, newPath string) error {
		if err := os.Rename(oldPath, newPath); err != nil {
			return err
		}
		if newPath == planPath {
			close(publishedPlan)
			<-continueRecovery
		}
		return nil
	}
	type reloadResult struct {
		authority *Authority
		err       error
	}
	reloaded := make(chan reloadResult, 1)
	go func() {
		authority, err := CreateOrLoad(registry, Binding{IssueID: "GH-77", Stage: "plan", Attempt: 2, Worktree: workdir}, RequireDurableRecovery(), withRecoveryRename(rename))
		reloaded <- reloadResult{authority: authority, err: err}
	}()
	select {
	case <-publishedPlan:
	case result := <-reloaded:
		t.Fatalf("recovery returned before publishing the first target: %v", result.err)
	case <-time.After(2 * time.Second):
		t.Fatal("cross-attempt recovery did not publish the first target")
	}

	changedManifest := authorityTestManifest()
	changedManifest.Sections[1].Globs = []string{"internal/changed/**"}
	oldApply := make(chan error, 1)
	go func() {
		oldApply <- old.Apply(WriteRequest{Manifest: changedManifest, Key: "architecture", Markdown: "stale architecture", Globs: changedManifest.Sections[1].Globs})
	}()
	select {
	case err := <-oldApply:
		close(continueRecovery)
		<-reloaded
		t.Fatalf("prior authority completed while cross-attempt recovery was partial: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(continueRecovery)
	result := <-reloaded
	if result.err != nil || result.authority == nil {
		t.Fatalf("cross-attempt recovery = %#v err=%v", result.authority, result.err)
	}
	if err := <-oldApply; ErrorClassOf(err) != ErrorStaleCapability {
		t.Fatalf("prior authority after recovery = %v, want stale capability", err)
	}
	if err := result.authority.Apply(WriteRequest{Manifest: manifest, Key: "architecture", Markdown: "current architecture", Globs: manifest.Sections[1].Globs}); err != nil {
		t.Fatalf("current authority rejected original manifest: %v", err)
	}
}

func TestStaleAuthorityCannotExpireRotatedBinding(t *testing.T) {
	workdir := t.TempDir()
	registry := &authorityTestRegistry{}
	binding := Binding{IssueID: "GH-77", Stage: "plan", Attempt: 1, Worktree: workdir}
	old, err := CreateOrLoad(registry, binding)
	if err != nil {
		t.Fatal(err)
	}
	manifest := authorityTestManifest()
	if err := old.Apply(WriteRequest{Manifest: manifest, Key: "goal", Markdown: "durable goal", Globs: manifest.Sections[0].Globs}); err != nil {
		t.Fatal(err)
	}
	current, err := CreateOrLoad(registry, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Expire(); ErrorClassOf(err) != ErrorStaleCapability {
		t.Fatalf("stale expiration = %v, want stale capability", err)
	}
	if err := current.Apply(WriteRequest{Manifest: manifest, Key: "architecture", Markdown: "still active", Globs: manifest.Sections[1].Globs}); err != nil {
		t.Fatalf("current authority after stale expiration: %v", err)
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

func mustWriteAuthorityFile(t *testing.T, workdir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workdir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRemoveAuthorityFile(t *testing.T, workdir, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(workdir, name)); err != nil {
		t.Fatal(err)
	}
}

func mustRenderAuthorityPlan(t *testing.T, markdown string) []byte {
	t.Helper()
	plan, _, err := renderPair([]acceptedSection{{Key: "goal", Markdown: normalizedMarkdown(markdown), Globs: authorityTestManifest().Sections[0].Globs}})
	if err != nil {
		t.Fatal(err)
	}
	return plan
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
