package stageresult_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/weston6142/watchtower/internal/stageresult"
)

func TestValidateAcceptsEachStagePayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		kind     stageresult.Kind
		evidence stageresult.Evidence
	}{
		{name: "execute", kind: stageresult.KindExecute, evidence: validExecuteEvidence()},
		{name: "correctness review", kind: stageresult.KindCorrectnessReview, evidence: validCorrectnessEvidence()},
		{name: "clean code review", kind: stageresult.KindCleanCodeReview, evidence: validCleanCodeEvidence()},
		{name: "librarian", kind: stageresult.KindLibrarian, evidence: validLibrarianEvidence()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := cloneEvidence(t, tt.evidence)
			candidate, err := stageresult.Build(stageresult.BuildInput{
				IssueID:              "GH-67",
				AttemptID:            "checkpoint-7",
				PredecessorAttemptID: "checkpoint-6",
				ExpectedKind:         tt.kind,
				Evidence:             tt.evidence,
			})
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			got, err := stageresult.Validate(candidate)
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}

			if got.SchemaVersion != stageresult.SchemaVersion || got.IssueID != "GH-67" || got.AttemptID != "checkpoint-7" {
				t.Fatalf("trusted envelope = %#v", got)
			}
			if got.PredecessorAttemptID != "checkpoint-6" || got.StageKind != tt.kind || got.ValidationStatus != stageresult.ValidationValid {
				t.Fatalf("validated envelope = %#v", got)
			}
			if payloadCount(got.Evidence) != 1 {
				t.Fatalf("payload count = %d, want 1", payloadCount(got.Evidence))
			}
			if !reflect.DeepEqual(tt.evidence, before) {
				t.Fatalf("Build or Validate mutated evidence\n got: %#v\nwant: %#v", tt.evidence, before)
			}
		})
	}
}

func TestValidateRejectsInvalidResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*stageresult.Result)
	}{
		{name: "unsupported_version", mutate: func(r *stageresult.Result) { r.SchemaVersion = 2 }},
		{name: "wrong_payload", mutate: func(r *stageresult.Result) {
			r.Execute = nil
			r.CorrectnessReview = validCorrectnessEvidence().CorrectnessReview
		}},
		{name: "multiple_payloads", mutate: func(r *stageresult.Result) {
			r.CorrectnessReview = validCorrectnessEvidence().CorrectnessReview
		}},
		{name: "retryable_without_context", mutate: func(r *stageresult.Result) { r.Outcome = stageresult.OutcomeRetryable }},
		{name: "completed_with_remaining_work", mutate: func(r *stageresult.Result) {
			r.RemainingWork = []stageresult.WorkItem{{Kind: stageresult.WorkPlanTask, Description: "finish task-0002"}}
		}},
		{name: "completed_with_remaining_task", mutate: func(r *stageresult.Result) {
			r.Execute.PlanTasks[0].Outcome = stageresult.TaskRemaining
		}},
		{name: "retryable_remaining_task_without_plan_work", mutate: func(r *stageresult.Result) {
			r.Outcome = stageresult.OutcomeRetryable
			r.Execute.PlanTasks[0].Outcome = stageresult.TaskRemaining
			r.RemainingConcerns = []stageresult.Concern{{Explanation: "the task still needs attention"}}
		}},
		{name: "completed_with_open_finding", mutate: func(r *stageresult.Result) {
			r.Execute = nil
			r.StageKind = stageresult.KindCorrectnessReview
			r.CorrectnessReview = validCorrectnessEvidence().CorrectnessReview
			r.CorrectnessReview.Findings[0].Status = stageresult.FindingOpen
		}},
		{name: "fixed_finding_without_fix", mutate: func(r *stageresult.Result) {
			r.Execute = nil
			r.StageKind = stageresult.KindCorrectnessReview
			r.CorrectnessReview = validCorrectnessEvidence().CorrectnessReview
			r.CorrectnessReview.Fixes = nil
			r.CorrectnessReview.NoChange = &stageresult.NoChangeConclusion{Explanation: "no change was made"}
		}},
		{name: "skip_without_explanation", mutate: func(r *stageresult.Result) {
			r.Execute.Skips = []stageresult.Skip{{Activity: "checks"}}
		}},
		{name: "execute_without_task_outcomes", mutate: func(r *stageresult.Result) { r.Execute.PlanTasks = nil }},
		{name: "execute_without_commit_or_skip", mutate: func(r *stageresult.Result) { r.Execute.Commits = nil }},
		{name: "execute_without_checks_or_skip", mutate: func(r *stageresult.Result) { r.Execute.Checks = nil }},
		{name: "review_no_change_with_fixes", mutate: func(r *stageresult.Result) {
			r.Execute = nil
			r.StageKind = stageresult.KindCorrectnessReview
			r.CorrectnessReview = validCorrectnessEvidence().CorrectnessReview
			r.CorrectnessReview.NoChange = &stageresult.NoChangeConclusion{Explanation: "nothing else changed"}
		}},
		{name: "review_without_change_or_no_change", mutate: func(r *stageresult.Result) {
			r.Execute = nil
			r.StageKind = stageresult.KindCleanCodeReview
			r.CleanCodeReview = validCleanCodeEvidence().CleanCodeReview
			r.CleanCodeReview.NoChange = nil
		}},
		{name: "review_without_checks_or_skip", mutate: func(r *stageresult.Result) {
			r.Execute = nil
			r.StageKind = stageresult.KindCleanCodeReview
			r.CleanCodeReview = validCleanCodeEvidence().CleanCodeReview
			r.CleanCodeReview.Checks = nil
		}},
		{name: "review_without_reviewed_paths_or_skip", mutate: func(r *stageresult.Result) {
			r.Execute = nil
			r.StageKind = stageresult.KindCleanCodeReview
			r.CleanCodeReview = validCleanCodeEvidence().CleanCodeReview
			r.CleanCodeReview.ReviewedPaths = nil
		}},
		{name: "librarian_no_change_with_updates", mutate: func(r *stageresult.Result) {
			r.Execute = nil
			r.StageKind = stageresult.KindLibrarian
			r.Librarian = validLibrarianEvidence().Librarian
			r.Librarian.NoChange = &stageresult.NoChangeConclusion{Explanation: "no documentation needed"}
		}},
		{name: "librarian_without_reviewed_paths_or_skip", mutate: func(r *stageresult.Result) {
			r.Execute = nil
			r.StageKind = stageresult.KindLibrarian
			r.Librarian = validLibrarianEvidence().Librarian
			r.Librarian.ReviewedPaths = nil
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			candidate := mustBuild(t, stageresult.KindExecute, validExecuteEvidence())
			tt.mutate(&candidate)

			got, err := stageresult.Validate(candidate)
			if err == nil {
				t.Fatal("Validate() error = nil")
			}
			var validationErr *stageresult.ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("error type = %T, want *ValidationError", err)
			}
			if validationErr.Field == "" {
				t.Fatalf("validation error has blank field: %v", err)
			}
			if got.ValidationStatus == stageresult.ValidationValid {
				t.Fatalf("rejected result marked valid: %#v", got)
			}
		})
	}
}

