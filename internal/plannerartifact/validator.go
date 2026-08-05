package plannerartifact

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const planRoot = "# Implementation Plan\n\n"

type planDocument struct {
	Sections []planSection
	Keys     []string
}

type planSection struct {
	Key      string
	Markdown string
}

type touchsetDocument struct {
	Globs []string `json:"globs"`
}

func parsePlan(data []byte) (planDocument, error) {
	if !utf8.Valid(data) {
		return planDocument{}, &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Reason: "plan is not valid UTF-8"}
	}
	text := string(data)
	if !strings.HasPrefix(text, planRoot) {
		return planDocument{}, &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Reason: "plan root is invalid"}
	}
	rest := text[len(planRoot):]
	if rest == "" {
		return planDocument{}, nil
	}
	lines := strings.SplitAfter(rest, "\n")
	document := planDocument{}
	seen := make(map[string]struct{})
	lastRank := -1
	lastTask := -1
	for i := 0; i < len(lines); {
		raw := lines[i]
		line := strings.TrimSuffix(raw, "\n")
		if line == "" {
			i++
			continue
		}
		key, ok := parseAnchorLine(line, "<!-- watchtower-section: key=")
		if !ok {
			return planDocument{}, &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Reason: "section start is invalid"}
		}
		if !validKey(key) {
			return planDocument{}, &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: key, Reason: "section key is invalid"}
		}
		if _, exists := seen[key]; exists {
			return planDocument{}, &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: key, Reason: "section key is duplicated"}
		}
		seen[key] = struct{}{}
		rank := anchorRank(key)
		if rank >= 0 && rank < lastRank {
			return planDocument{}, &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: key, Reason: "sections are out of order"}
		}
		if task, ok := taskNumber(key); ok {
			if lastTask >= 0 && task <= lastTask {
				return planDocument{}, &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: key, Reason: "task sections are out of order"}
			}
			lastTask = task
		}
		if rank >= 0 {
			lastRank = rank
		}
		if !strings.HasSuffix(raw, "\n") {
			return planDocument{}, &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: key, Reason: "section start is unterminated"}
		}
		i++
		var markdown strings.Builder
		terminated := false
		for i < len(lines) {
			contentRaw := lines[i]
			contentLine := strings.TrimSuffix(contentRaw, "\n")
			if endKey, isEnd := parseAnchorLine(contentLine, "<!-- watchtower-section-end: key="); isEnd {
				if endKey != key {
					return planDocument{}, &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: key, Reason: "section end key does not match"}
				}
				terminated = true
				i++
				break
			}
			if _, isStart := parseAnchorLine(contentLine, "<!-- watchtower-section: key="); isStart {
				return planDocument{}, &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: key, Reason: "section end is missing"}
			}
			markdown.WriteString(contentRaw)
			i++
		}
		if !terminated {
			return planDocument{}, &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: key, Reason: "section end is missing"}
		}
		document.Sections = append(document.Sections, planSection{Key: key, Markdown: markdown.String()})
		document.Keys = append(document.Keys, key)
	}
	return document, nil
}

func parseAnchorLine(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, " -->") {
		return "", false
	}
	key := strings.TrimSuffix(strings.TrimPrefix(line, prefix), " -->")
	if key == "" || strings.ContainsAny(key, " \t") {
		return key, false
	}
	return key, true
}

func anchorRank(key string) int {
	for i, required := range requiredManifestPrefix {
		if key == required {
			return i
		}
	}
	if task, ok := taskNumber(key); ok {
		return len(requiredManifestPrefix) + task
	}
	if key == "verification" {
		return len(requiredManifestPrefix) + 100000
	}
	return -1
}

func parseTouchset(data []byte) (touchsetDocument, error) {
	if !utf8.Valid(data) {
		return touchsetDocument{}, fmt.Errorf("touchset is not valid UTF-8")
	}
	var document touchsetDocument
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return touchsetDocument{}, fmt.Errorf("touchset JSON is invalid")
	}
	if err := ensureEOF(decoder); err != nil {
		return touchsetDocument{}, fmt.Errorf("touchset JSON has trailing data")
	}
	if document.Globs == nil {
		return touchsetDocument{}, fmt.Errorf("touchset globs must be an array")
	}
	for _, glob := range document.Globs {
		if _, err := canonicalGlob(glob); err != nil {
			return touchsetDocument{}, fmt.Errorf("touchset glob is invalid")
		}
	}
	return document, nil
}

func validateDocumentAgainstManifest(document planDocument, manifest Manifest, complete bool) error {
	if len(document.Sections) > len(manifest.Sections) {
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Reason: "plan has more sections than its manifest"}
	}
	for i, section := range document.Sections {
		if i >= len(manifest.Sections) || section.Key != manifest.Sections[i].Key {
			return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: section.Key, Reason: "section does not match manifest order"}
		}
	}
	if complete && len(document.Sections) != len(manifest.Sections) {
		key := ""
		if len(document.Sections) < len(manifest.Sections) {
			key = manifest.Sections[len(document.Sections)].Key
		}
		return &DiagnosticError{Scope: ScopeSectionStructure, Artifact: "plan.md", Key: key, Reason: "manifest section is incomplete"}
	}
	return nil
}

func manifestGlobs(manifest Manifest) []string {
	var globs []string
	for _, entry := range manifest.Sections {
		canonical, _ := canonicalGlobs(entry.Globs)
		globs = append(globs, canonical...)
	}
	canonical, _ := canonicalGlobs(globs)
	return canonical
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
