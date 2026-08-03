package review

import (
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/contextpack"
)

// Target identifies the exact archived artifact set that an operator is being
// asked to review.
type Target struct {
	IssueID         string                 `json:"issue_id"`
	Stage           string                 `json:"stage"`
	CheckpointID    int64                  `json:"checkpoint_id"`
	Artifacts       []contextpack.Artifact `json:"artifacts"`
	ArtifactVersion string                 `json:"artifact_version"`
	NextStage       string                 `json:"next_stage"`
}

type Outcome string

const (
	OutcomeAccepted Outcome = "accepted"
	OutcomeRevise   Outcome = "revise"
	OutcomeStale    Outcome = "stale"
)

var ErrStaleTarget = errors.New("artifact review target is stale")

// Canonical validates and normalizes a review target, including the complete
// artifact digest set used to version the review.
func (t Target) Canonical() (Target, error) {
	if strings.TrimSpace(t.IssueID) == "" {
		return Target{}, fmt.Errorf("review target issue is empty")
	}
	if strings.TrimSpace(t.Stage) == "" {
		return Target{}, fmt.Errorf("review target stage is empty")
	}
	if t.CheckpointID <= 0 {
		return Target{}, fmt.Errorf("review target checkpoint must be positive")
	}
	canonical := t
	canonical.Artifacts = append([]contextpack.Artifact(nil), t.Artifacts...)
	seen := make(map[string]struct{}, len(canonical.Artifacts))
	for i, artifact := range canonical.Artifacts {
		name := filepath.ToSlash(filepath.Clean(filepath.FromSlash(artifact.Name)))
		if artifact.Name == "" || filepath.IsAbs(filepath.FromSlash(artifact.Name)) ||
			name == "." || name == ".." || strings.HasPrefix(name, "../") || name != artifact.Name {
			return Target{}, fmt.Errorf("unsafe review artifact name %q", artifact.Name)
		}
		if _, ok := seen[name]; ok {
			return Target{}, fmt.Errorf("duplicate review artifact %q", name)
		}
		seen[name] = struct{}{}
		digest := strings.ToLower(artifact.SHA256)
		if len(digest) != 64 {
			return Target{}, fmt.Errorf("review artifact %q has invalid sha256", name)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return Target{}, fmt.Errorf("review artifact %q has invalid sha256: %w", name, err)
		}
		canonical.Artifacts[i] = contextpack.Artifact{Name: name, SHA256: digest}
	}
	sort.Slice(canonical.Artifacts, func(i, j int) bool {
		return canonical.Artifacts[i].Name < canonical.Artifacts[j].Name
	})
	version := canonical.VersionKey()
	if t.ArtifactVersion != "" && t.ArtifactVersion != version {
		return Target{}, fmt.Errorf("review artifact version does not match artifact set")
	}
	canonical.ArtifactVersion = version
	return canonical, nil
}

// VersionKey is stable for a checkpoint and its complete sorted artifact set.
func (t Target) VersionKey() string {
	artifacts := append([]contextpack.Artifact(nil), t.Artifacts...)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	var version strings.Builder
	fmt.Fprintf(&version, "%d", t.CheckpointID)
	for _, artifact := range artifacts {
		version.WriteByte('|')
		version.WriteString(artifact.Name)
		version.WriteByte('=')
		version.WriteString(strings.ToLower(artifact.SHA256))
	}
	return version.String()
}

// Matches compares canonical target identity, including every artifact
// digest, so an answer cannot authorize a different archived version.
func (t Target) Matches(other Target) bool {
	left, err := t.Canonical()
	if err != nil {
		return false
	}
	right, err := other.Canonical()
	if err != nil {
		return false
	}
	if left.IssueID != right.IssueID || left.Stage != right.Stage ||
		left.CheckpointID != right.CheckpointID || left.ArtifactVersion != right.ArtifactVersion ||
		left.NextStage != right.NextStage || len(left.Artifacts) != len(right.Artifacts) {
		return false
	}
	for i := range left.Artifacts {
		if left.Artifacts[i] != right.Artifacts[i] {
			return false
		}
	}
	return true
}
