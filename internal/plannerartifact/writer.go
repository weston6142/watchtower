package plannerartifact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func normalizedManifest(manifest Manifest) (Manifest, error) {
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	normalized := Manifest{Sections: make([]ManifestEntry, len(manifest.Sections))}
	for i, entry := range manifest.Sections {
		globs, err := canonicalGlobs(entry.Globs)
		if err != nil {
			return Manifest{}, err
		}
		normalized.Sections[i] = ManifestEntry{Key: entry.Key, Globs: globs}
	}
	return normalized, nil
}

func manifestsEqual(a, b Manifest) bool {
	if len(a.Sections) != len(b.Sections) {
		return false
	}
	for i := range a.Sections {
		if a.Sections[i].Key != b.Sections[i].Key || !sameStrings(a.Sections[i].Globs, b.Sections[i].Globs) {
			return false
		}
	}
	return true
}

func (s *Session) Apply(request WriteRequest) error {
	manifest, err := normalizedManifest(request.Manifest)
	if err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: request.Key, Reason: "request manifest is invalid"}
	}
	if s.manifest == nil {
		s.manifest = &manifest
		if err := s.writeManifest(); err != nil {
			return err
		}
	} else if !manifestsEqual(*s.manifest, manifest) {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "pair", Key: request.Key, Reason: "request manifest changed"}
	}

	entryIndex := -1
	for i, entry := range manifest.Sections {
		if entry.Key == request.Key {
			entryIndex = i
			break
		}
	}
	if entryIndex < 0 {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: request.Key, Reason: "request key is not in manifest"}
	}
	requestGlobs, err := canonicalGlobs(request.Globs)
	if err != nil || !sameStrings(requestGlobs, manifest.Sections[entryIndex].Globs) {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "touchset.json", Key: request.Key, Reason: "request globs do not match manifest"}
	}
	if !utf8.ValidString(request.Markdown) {
		return &DiagnosticError{Scope: ScopeTransport, Artifact: "plan.md", Key: request.Key, Reason: "section is not valid UTF-8"}
	}
	deltaBytes, err := json.Marshal(manifest.Sections[entryIndex].Globs)
	if err != nil {
		return &DiagnosticError{Scope: ScopeTransport, Artifact: "touchset.json", Key: request.Key, Reason: "cannot encode section delta"}
	}
	observed := len([]byte(request.Markdown)) + len(deltaBytes)
	if observed > MaxOperationBytes {
		return &DiagnosticError{Scope: ScopeTransport, Artifact: "section", Key: request.Key, Observed: observed, Limit: MaxOperationBytes, Reason: "operation exceeds bounded write"}
	}

	planPath := filepath.Join(s.workdir, "plan.md")
	touchsetPath := filepath.Join(s.workdir, "touchset.json")
	plan, err := os.ReadFile(planPath)
	if err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: request.Key, Reason: "cannot read current plan"}
	}
	document, err := parsePlan(plan)
	if err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: request.Key, Reason: "current plan is invalid"}
	}
	touchsetBytes, err := os.ReadFile(touchsetPath)
	if err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "touchset.json", Key: request.Key, Reason: "cannot read current touchset"}
	}
	currentTouchset, err := parseTouchset(touchsetBytes)
	if err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "touchset.json", Key: request.Key, Reason: "current touchset is invalid"}
	}
	if err := validateDocumentAgainstManifest(document, manifest, false); err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: request.Key, Reason: "current plan is not a manifest prefix"}
	}
	acceptedGlobs := manifestGlobs(Manifest{Sections: manifest.Sections[:len(document.Sections)]})
	currentGlobs, err := canonicalGlobs(currentTouchset.Globs)
	if err != nil || !sameStrings(currentGlobs, acceptedGlobs) {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "pair", Key: request.Key, Reason: "current plan and touchset are inconsistent"}
	}

	if entryIndex < len(document.Sections) {
		section := document.Sections[entryIndex]
		if section.Key != request.Key || !sameStrings(requestGlobs, manifest.Sections[entryIndex].Globs) || section.Markdown != normalizedMarkdown(request.Markdown) {
			return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: request.Key, Reason: "accepted section conflicts with replay"}
		}
		return nil
	}
	if entryIndex != len(document.Sections) {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: request.Key, Reason: "request skips a pending section"}
	}

	candidatePlan := append([]byte(nil), plan...)
	if len(candidatePlan) > 0 && candidatePlan[len(candidatePlan)-1] != '\n' {
		candidatePlan = append(candidatePlan, '\n')
	}
	candidatePlan = append(candidatePlan, []byte("<!-- watchtower-section: key="+request.Key+" -->\n")...)
	candidatePlan = append(candidatePlan, []byte(normalizedMarkdown(request.Markdown))...)
	candidatePlan = append(candidatePlan, []byte("<!-- watchtower-section-end: key="+request.Key+" -->\n")...)
	mergedGlobs := append([]string(nil), currentTouchset.Globs...)
	mergedGlobs = append(mergedGlobs, manifest.Sections[entryIndex].Globs...)
	mergedGlobs, err = canonicalGlobs(mergedGlobs)
	if err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "touchset.json", Key: request.Key, Reason: "candidate touchset is invalid"}
	}
	candidateTouchset, err := json.Marshal(touchsetDocument{Globs: mergedGlobs})
	if err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "touchset.json", Key: request.Key, Reason: "candidate touchset cannot be encoded"}
	}
	candidateDocument, err := parsePlan(candidatePlan)
	if err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: request.Key, Reason: "candidate plan is invalid"}
	}
	if err := validateDocumentAgainstManifest(candidateDocument, manifest, false); err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: request.Key, Reason: "candidate plan is not a manifest prefix"}
	}
	parsedCandidateTouchset, err := parseTouchset(candidateTouchset)
	if err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "touchset.json", Key: request.Key, Reason: "candidate touchset is invalid"}
	}
	wantGlobs := manifestGlobs(Manifest{Sections: manifest.Sections[:len(candidateDocument.Sections)]})
	gotGlobs, _ := canonicalGlobs(parsedCandidateTouchset.Globs)
	if !sameStrings(gotGlobs, wantGlobs) {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "pair", Key: request.Key, Reason: "candidate plan and touchset are inconsistent"}
	}
	return s.publishPair(candidatePlan, candidateTouchset, request.Key)
}

