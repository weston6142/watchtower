package recoverymatrix

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// BuildCompletionReceipt accepts only two complete deterministic runs and a
// successful repository verification bound to the same revision.
func BuildCompletionReceipt(first, second RunSummary, verification RepositoryVerification) (CompletionReceipt, error) {
	if strings.TrimSpace(first.Revision) == "" || first.Revision != second.Revision {
		return CompletionReceipt{}, fmt.Errorf("matrix revision is empty or differs between runs")
	}
	if strings.TrimSpace(first.ManifestIdentity) == "" || first.ManifestIdentity != second.ManifestIdentity {
		return CompletionReceipt{}, fmt.Errorf("matrix manifest identity is empty or differs between runs")
	}
	if err := validateCompleteRun("first", first); err != nil {
		return CompletionReceipt{}, err
	}
	if err := validateCompleteRun("second", second); err != nil {
		return CompletionReceipt{}, err
	}
	if err := compareDeterministicRuns(first, second); err != nil {
		return CompletionReceipt{}, err
	}
	if !verification.Passed {
		return CompletionReceipt{}, fmt.Errorf("repository verification did not pass")
	}
	if verification.Revision != first.Revision {
		return CompletionReceipt{}, fmt.Errorf("repository revision does not match matrix revision")
	}

	return CompletionReceipt{
		ManifestIdentity:   first.ManifestIdentity,
		ProductionRevision: first.Revision,
		Runs: [2]ReceiptRun{
			receiptRun(first),
			receiptRun(second),
		},
		MatrixResult:                 "passed",
		DeterminismResult:            "passed",
		RepositoryVerificationResult: "passed",
	}, nil
}

func validateCompleteRun(label string, summary RunSummary) error {
	if summary.Filtered {
		return fmt.Errorf("%s matrix run is filtered", label)
	}
	if summary.Partial {
		return fmt.Errorf("%s matrix run is partial", label)
	}
	if summary.Compiled <= 0 || summary.Executed != summary.Compiled || len(summary.Results) != summary.Compiled || summary.Passed+summary.Failed+summary.Skipped != summary.Executed {
		return fmt.Errorf("%s matrix run count is incomplete", label)
	}
	if summary.Skipped != 0 {
		return fmt.Errorf("%s matrix run has skipped scenarios", label)
	}
	if summary.Failed != 0 {
		return fmt.Errorf("%s matrix run has failed scenarios", label)
	}
	if summary.Passed != summary.Compiled {
		return fmt.Errorf("%s matrix run pass count is incomplete", label)
	}
	if summary.Panics != 0 {
		return fmt.Errorf("%s matrix run recorded a panic", label)
	}
	if summary.Timeouts != 0 {
		return fmt.Errorf("%s matrix run recorded a timeout", label)
	}
	if summary.UnexpectedCalls != 0 {
		return fmt.Errorf("%s matrix run recorded an unexpected adapter call", label)
	}
	if summary.UnconsumedScripts != 0 {
		return fmt.Errorf("%s matrix run has unconsumed scripts", label)
	}
	if summary.MissingResults != 0 {
		return fmt.Errorf("%s matrix run has missing results", label)
	}
	for _, result := range summary.Results {
		if result.Status != ResultPassed {
			return fmt.Errorf("%s matrix run contains non-passing result %q", label, result.ScenarioID)
		}
	}
	return nil
}

func compareDeterministicRuns(first, second RunSummary) error {
	if len(first.Results) != len(second.Results) {
		return fmt.Errorf("matrix inventory count differs between runs")
	}
	for index := range first.Results {
		left := first.Results[index]
		right := second.Results[index]
		if left.ScenarioID != right.ScenarioID {
			return fmt.Errorf("matrix inventory differs at result %d", index)
		}
		if !equalNormalizedObservation(left.Observation, right.Observation) {
			return fmt.Errorf("normalized observation differs for scenario %q", left.ScenarioID)
		}
	}
	return nil
}

func equalNormalizedObservation(left, right Observation) bool {
	if left.PublicOutcome != right.PublicOutcome || left.DurableState != right.DurableState || left.NormalizedClassification != right.NormalizedClassification {
		return false
	}
	return slices.Equal(sortedCopy(left.ArtifactIdentities), sortedCopy(right.ArtifactIdentities)) &&
		slices.Equal(sortedCopy(left.Effects), sortedCopy(right.Effects))
}

func sortedCopy(values []string) []string {
	copyOfValues := append([]string(nil), values...)
	sort.Strings(copyOfValues)
	return copyOfValues
}

func receiptRun(summary RunSummary) ReceiptRun {
	return ReceiptRun{
		Compiled: summary.Compiled,
		Executed: summary.Executed,
		Passed:   summary.Passed,
		Failed:   summary.Failed,
		Skipped:  summary.Skipped,
	}
}
