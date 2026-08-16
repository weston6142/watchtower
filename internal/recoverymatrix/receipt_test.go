package recoverymatrix_test

import (
	"strings"
	"testing"

	"github.com/weston6142/watchtower/internal/recoverymatrix"
)

func TestCompletionReceiptFailsClosed(t *testing.T) {
	base := completeRunSummary()
	tests := []struct {
		name   string
		mutate func(*recoverymatrix.RunSummary, *recoverymatrix.RunSummary, *recoverymatrix.RepositoryVerification)
		want   string
	}{
		{"different revision", func(_, second *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) {
			second.Revision = "other"
		}, "revision"},
		{"different manifest", func(_, second *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) {
			second.ManifestIdentity = "sha256:other"
		}, "manifest"},
		{"filtered", func(first, _ *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) {
			first.Filtered = true
		}, "filtered"},
		{"partial", func(first, _ *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) {
			first.Partial = true
		}, "partial"},
		{"compiled mismatch", func(first, _ *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) { first.Compiled++ }, "count"},
		{"skip", func(first, _ *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) {
			first.Skipped = 1
			first.Passed--
		}, "skipped"},
		{"failure", func(first, _ *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) {
			first.Failed = 1
			first.Passed--
		}, "failed"},
		{"panic", func(first, _ *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) { first.Panics = 1 }, "panic"},
		{"timeout", func(first, _ *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) {
			first.Timeouts = 1
		}, "timeout"},
		{"unexpected call", func(first, _ *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) {
			first.UnexpectedCalls = 1
		}, "unexpected"},
		{"unconsumed script", func(first, _ *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) {
			first.UnconsumedScripts = 1
		}, "unconsumed"},
		{"missing result", func(first, _ *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) {
			first.MissingResults = 1
		}, "missing"},
		{"inventory differs", func(_, second *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) {
			second.Results[0].ScenarioID = "failure/spec/runner"
		}, "inventory"},
		{"observation differs", func(_, second *recoverymatrix.RunSummary, _ *recoverymatrix.RepositoryVerification) {
			second.Results[0].Observation.PublicOutcome = "blocked"
		}, "observation"},
		{"repository failed", func(_, _ *recoverymatrix.RunSummary, verification *recoverymatrix.RepositoryVerification) {
			verification.Passed = false
		}, "repository verification"},
		{"repository revision differs", func(_, _ *recoverymatrix.RunSummary, verification *recoverymatrix.RepositoryVerification) {
			verification.Revision = "other"
		}, "repository revision"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first := cloneRunSummary(base)
			second := cloneRunSummary(base)
			verification := recoverymatrix.RepositoryVerification{Revision: base.Revision, Passed: true}
			test.mutate(&first, &second, &verification)
			if _, err := recoverymatrix.BuildCompletionReceipt(first, second, verification); err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("receipt error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestCompletionReceiptAcceptsTwoIdenticalRuns(t *testing.T) {
	first := completeRunSummary()
	second := cloneRunSummary(first)
	second.Results[0].Observation.DiagnosticCheckpoints = []string{"different", "diagnostics"}
	receipt, err := recoverymatrix.BuildCompletionReceipt(first, second, recoverymatrix.RepositoryVerification{Revision: first.Revision, Passed: true})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ManifestIdentity != first.ManifestIdentity || receipt.ProductionRevision != first.Revision {
		t.Fatalf("receipt identity = %#v", receipt)
	}
	if receipt.MatrixResult != "passed" || receipt.DeterminismResult != "passed" || receipt.RepositoryVerificationResult != "passed" {
		t.Fatalf("receipt results = %#v", receipt)
	}
	if receipt.Runs[0].Executed != first.Executed || receipt.Runs[1].Passed != second.Passed {
		t.Fatalf("receipt counts = %#v", receipt.Runs)
	}
}

func completeRunSummary() recoverymatrix.RunSummary {
	observation := recoverymatrix.Observation{
		PublicOutcome:            "complete",
		DurableState:             "merged",
		NormalizedClassification: "none",
		ArtifactIdentities:       []string{"sha256:artifact"},
		Effects:                  []string{"land:issue/GH-69"},
		DiagnosticCheckpoints:    []string{"runner_succeeded"},
	}
	return recoverymatrix.RunSummary{
		ManifestIdentity: "sha256:manifest",
		Revision:         "abcdef",
		Compiled:         1,
		Executed:         1,
		Passed:           1,
		Results: []recoverymatrix.ScenarioResult{{
			ScenarioID:  "failure/brainstorm/runner",
			Status:      recoverymatrix.ResultPassed,
			Observation: observation,
		}},
	}
}

func cloneRunSummary(summary recoverymatrix.RunSummary) recoverymatrix.RunSummary {
	clone := summary
	clone.Results = append([]recoverymatrix.ScenarioResult(nil), summary.Results...)
	for index := range clone.Results {
		clone.Results[index].Observation.ArtifactIdentities = append([]string(nil), summary.Results[index].Observation.ArtifactIdentities...)
		clone.Results[index].Observation.Effects = append([]string(nil), summary.Results[index].Observation.Effects...)
		clone.Results[index].Observation.DiagnosticCheckpoints = append([]string(nil), summary.Results[index].Observation.DiagnosticCheckpoints...)
	}
	return clone
}
