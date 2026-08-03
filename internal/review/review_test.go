package review

import (
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/contextpack"
)

func TestCanonicalTargetSortsArtifactsAndBindsCompleteVersion(t *testing.T) {
	target := Target{
		IssueID:      "GH-26",
		Stage:        "plan",
		CheckpointID: 7,
		Artifacts: []contextpack.Artifact{
			{Name: "touchset.json", SHA256: strings.Repeat("b", 64)},
			{Name: "plan.md", SHA256: strings.Repeat("a", 64)},
		},
		NextStage: "execute",
	}

	got, err := target.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if got.Artifacts[0].Name != "plan.md" || got.Artifacts[1].Name != "touchset.json" {
		t.Fatalf("artifacts were not sorted: %+v", got.Artifacts)
	}
	if got.ArtifactVersion == "" || got.ArtifactVersion != got.VersionKey() {
		t.Fatalf("artifact version = %q, version key = %q", got.ArtifactVersion, got.VersionKey())
	}
	changed := got
	changed.Artifacts = append([]contextpack.Artifact(nil), got.Artifacts...)
	changed.Artifacts[1].SHA256 = strings.Repeat("c", 64)
	if changed.VersionKey() == got.VersionKey() {
		t.Fatal("changing one artifact digest did not change the version")
	}
}

func TestCanonicalTargetRejectsUnsafeDuplicateAndForgedArtifacts(t *testing.T) {
	validDigest := strings.Repeat("a", 64)
	tests := []struct {
		name     string
		artifact []contextpack.Artifact
	}{
		{name: "empty name", artifact: []contextpack.Artifact{{Name: "", SHA256: validDigest}}},
		{name: "path traversal", artifact: []contextpack.Artifact{{Name: "../plan.md", SHA256: validDigest}}},
		{name: "duplicate name", artifact: []contextpack.Artifact{
			{Name: "plan.md", SHA256: validDigest},
			{Name: "plan.md", SHA256: validDigest},
		}},
		{name: "bad digest", artifact: []contextpack.Artifact{{Name: "plan.md", SHA256: "digest"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := (Target{
				IssueID: "GH-26", Stage: "plan", CheckpointID: 7,
				Artifacts: test.artifact, NextStage: "execute",
			}).Canonical()
			if err == nil {
				t.Fatal("Canonical() accepted invalid artifact target")
			}
		})
	}

	target := Target{
		IssueID: "GH-26", Stage: "plan", CheckpointID: 7,
		Artifacts: []contextpack.Artifact{{Name: "plan.md", SHA256: validDigest}},
		NextStage: "execute", ArtifactVersion: "forged",
	}
	if _, err := target.Canonical(); err == nil {
		t.Fatal("Canonical() accepted a forged artifact version")
	}
}
