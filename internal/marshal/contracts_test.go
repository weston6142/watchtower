package marshal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalFinalizationExamplesPassProductionLoaders(t *testing.T) {
	dir := t.TempDir()
	decisionPath := filepath.Join(dir, "merge-decision.json")
	verificationPath := filepath.Join(dir, "verification.json")
	if err := os.WriteFile(decisionPath, []byte(MergeDecisionExample()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(verificationPath, []byte(VerificationExample()), 0o644); err != nil {
		t.Fatal(err)
	}
	decision, err := LoadMergeDecision(decisionPath)
	if err != nil || decision.Decision != "merge" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	verification, err := LoadVerification(verificationPath)
	if err != nil || !verification.Passed || len(verification.Commands) != 1 {
		t.Fatalf("verification=%+v err=%v", verification, err)
	}
}

func TestMergeDecisionRejectsUnknownFieldsAndMissingMergeIdentity(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":  `{"decision":"merge","branch_commit":"b","base_commit":"a","rationale":"rich"}`,
		"missing branch": `{"decision":"merge","base_commit":"a"}`,
		"missing base":   `{"decision":"merge","branch_commit":"b"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "merge-decision.json")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadMergeDecision(path); err == nil {
				t.Fatal("invalid merge decision accepted")
			}
		})
	}
}
