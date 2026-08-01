package contextpack

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weston6142/watchtower/internal/levers"
)

func TestStageBriefIncludesFinalizationContract(t *testing.T) {
	dir := t.TempDir()
	contract := "## Finalization artifact contract\n\nstrict receipts\n"
	if err := WriteStageBrief(dir, Brief{
		IssueID: "GH-1", Stage: "merge-verification", FinalizationContract: contract,
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "STAGE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), contract) {
		t.Fatalf("stage brief missing contract:\n%s", body)
	}
}

func TestArchiveAndMaterializeDeclaredArtifact(t *testing.T) {
	source := t.TempDir()
	issueDir := t.TempDir()
	next := t.TempDir()
	content := []byte("# Approved brainstorm\n")
	if err := os.WriteFile(filepath.Join(source, "brainstorm.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "private-notes.md"), []byte("do not share"), 0o644); err != nil {
		t.Fatal(err)
	}

	artifacts, err := Archive(source, issueDir, []string{"brainstorm.md"})
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := fmt.Sprintf("%x", sha256.Sum256(content))
	if len(artifacts) != 1 || artifacts[0].Name != "brainstorm.md" ||
		artifacts[0].SHA256 != wantDigest {
		t.Fatalf("artifacts: %+v", artifacts)
	}
	durable := filepath.Join(issueDir, "artifacts", "brainstorm.md")
	if got, err := os.ReadFile(durable); err != nil || string(got) != string(content) {
		t.Fatalf("durable artifact: %q err=%v", got, err)
	}
	if err := Materialize(issueDir, next, []string{"brainstorm.md"}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(next, "brainstorm.md")); err != nil ||
		string(got) != string(content) {
		t.Fatalf("materialized artifact: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(next, "private-notes.md")); !os.IsNotExist(err) {
		t.Fatalf("undeclared file materialized: %v", err)
	}
}

func TestArchiveRejectsNonRegularAndEscapingArtifacts(t *testing.T) {
	source := t.TempDir()
	issueDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dir", "../outside.md", "/tmp/outside.md"} {
		if _, err := Archive(source, issueDir, []string{name}); err == nil {
			t.Fatalf("Archive accepted %q", name)
		}
	}
}

func TestDecisionLedgerIncludesOnlyResolvedDecisions(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	choice := 1
	ledger := DecisionLedger([]Decision{
		{Stage: "brainstorm", Question: "Which shape?", Kind: levers.DecisionChoice,
			Options: []string{"A", "B"}, Response: levers.ChoiceResponse(choice),
			Why: "B is safer", Consequences: []string{"fast", "safe"}, Status: "answered", At: now},
		{Stage: "spec", Question: "Clarify scope", Kind: levers.DecisionFreeform,
			Response: levers.FreeformResponse("Only the API."), Why: "Keeps scope bounded",
			Consequences: []string{"UI later"}, Status: "auto", At: now.Add(time.Minute)},
		{Stage: "plan", Question: "Still waiting?", Status: "pending", At: now},
		{Stage: "execute", Question: "Orphaned?", Status: "orphaned", At: now},
	})
	for _, want := range []string{
		"Which shape?", "B", "B is safer", "Clarify scope", "Only the API.", "UI later",
		"brainstorm", "2026-07-30T12:00:00Z",
	} {
		if !strings.Contains(ledger, want) {
			t.Fatalf("ledger missing %q:\n%s", want, ledger)
		}
	}
	for _, unwanted := range []string{"Still waiting?", "Orphaned?"} {
		if strings.Contains(ledger, unwanted) {
			t.Fatalf("ledger contains %q:\n%s", unwanted, ledger)
		}
	}
}

func TestStageBriefContainsRecoveryFactsWithoutTranscript(t *testing.T) {
	dir := t.TempDir()
	err := WriteStageBrief(dir, Brief{
		IssueID: "GH-4", Stage: "execute", StartCommit: "abc123", BaseCommit: "base123",
		Branch: "watchtower/GH-4", RequiredInputs: []string{"spec.md", "plan.md"},
		ExpectedOutputs: []string{"committed implementation"}, ProhibitedActions: []string{"merge"},
		VerificationOwner: "merge-verification",
		Recovery: &Recovery{LastSuccessfulStage: "plan", CurrentHead: "def456",
			Dirty: true, LastFailure: "go test failed", OutstandingOutputs: []string{"tests"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "STAGE.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"GH-4", "execute", "abc123", "base123", "watchtower/GH-4",
		"spec.md", "plan.md", "merge-verification", "go test failed", "dirty: yes",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("brief missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(strings.ToLower(text), "transcript") {
		t.Fatalf("brief mentions transcript:\n%s", text)
	}
}