func normalizedMarkdown(markdown string) string {
	if strings.HasSuffix(markdown, "\n") {
		return markdown
	}
	return markdown + "\n"
}

func (s *Session) writeManifest() error {
	data, err := json.Marshal(s.manifest)
	if err != nil {
		return &DiagnosticError{Scope: ScopeTransport, Artifact: "pair", Reason: "cannot encode planner manifest"}
	}
	return writeSynced(s.manifestPath(), data, 0o600)
}

func (s *Session) publishPair(plan, touchset []byte, key string) error {
	planPath := filepath.Join(s.workdir, "plan.md")
	touchsetPath := filepath.Join(s.workdir, "touchset.json")
	planMode, err := targetMode(planPath)
	if err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: key, Reason: "cannot inspect target mode"}
	}
	touchsetMode, err := targetMode(touchsetPath)
	if err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "touchset.json", Key: key, Reason: "cannot inspect target mode"}
	}
	staging, err := os.MkdirTemp("", "watchtower-planner-apply-")
	if err != nil {
		return &DiagnosticError{Scope: ScopeTransport, Artifact: "pair", Key: key, Reason: "cannot create private staging"}
	}
	if err := os.Chmod(staging, 0o700); err != nil {
		_ = os.RemoveAll(staging)
		return &DiagnosticError{Scope: ScopeTransport, Artifact: "pair", Key: key, Reason: "cannot secure private staging"}
	}
	defer os.RemoveAll(staging)
	stagedPlan := filepath.Join(staging, "plan.md")
	stagedTouchset := filepath.Join(staging, "touchset.json")
	if err := writeSynced(stagedPlan, plan, planMode); err != nil {
		return &DiagnosticError{Scope: ScopeTransport, Artifact: "plan.md", Key: key, Reason: "cannot stage plan"}
	}
	if err := writeSynced(stagedTouchset, touchset, touchsetMode); err != nil {
		return &DiagnosticError{Scope: ScopeTransport, Artifact: "touchset.json", Key: key, Reason: "cannot stage touchset"}
	}
	if err := os.Rename(stagedPlan, planPath); err != nil {
		return &DiagnosticError{Scope: ScopeTransport, Artifact: "pair", Key: key, Reason: "cannot publish plan and touchset"}
	}
	if err := os.Rename(stagedTouchset, touchsetPath); err != nil {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "pair", Key: key, Reason: "partial pair publication requires retry"}
	}
	s.completedKeys = append(s.completedKeys, key)
	return nil
}

func targetMode(path string) (os.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, fmt.Errorf("target is not a regular file")
	}
	return info.Mode().Perm(), nil
}
