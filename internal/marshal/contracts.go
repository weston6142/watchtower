package marshal

import (
	"encoding/json"
	"fmt"
	"os"
)

// MergeDecision is the machine-readable integration recommendation emitted by
// the final verifier. Human evidence belongs in merge-report.md.
type MergeDecision struct {
	Decision     string `json:"decision"`
	BranchCommit string `json:"branch_commit,omitempty"`
	BaseCommit   string `json:"base_commit,omitempty"`
}

func LoadMergeDecision(path string) (MergeDecision, error) {
	file, err := os.Open(path)
	if err != nil {
		return MergeDecision{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var decision MergeDecision
	if err := decoder.Decode(&decision); err != nil {
		return MergeDecision{}, fmt.Errorf("decode merge decision: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return MergeDecision{}, fmt.Errorf("decode merge decision: %w", err)
	}
	switch decision.Decision {
	case "hold":
		return decision, nil
	case "merge":
		if decision.BranchCommit == "" || decision.BaseCommit == "" {
			return MergeDecision{}, fmt.Errorf(
				"merge decision requires branch_commit and base_commit")
		}
		return decision, nil
	default:
		return MergeDecision{}, fmt.Errorf(
			"invalid merge decision %q: want merge or hold", decision.Decision)
	}
}

func MergeDecisionExample() string {
	return "{\n  \"decision\": \"merge\",\n" +
		"  \"branch_commit\": \"1111111111111111111111111111111111111111\",\n" +
		"  \"base_commit\": \"0000000000000000000000000000000000000000\"\n}\n"
}

func VerificationExample() string {
	return "{\n  \"base_sha\": \"0000000000000000000000000000000000000000\",\n" +
		"  \"branch_sha\": \"1111111111111111111111111111111111111111\",\n" +
		"  \"tree_sha\": \"2222222222222222222222222222222222222222\",\n" +
		"  \"passed\": true,\n" +
		"  \"commands\": [[\"go\", \"test\", \"./...\"]]\n}\n"
}

func FinalizationContractMarkdown() string {
	return "## Finalization artifact contract\n\n" +
		"Write rich evidence only to `merge-report.md`. The two JSON receipts " +
		"must contain exactly the fields shown; unknown fields are rejected.\n\n" +
		"### merge-decision.json\n\n```json\n" + MergeDecisionExample() +
		"```\n\n### verification.json\n\n```json\n" + VerificationExample() + "```\n"
}
