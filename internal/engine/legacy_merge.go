package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/store"
)

type legacyMergeKind uint8

const (
	legacyMergeLanded legacyMergeKind = iota + 1
	legacyMergeBranchOnly
)

type legacyMergeEvidence struct {
	IssueID     string
	BaseBranch  string
	IssueBranch string
	LandedSHA   string
	Kind        legacyMergeKind
}

type legacyEvidenceError struct {
	IssueID string
	Reason  string
}

func (e *legacyEvidenceError) Error() string {
	return fmt.Sprintf("legacy merge evidence for %s rejected: %s", e.IssueID, e.Reason)
}

func foldLegacyMergeEvidence(
	issue store.IssueRow, events []core.Event, baseBranch string,
) (legacyMergeEvidence, error) {
	reject := func(reason string) (legacyMergeEvidence, error) {
		return legacyMergeEvidence{}, &legacyEvidenceError{IssueID: issue.ID, Reason: reason}
	}
	if issue.ID == "" {
		return reject("issue ID is empty")
	}
	if issue.State != "done" && issue.State != "merged" {
		return reject(fmt.Sprintf("issue state %q is not a completed merge", issue.State))
	}
	if baseBranch == "" {
		return reject("base branch is empty")
	}

	relevant := make([]core.Event, 0, len(events))
	for _, event := range events {
		if event.IssueID != issue.ID {
			continue
		}
		switch event.Type {
		case core.EvIssueMerged, core.EvIssueCompleted, core.EvPublishSucceeded:
			relevant = append(relevant, event)
		}
	}
	sort.SliceStable(relevant, func(i, j int) bool {
		if relevant[i].Seq == relevant[j].Seq {
			return relevant[i].ID < relevant[j].ID
		}
		return relevant[i].Seq < relevant[j].Seq
	})

	var (
		mergedSeq, completedSeq int64
		hasMerged, hasCompleted bool
		mergedBranch            string
		mergedFields            map[string]json.RawMessage
		landedSHA               string
	)
	for _, event := range relevant {
		fields, err := legacyEventFields(event.Payload)
		if err != nil {
			return reject(fmt.Sprintf("%s payload is malformed: %v", event.Type, err))
		}

		if event.Type == core.EvIssueMerged {
			if hasMerged {
				return reject("duplicate issue_merged evidence")
			}
			hasMerged = true
			mergedSeq = event.Seq
			mergedFields = fields
			recordedBranch, present, err := legacyStringField(fields, "branch")
			if err != nil {
				return reject(fmt.Sprintf("issue_merged branch evidence is malformed: %v", err))
			}
			if present {
				mergedBranch = recordedBranch
			}
		}
		if event.Type == core.EvIssueCompleted {
			if hasCompleted {
				return reject("duplicate issue_completed evidence")
			}
			hasCompleted = true
			completedSeq = event.Seq
		}

		commit, present, err := legacyStringField(fields, "commit")
		if err != nil {
			return reject(fmt.Sprintf("%s commit evidence is malformed: %v", event.Type, err))
		}
		if present {
			var evidenceErr error
			landedSHA, evidenceErr = mergeLegacyStringEvidence(landedSHA, commit)
			if evidenceErr != nil {
				return reject(evidenceErr.Error())
			}
		}
		landed, present, err := legacyStringField(fields, "landed_sha")
		if err != nil {
			return reject(fmt.Sprintf("%s landed_sha evidence is malformed: %v", event.Type, err))
		}
		if present {
			var evidenceErr error
			landedSHA, evidenceErr = mergeLegacyStringEvidence(landedSHA, landed)
			if evidenceErr != nil {
				return reject(evidenceErr.Error())
			}
		}

		baseBranchEvidence, present, err := legacyStringField(fields, "base_branch")
		if err != nil {
			return reject(fmt.Sprintf("%s base_branch evidence is malformed: %v", event.Type, err))
		}
		if present && baseBranchEvidence != baseBranch {
			return reject(fmt.Sprintf("conflicting base branches %q and %q", baseBranchEvidence, baseBranch))
		}
		issueBranchEvidence, present, err := legacyStringField(fields, "branch")
		if err != nil {
			return reject(fmt.Sprintf("%s branch evidence is malformed: %v", event.Type, err))
		}
		if present && issueBranchEvidence != "issue/"+issue.ID && issueBranchEvidence != baseBranch {
			return reject(fmt.Sprintf("conflicting branch evidence %q and %q", issueBranchEvidence, baseBranch))
		}

		if event.Type == core.EvIssueCompleted {
			mergeOutcome, present, err := legacyStringField(fields, "merge")
			if err != nil {
				return reject(fmt.Sprintf("completion outcome is malformed: %v", err))
			}
			if present && mergeOutcome == "left-unmerged" {
				return reject("completion is left-unmerged")
			}
			if present && mergeOutcome == "none" {
				return reject("completion explicitly reports no merge")
			}
		}
	}

	if !hasMerged {
		return reject("missing issue_merged evidence")
	}
	if !hasCompleted {
		return reject("missing issue_completed evidence")
	}
	if mergedSeq >= completedSeq {
		return reject("issue_merged evidence does not precede issue_completed evidence")
	}
	if landedSHA != "" {
		return legacyMergeEvidence{
			IssueID: issue.ID, BaseBranch: baseBranch,
			LandedSHA: landedSHA, Kind: legacyMergeLanded,
		}, nil
	}
	if mergedBranch != "issue/"+issue.ID {
		return reject("branch-only merge evidence does not identify the exact issue branch")
	}
	if len(mergedFields) != 1 {
		return reject("issue_merged branch-only payload contains extra merge evidence")
	}
	return legacyMergeEvidence{
		IssueID: issue.ID, BaseBranch: baseBranch,
		IssueBranch: mergedBranch, Kind: legacyMergeBranchOnly,
	}, nil
}

func legacyEventFields(payload json.RawMessage) (map[string]json.RawMessage, error) {
	if len(payload) == 0 || string(payload) == "null" {
		return map[string]json.RawMessage{}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return map[string]json.RawMessage{}, nil
	}
	return fields, nil
}

func legacyStringField(fields map[string]json.RawMessage, name string) (string, bool, error) {
	raw, ok := fields[name]
	if !ok {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, err
	}
	if strings.TrimSpace(value) == "" {
		return "", true, fmt.Errorf("%s is empty", name)
	}
	return value, true, nil
}

func mergeLegacyStringEvidence(existing, next string) (string, error) {
	if existing == "" {
		return next, nil
	}
	if existing != next {
		return "", fmt.Errorf("conflicting landed commits %q and %q", existing, next)
	}
	return existing, nil
}