func TestProjectRetryReturnsOnlyActionableUnfinishedContext(t *testing.T) {
	t.Parallel()

	evidence := validCorrectnessEvidence()
	evidence.Outcome = stageresult.OutcomeRetryable
	evidence.RemainingWork = []stageresult.WorkItem{
		{Kind: stageresult.WorkFinding, Description: "resolve F-2", Paths: []string{"internal/engine/engine.go"}},
		{Kind: stageresult.WorkReviewPath, Description: "review lifecycle recovery", Paths: []string{"internal/engine/stage_lifecycle.go"}},
	}
	evidence.RemainingConcerns = []stageresult.Concern{{Explanation: "confirm persistence ordering under failure"}}
	evidence.CorrectnessReview.Findings = append(evidence.CorrectnessReview.Findings,
		stageresult.Finding{ID: "F-2", Summary: "retry path remains unreviewed", Status: stageresult.FindingOpen, Paths: []string{"internal/engine/stage_lifecycle.go"}},
	)
	evidence.CorrectnessReview.Skips = []stageresult.Skip{{Activity: "race check", Explanation: "owned by merge verification"}}

	result, err := stageresult.Validate(mustBuild(t, stageresult.KindCorrectnessReview, evidence))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	projection, err := stageresult.ProjectRetry(result)
	if err != nil {
		t.Fatalf("ProjectRetry() error = %v", err)
	}

	if projection.SourceAttemptID != "checkpoint-7" {
		t.Fatalf("source attempt = %q", projection.SourceAttemptID)
	}
	if len(projection.OpenFindings) != 1 || projection.OpenFindings[0].ID != "F-2" {
		t.Fatalf("open findings = %#v", projection.OpenFindings)
	}
	if len(projection.UnreviewedPaths) != 1 || projection.UnreviewedPaths[0].Description != "review lifecycle recovery" {
		t.Fatalf("unreviewed paths = %#v", projection.UnreviewedPaths)
	}
	if len(projection.SkippedActivities) != 1 || projection.SkippedActivities[0].Explanation != "owned by merge verification" {
		t.Fatalf("skipped activities = %#v", projection.SkippedActivities)
	}
	if len(projection.RemainingConcerns) != 1 {
		t.Fatalf("remaining concerns = %#v", projection.RemainingConcerns)
	}
	if len(result.CorrectnessReview.Fixes) != 1 || result.CorrectnessReview.ReviewedPaths[0] != "internal/stageresult/result.go" {
		t.Fatalf("durable completed evidence lost: %#v", result.CorrectnessReview)
	}
	if containsWork(projection.RemainingWork, "fix F-1") || containsWork(projection.UnreviewedPaths, "internal/stageresult/result.go") {
		t.Fatalf("projection repeated completed evidence: %#v", projection)
	}
}

func TestProjectRetrySeparatesMissingDocumentation(t *testing.T) {
	t.Parallel()

	evidence := validLibrarianEvidence()
	evidence.Outcome = stageresult.OutcomeRetryable
	evidence.RemainingWork = []stageresult.WorkItem{{
		Kind: stageresult.WorkDocumentation, Description: "document structured retry semantics", Paths: []string{"docs/guildhall/lane-ops-and-issue-states.md"},
	}}
	result, err := stageresult.Validate(mustBuild(t, stageresult.KindLibrarian, evidence))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	projection, err := stageresult.ProjectRetry(result)
	if err != nil {
		t.Fatalf("ProjectRetry() error = %v", err)
	}
	if len(projection.MissingDocumentation) != 1 || len(projection.UnreviewedPaths) != 0 {
		t.Fatalf("projection = %#v", projection)
	}
}

func validExecuteEvidence() stageresult.Evidence {
	return stageresult.Evidence{
		SchemaVersion: stageresult.SchemaVersion,
		StageKind:     stageresult.KindExecute,
		Outcome:       stageresult.OutcomeCompleted,
		Execute: &stageresult.ExecutePayload{
			PlanTasks: []stageresult.PlanTask{{ID: "task-0001", Outcome: stageresult.TaskCompleted, Summary: "Added the contract."}},
			Commits:   []stageresult.Commit{{SHA: "abc123", Message: "feat(results): define stage result contracts", TaskIDs: []string{"task-0001"}}},
			Checks: []stageresult.Check{
				{Name: "contract red", Command: "go test ./internal/stageresult -run TestValidate", Result: stageresult.CheckRed, Affected: true},
				{Name: "contract green", Command: "go test ./internal/stageresult -run TestValidate", Result: stageresult.CheckGreen, Affected: true},
			},
			Skips: []stageresult.Skip{},
		},
	}
}

