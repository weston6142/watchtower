package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/store"
)

type legacyMergeEvidence struct {
	IssueID    string
	BaseBranch string
	LandedSHA  string
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
		landedSHA               string
		completionOutcome       string
	)
	for _, event := range relevant {
		fields, err := legacyEventFields(event.Payload)
		if err != nil {
			return reject(fmt.Sprintf("%s payload is malformed: %v", event.Type, err))
		}

		if event.Type == core.EvIssueMerged && !hasMerged {
			hasMerged = true
			mergedSeq = event.Seq
		}
		if event.Type == core.EvIssueCompleted && !hasCompleted {
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

		branch, present, err := legacyStringField(fields, "base_branch")
		if err != nil {
			return reject(fmt.Sprintf("%s base_branch evidence is malformed: %v", event.Type, err))
		}
		if present && branch != baseBranch {
			return reject(fmt.Sprintf("conflicting base branches %q and %q", branch, baseBranch))
		}
		branch, present, err = legacyStringField(fields, "branch")
		if err != nil {
			return reject(fmt.Sprintf("%s branch evidence is malformed: %v", event.Type, err))
		}
		if present && branch != "issue/"+issue.ID && branch != baseBranch {
			return reject(fmt.Sprintf("conflicting branch evidence %q and %q", branch, baseBranch))
		}

		if event.Type == core.EvIssueCompleted {
			merge, present, err := legacyStringField(fields, "merge")
			if err != nil {
				return reject(fmt.Sprintf("completion outcome is malformed: %v", err))
			}
			if present {
				if merge == "left-unmerged" {
					return reject("completion is left-unmerged")
				}
				if completionOutcome != "" && completionOutcome != merge {
					return reject(fmt.Sprintf("conflicting completion outcomes %q and %q", completionOutcome, merge))
				}
				completionOutcome = merge
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
	if landedSHA == "" {
		return reject("missing landed commit evidence")
	}
	return legacyMergeEvidence{IssueID: issue.ID, BaseBranch: baseBranch, LandedSHA: landedSHA}, nil
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
