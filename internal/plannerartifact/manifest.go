package plannerartifact

import (
	"fmt"
	"strings"

	"github.com/weston6142/watchtower/internal/touchset"
)

const MaxOperationBytes = 64 * 1024

const (
	ScopeTransport                 DiagnosticScope = "transport"
	ScopeSectionStructure          DiagnosticScope = "section-structure"
	ScopeMalformedStartingArtifact DiagnosticScope = "malformed-starting-artifact"
	ScopeFinalValidation           DiagnosticScope = "final-validation"
)

type DiagnosticScope string

type DiagnosticError struct {
	Scope    DiagnosticScope
	Artifact string
	Key      string
	Observed int
	Limit    int
	Reason   string
}

func (e *DiagnosticError) Error() string {
	parts := []string{string(e.Scope)}
	if e.Artifact != "" {
		parts = append(parts, "artifact="+e.Artifact)
	}
	if e.Key != "" {
		parts = append(parts, "key="+e.Key)
	}
	if e.Observed != 0 {
		parts = append(parts, fmt.Sprintf("observed=%d", e.Observed))
	}
	if e.Limit != 0 {
		parts = append(parts, fmt.Sprintf("limit=%d", e.Limit))
	}
	if e.Reason != "" {
		parts = append(parts, e.Reason)
	}
	return "planner artifact: " + strings.Join(parts, " ")
}

type ManifestEntry struct {
	Key   string   `json:"key"`
	Globs []string `json:"globs"`
}

type Manifest struct {
	Sections []ManifestEntry `json:"sections"`
}

type WriteRequest struct {
	Manifest Manifest `json:"manifest"`
	Key      string   `json:"key"`
	Markdown string   `json:"markdown"`
	Globs    []string `json:"globs"`
}

func ValidateManifest(manifest Manifest) error {
	if len(manifest.Sections) < 7 {
		return fmt.Errorf("manifest requires at least seven sections")
	}
	seen := make(map[string]struct{}, len(manifest.Sections))
	for i, entry := range manifest.Sections {
		if !validKey(entry.Key) {
			return fmt.Errorf("manifest section %q has an invalid key", entry.Key)
		}
		if _, ok := seen[entry.Key]; ok {
			return fmt.Errorf("manifest section %q is duplicated", entry.Key)
		}
		seen[entry.Key] = struct{}{}
		if len(entry.Globs) == 0 {
			return fmt.Errorf("manifest section %q has no canonical globs", entry.Key)
		}
		if _, err := canonicalGlobs(entry.Globs); err != nil {
			return fmt.Errorf("manifest section %q has invalid globs: %w", entry.Key, err)
		}
		if i < len(requiredManifestPrefix) && entry.Key != requiredManifestPrefix[i] {
			return fmt.Errorf("manifest section %q is out of order", entry.Key)
		}
	}

	if len(manifest.Sections) <= len(requiredManifestPrefix) {
		return fmt.Errorf("manifest requires at least one task section")
	}
	taskCount := 0
	lastTask := -1
	verificationIndex := -1
	for i := len(requiredManifestPrefix); i < len(manifest.Sections); i++ {
		key := manifest.Sections[i].Key
		if key == "verification" {
			verificationIndex = i
			continue
		}
		if strings.HasPrefix(key, "task-") {
			n, ok := taskNumber(key)
			if !ok || (lastTask >= 0 && n <= lastTask) {
				return fmt.Errorf("manifest task section %q is invalid or out of order", key)
			}
			lastTask = n
			taskCount++
		}
	}
	if taskCount == 0 {
		return fmt.Errorf("manifest requires at least one task section")
	}
	if verificationIndex != len(manifest.Sections)-1 {
		return fmt.Errorf("manifest verification section must be last")
	}
	return nil
}

var requiredManifestPrefix = []string{
	"goal",
	"architecture",
	"technology-stack",
	"execution-contract",
	"file-structure",
}

func validKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func taskNumber(key string) (int, bool) {
	if len(key) != len("task-0000") || !strings.HasPrefix(key, "task-") {
		return 0, false
	}
	n := 0
	for _, r := range key[len("task-"):] {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

func canonicalGlobs(globs []string) ([]string, error) {
	return touchset.CanonicalGlobs(globs)
}

func canonicalGlob(glob string) (string, error) {
	return touchset.CanonicalGlob(glob)
}