func validCorrectnessEvidence() stageresult.Evidence {
	return stageresult.Evidence{
		SchemaVersion: stageresult.SchemaVersion,
		StageKind:     stageresult.KindCorrectnessReview,
		Outcome:       stageresult.OutcomeCompleted,
		CorrectnessReview: &stageresult.CorrectnessReviewPayload{
			Findings:      []stageresult.Finding{{ID: "F-1", Summary: "invalid result advanced", Status: stageresult.FindingFixed, Paths: []string{"internal/stageresult/result.go"}}},
			Fixes:         []stageresult.Fix{{Summary: "reject invalid result", FindingIDs: []string{"F-1"}, Paths: []string{"internal/stageresult/result.go"}, Commit: "def456"}},
			Checks:        []stageresult.Check{{Name: "regression", Command: "go test ./internal/stageresult", Result: stageresult.CheckGreen, Affected: true}},
			ReviewedPaths: []string{"internal/stageresult/result.go"},
			Skips:         []stageresult.Skip{},
		},
	}
}

func validCleanCodeEvidence() stageresult.Evidence {
	return stageresult.Evidence{
		SchemaVersion: stageresult.SchemaVersion,
		StageKind:     stageresult.KindCleanCodeReview,
		Outcome:       stageresult.OutcomeCompleted,
		CleanCodeReview: &stageresult.CleanCodeReviewPayload{
			Findings:      []stageresult.Finding{},
			Fixes:         []stageresult.Fix{},
			Checks:        []stageresult.Check{{Name: "changed code", Command: "go test ./internal/stageresult", Result: stageresult.CheckGreen, Affected: true}},
			ReviewedPaths: []string{"internal/stageresult/result.go"},
			Skips:         []stageresult.Skip{},
			NoChange:      &stageresult.NoChangeConclusion{Explanation: "changed code follows repository conventions"},
		},
	}
}

func validLibrarianEvidence() stageresult.Evidence {
	return stageresult.Evidence{
		SchemaVersion: stageresult.SchemaVersion,
		StageKind:     stageresult.KindLibrarian,
		Outcome:       stageresult.OutcomeCompleted,
		Librarian: &stageresult.LibrarianPayload{
			ReviewedPaths:        []string{"internal/stageresult/result.go"},
			DocumentationUpdates: []stageresult.DocumentationUpdate{{Path: "docs/guildhall/repo-layout-and-module-path.md", Summary: "document result boundary"}},
			Skips:                []stageresult.Skip{},
		},
	}
}

func mustBuild(t *testing.T, kind stageresult.Kind, evidence stageresult.Evidence) stageresult.Result {
	t.Helper()
	result, err := stageresult.Build(stageresult.BuildInput{
		IssueID: "GH-67", AttemptID: "checkpoint-7", ExpectedKind: kind, Evidence: evidence,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return result
}

func cloneEvidence(t *testing.T, evidence stageresult.Evidence) stageresult.Evidence {
	t.Helper()
	result, err := stageresult.Build(stageresult.BuildInput{
		IssueID: "copy", AttemptID: "copy", ExpectedKind: evidence.StageKind, Evidence: evidence,
	})
	if err != nil {
		t.Fatalf("clone Build() error = %v", err)
	}
	return result.Evidence
}

func payloadCount(e stageresult.Evidence) int {
	count := 0
	for _, present := range []bool{e.Execute != nil, e.CorrectnessReview != nil, e.CleanCodeReview != nil, e.Librarian != nil} {
		if present {
			count++
		}
	}
	return count
}

func containsWork(items []stageresult.WorkItem, text string) bool {
	for _, item := range items {
		if item.Description == text {
			return true
		}
		for _, path := range item.Paths {
			if path == text {
				return true
			}
		}
	}
	return false
}
