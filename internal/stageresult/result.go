package stageresult

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

const SchemaVersion = 1

type Kind string

const (
	KindExecute           Kind = "execute"
	KindCorrectnessReview Kind = "correctness_review"
	KindCleanCodeReview   Kind = "clean_code_review"
	KindLibrarian         Kind = "librarian"
)

type Outcome string

const (
	OutcomeCompleted Outcome = "completed"
	OutcomeRetryable Outcome = "retryable"
)

type ValidationStatus string

const (
	ValidationInvalid ValidationStatus = "invalid"
	ValidationValid   ValidationStatus = "valid"
)

type WorkKind string

const (
	WorkPlanTask      WorkKind = "plan_task"
	WorkFinding       WorkKind = "finding"
	WorkCheck         WorkKind = "check"
	WorkReviewPath    WorkKind = "review_path"
	WorkDocumentation WorkKind = "documentation"
)

type TaskOutcome string

const (
	TaskCompleted TaskOutcome = "completed"
	TaskRemaining TaskOutcome = "remaining"
	TaskSkipped   TaskOutcome = "skipped"
)

type CheckResult string

const (
	CheckRed   CheckResult = "red"
	CheckGreen CheckResult = "green"
)

type FindingStatus string

const (
	FindingOpen  FindingStatus = "open"
	FindingFixed FindingStatus = "fixed"
)

type WorkItem struct {
	Kind        WorkKind `json:"kind"`
	Description string   `json:"description"`
	Paths       []string `json:"paths,omitempty"`
}

type Concern struct {
	Explanation string `json:"explanation"`
}

type Skip struct {
	Activity    string `json:"activity"`
	Explanation string `json:"explanation"`
}

type PlanTask struct {
	ID      string      `json:"id"`
	Outcome TaskOutcome `json:"outcome"`
	Summary string      `json:"summary"`
}

type Commit struct {
	SHA     string   `json:"sha"`
	Message string   `json:"message"`
	TaskIDs []string `json:"task_ids"`
}

type Check struct {
	Name     string      `json:"name"`
	Command  string      `json:"command"`
	Result   CheckResult `json:"result"`
	Affected bool        `json:"affected"`
}

type Finding struct {
	ID      string        `json:"id"`
	Summary string        `json:"summary"`
	Status  FindingStatus `json:"status"`
	Paths   []string      `json:"paths,omitempty"`
}

type Fix struct {
	Summary    string   `json:"summary"`
	FindingIDs []string `json:"finding_ids"`
	Paths      []string `json:"paths"`
	Commit     string   `json:"commit,omitempty"`
}

type NoChangeConclusion struct {
	Explanation string `json:"explanation"`
}

type DocumentationUpdate struct {
	Path    string `json:"path"`
	Summary string `json:"summary"`
}

type ExecutePayload struct {
	PlanTasks []PlanTask `json:"plan_tasks"`
	Commits   []Commit   `json:"commits"`
	Checks    []Check    `json:"checks"`
	Skips     []Skip     `json:"skips"`
}

type CorrectnessReviewPayload struct {
	Findings      []Finding           `json:"findings"`
	Fixes         []Fix               `json:"fixes"`
	Checks        []Check             `json:"checks"`
	ReviewedPaths []string            `json:"reviewed_paths"`
	Skips         []Skip              `json:"skips"`
	NoChange      *NoChangeConclusion `json:"no_change"`
}

type CleanCodeReviewPayload struct {
	Findings      []Finding           `json:"findings"`
	Fixes         []Fix               `json:"fixes"`
	Checks        []Check             `json:"checks"`
	ReviewedPaths []string            `json:"reviewed_paths"`
	Skips         []Skip              `json:"skips"`
	NoChange      *NoChangeConclusion `json:"no_change"`
}

type LibrarianPayload struct {
	ReviewedPaths        []string              `json:"reviewed_paths"`
	DocumentationUpdates []DocumentationUpdate `json:"documentation_updates"`
	Skips                []Skip                `json:"skips"`
	NoChange             *NoChangeConclusion   `json:"no_change"`
}

type reviewPayload struct {
	findings      []Finding
	fixes         []Fix
	checks        []Check
	reviewedPaths []string
	skips         []Skip
	noChange      *NoChangeConclusion
}

type Evidence struct {
	SchemaVersion     int                       `json:"schema_version"`
	StageKind         Kind                      `json:"stage_kind"`
	Outcome           Outcome                   `json:"outcome"`
	RemainingWork     []WorkItem                `json:"remaining_work"`
	RemainingConcerns []Concern                 `json:"remaining_concerns"`
	Execute           *ExecutePayload           `json:"execute,omitempty"`
	CorrectnessReview *CorrectnessReviewPayload `json:"correctness_review,omitempty"`
	CleanCodeReview   *CleanCodeReviewPayload   `json:"clean_code_review,omitempty"`
	Librarian         *LibrarianPayload         `json:"librarian,omitempty"`
}

type Result struct {
	Evidence
	IssueID              string           `json:"issue_id"`
	AttemptID            string           `json:"attempt_id"`
	PredecessorAttemptID string           `json:"predecessor_attempt_id,omitempty"`
	ValidationStatus     ValidationStatus `json:"validation_status"`
}

type BuildInput struct {
	IssueID              string
	AttemptID            string
	PredecessorAttemptID string
	ExpectedKind         Kind
	Evidence             Evidence
}

type RetryContext struct {
	SourceAttemptID      string     `json:"source_attempt_id"`
	RemainingWork        []WorkItem `json:"remaining_work"`
	UnfinishedPlanTasks  []WorkItem `json:"unfinished_plan_tasks"`
	OpenFindings         []Finding  `json:"open_findings"`
	SkippedActivities    []Skip     `json:"skipped_activities"`
	UnreviewedPaths      []WorkItem `json:"unreviewed_paths"`
	MissingDocumentation []WorkItem `json:"missing_documentation"`
	RemainingConcerns    []Concern  `json:"remaining_concerns"`
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid stage result %s: %s", e.Field, e.Message)
}

func Build(input BuildInput) (Result, error) {
	if !supportedKind(input.ExpectedKind) {
		return Result{}, invalid("expected_kind", "must identify a supported result-producing stage")
	}
	if input.Evidence.StageKind != input.ExpectedKind {
		return Result{}, invalid("stage_kind", fmt.Sprintf("got %q, expected %q", input.Evidence.StageKind, input.ExpectedKind))
	}
	evidence, err := clone(input.Evidence)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Evidence:             evidence,
		IssueID:              input.IssueID,
		AttemptID:            input.AttemptID,
		PredecessorAttemptID: input.PredecessorAttemptID,
		ValidationStatus:     ValidationInvalid,
	}, nil
}

func Validate(candidate Result) (Result, error) {
	result, err := clone(candidate)
	if err != nil {
		return Result{}, err
	}
	result.ValidationStatus = ValidationInvalid
	if err := validateResult(result); err != nil {
		return result, err
	}
	result.ValidationStatus = ValidationValid
	return result, nil
}

func ValidatePersisted(candidate Result) (Result, error) {
	if candidate.ValidationStatus != ValidationValid {
		return Result{}, invalid("validation_status", "must be valid before persistence")
	}
	return Validate(candidate)
}

func KindForAgentPackage(name string) (Kind, bool) {
	switch name {
	case "executor":
		return KindExecute, true
	case "correctness-reviewer":
		return KindCorrectnessReview, true
	case "clean-code-reviewer":
		return KindCleanCodeReview, true
	case "librarian":
		return KindLibrarian, true
	default:
		return "", false
	}
}

func ProjectRetry(result Result) (RetryContext, error) {
	validated, err := ValidatePersisted(result)
	if err != nil {
		return RetryContext{}, err
	}
	context := RetryContext{
		SourceAttemptID:   validated.AttemptID,
		RemainingConcerns: validated.RemainingConcerns,
	}
	for _, item := range validated.RemainingWork {
		switch item.Kind {
		case WorkPlanTask:
			context.UnfinishedPlanTasks = append(context.UnfinishedPlanTasks, item)
		case WorkReviewPath:
			context.UnreviewedPaths = append(context.UnreviewedPaths, item)
		case WorkDocumentation:
			context.MissingDocumentation = append(context.MissingDocumentation, item)
		default:
			context.RemainingWork = append(context.RemainingWork, item)
		}
	}
	switch validated.StageKind {
	case KindExecute:
		context.SkippedActivities = validated.Execute.Skips
	case KindCorrectnessReview:
		context.OpenFindings = openFindings(validated.CorrectnessReview.Findings)
		context.SkippedActivities = validated.CorrectnessReview.Skips
	case KindCleanCodeReview:
		context.OpenFindings = openFindings(validated.CleanCodeReview.Findings)
		context.SkippedActivities = validated.CleanCodeReview.Skips
	case KindLibrarian:
		context.SkippedActivities = validated.Librarian.Skips
	}
	return clone(context)
}

func validateResult(result Result) error {
	if result.SchemaVersion != SchemaVersion {
		return invalid("schema_version", fmt.Sprintf("unsupported version %d", result.SchemaVersion))
	}
	if strings.TrimSpace(result.IssueID) == "" {
		return invalid("issue_id", "must not be blank")
	}
	if strings.TrimSpace(result.AttemptID) == "" {
		return invalid("attempt_id", "must not be blank")
	}
	if !supportedKind(result.StageKind) {
		return invalid("stage_kind", "must identify a supported result-producing stage")
	}
	if result.PredecessorAttemptID != "" && result.PredecessorAttemptID == result.AttemptID {
		return invalid("predecessor_attempt_id", "must refer to a different attempt")
	}
	if result.Outcome != OutcomeCompleted && result.Outcome != OutcomeRetryable {
		return invalid("outcome", "must be completed or retryable")
	}
	if err := validateWork(result.RemainingWork); err != nil {
		return err
	}
	for i, concern := range result.RemainingConcerns {
		if strings.TrimSpace(concern.Explanation) == "" {
			return invalid(fmt.Sprintf("remaining_concerns[%d].explanation", i), "must be actionable")
		}
	}
	if result.Outcome == OutcomeRetryable && len(result.RemainingWork) == 0 && len(result.RemainingConcerns) == 0 {
		return invalid("outcome", "retryable requires remaining work or a remaining concern")
	}
	if result.Outcome == OutcomeCompleted && len(result.RemainingWork) != 0 {
		return invalid("remaining_work", "must be empty when outcome is completed")
	}
	if result.Outcome == OutcomeCompleted && len(result.RemainingConcerns) != 0 {
		return invalid("remaining_concerns", "must be empty when outcome is completed")
	}

	if payloadCount(result.Evidence) != 1 {
		return invalid("payload", "must contain exactly one stage payload")
	}
	switch result.StageKind {
	case KindExecute:
		if result.Execute == nil {
			return invalid("execute", "payload must match stage_kind")
		}
		return validateExecute(result.Outcome, result.RemainingWork, result.Execute)
	case KindCorrectnessReview:
		if result.CorrectnessReview == nil {
			return invalid("correctness_review", "payload must match stage_kind")
		}
		return validateReview("correctness_review", result.Outcome, reviewPayload{
			findings: result.CorrectnessReview.Findings, fixes: result.CorrectnessReview.Fixes,
			checks: result.CorrectnessReview.Checks, reviewedPaths: result.CorrectnessReview.ReviewedPaths,
			skips: result.CorrectnessReview.Skips, noChange: result.CorrectnessReview.NoChange,
		})
	case KindCleanCodeReview:
		if result.CleanCodeReview == nil {
			return invalid("clean_code_review", "payload must match stage_kind")
		}
		return validateReview("clean_code_review", result.Outcome, reviewPayload{
			findings: result.CleanCodeReview.Findings, fixes: result.CleanCodeReview.Fixes,
			checks: result.CleanCodeReview.Checks, reviewedPaths: result.CleanCodeReview.ReviewedPaths,
			skips: result.CleanCodeReview.Skips, noChange: result.CleanCodeReview.NoChange,
		})
	case KindLibrarian:
		if result.Librarian == nil {
			return invalid("librarian", "payload must match stage_kind")
		}
		return validateLibrarian(result.Librarian)
	}
	return invalid("stage_kind", "unsupported stage kind")
}

func validateExecute(outcome Outcome, remainingWork []WorkItem, payload *ExecutePayload) error {
	if len(payload.PlanTasks) == 0 {
		return invalid("execute.plan_tasks", "must record task outcomes")
	}
	taskIDs := make(map[string]struct{}, len(payload.PlanTasks))
	hasRemainingTask := false
	for i, task := range payload.PlanTasks {
		prefix := fmt.Sprintf("execute.plan_tasks[%d]", i)
		if strings.TrimSpace(task.ID) == "" {
			return invalid(prefix+".id", "must not be blank")
		}
		if _, exists := taskIDs[task.ID]; exists {
			return invalid(prefix+".id", "must be unique")
		}
		taskIDs[task.ID] = struct{}{}
		if task.Outcome != TaskCompleted && task.Outcome != TaskRemaining && task.Outcome != TaskSkipped {
			return invalid(prefix+".outcome", "must be completed, remaining, or skipped")
		}
		if outcome == OutcomeCompleted && task.Outcome == TaskRemaining {
			return invalid(prefix+".outcome", "remaining task requires a retryable result")
		}
		hasRemainingTask = hasRemainingTask || task.Outcome == TaskRemaining
		if strings.TrimSpace(task.Summary) == "" {
			return invalid(prefix+".summary", "must not be blank")
		}
	}
	if hasRemainingTask && !hasWorkKind(remainingWork, WorkPlanTask) {
		return invalid("remaining_work", "remaining plan tasks require an actionable plan_task work item")
	}
	if err := validateSkips("execute.skips", payload.Skips); err != nil {
		return err
	}
	if len(payload.Commits) == 0 && !hasSkip(payload.Skips, "commits") {
		return invalid("execute.commits", "must record commits or an explained commits skip")
	}
	for i, commit := range payload.Commits {
		prefix := fmt.Sprintf("execute.commits[%d]", i)
		if strings.TrimSpace(commit.SHA) == "" || strings.TrimSpace(commit.Message) == "" {
			return invalid(prefix, "sha and message must not be blank")
		}
		for _, taskID := range commit.TaskIDs {
			if _, ok := taskIDs[taskID]; !ok {
				return invalid(prefix+".task_ids", fmt.Sprintf("references unknown task %q", taskID))
			}
		}
	}
	if len(payload.Checks) == 0 && !hasSkip(payload.Skips, "checks") {
		return invalid("execute.checks", "must record checks or an explained checks skip")
	}
	return validateChecks("execute.checks", payload.Checks)
}

func validateReview(prefix string, outcome Outcome, payload reviewPayload) error {
	if err := validateFindings(prefix+".findings", payload.findings); err != nil {
		return err
	}
	if err := validateFixes(prefix+".fixes", payload.findings, payload.fixes); err != nil {
		return err
	}
	fixed := make(map[string]bool, len(payload.findings))
	for _, fix := range payload.fixes {
		for _, findingID := range fix.FindingIDs {
			fixed[findingID] = true
		}
	}
	for i, finding := range payload.findings {
		if outcome == OutcomeCompleted && finding.Status == FindingOpen {
			return invalid(fmt.Sprintf("%s.findings[%d].status", prefix, i), "open finding requires a retryable result")
		}
		if finding.Status == FindingFixed && !fixed[finding.ID] {
			return invalid(fmt.Sprintf("%s.findings[%d].status", prefix, i), "fixed finding requires a recorded fix")
		}
	}
	if err := validateChecks(prefix+".checks", payload.checks); err != nil {
		return err
	}
	if err := validatePaths(prefix+".reviewed_paths", payload.reviewedPaths); err != nil {
		return err
	}
	if err := validateSkips(prefix+".skips", payload.skips); err != nil {
		return err
	}
	if len(payload.checks) == 0 && !hasSkip(payload.skips, "checks") {
		return invalid(prefix+".checks", "must record checks or an explained checks skip")
	}
	if len(payload.reviewedPaths) == 0 && !hasSkip(payload.skips, "reviewed_paths") {
		return invalid(prefix+".reviewed_paths", "must record reviewed paths or an explained reviewed-paths skip")
	}
	if len(payload.fixes) > 0 && payload.noChange != nil {
		return invalid(prefix+".no_change", "must not coexist with fixes")
	}
	if len(payload.fixes) == 0 && payload.noChange == nil {
		return invalid(prefix+".no_change", "is required when no fixes were made")
	}
	if payload.noChange != nil && strings.TrimSpace(payload.noChange.Explanation) == "" {
		return invalid(prefix+".no_change.explanation", "must not be blank")
	}
	return nil
}

func validateLibrarian(payload *LibrarianPayload) error {
	if err := validatePaths("librarian.reviewed_paths", payload.ReviewedPaths); err != nil {
		return err
	}
	if err := validateSkips("librarian.skips", payload.Skips); err != nil {
		return err
	}
	if len(payload.ReviewedPaths) == 0 && !hasSkip(payload.Skips, "reviewed_paths") {
		return invalid("librarian.reviewed_paths", "must record reviewed paths or an explained reviewed-paths skip")
	}
	for i, update := range payload.DocumentationUpdates {
		prefix := fmt.Sprintf("librarian.documentation_updates[%d]", i)
		if err := validatePath(prefix+".path", update.Path); err != nil {
			return err
		}
		if strings.TrimSpace(update.Summary) == "" {
			return invalid(prefix+".summary", "must not be blank")
		}
	}
	if len(payload.DocumentationUpdates) > 0 && payload.NoChange != nil {
		return invalid("librarian.no_change", "must not coexist with documentation updates")
	}
	if len(payload.DocumentationUpdates) == 0 && payload.NoChange == nil {
		return invalid("librarian.no_change", "is required when documentation was not updated")
	}
	if payload.NoChange != nil && strings.TrimSpace(payload.NoChange.Explanation) == "" {
		return invalid("librarian.no_change.explanation", "must not be blank")
	}
	return nil
}

func validateWork(items []WorkItem) error {
	for i, item := range items {
		prefix := fmt.Sprintf("remaining_work[%d]", i)
		switch item.Kind {
		case WorkPlanTask, WorkFinding, WorkCheck, WorkReviewPath, WorkDocumentation:
		default:
			return invalid(prefix+".kind", "must be a supported work kind")
		}
		if strings.TrimSpace(item.Description) == "" {
			return invalid(prefix+".description", "must be actionable")
		}
		if err := validatePaths(prefix+".paths", item.Paths); err != nil {
			return err
		}
	}
	return nil
}

func validateFindings(prefix string, findings []Finding) error {
	ids := make(map[string]struct{}, len(findings))
	for i, finding := range findings {
		field := fmt.Sprintf("%s[%d]", prefix, i)
		if strings.TrimSpace(finding.ID) == "" || strings.TrimSpace(finding.Summary) == "" {
			return invalid(field, "id and summary must not be blank")
		}
		if _, exists := ids[finding.ID]; exists {
			return invalid(field+".id", "must be unique")
		}
		ids[finding.ID] = struct{}{}
		if finding.Status != FindingOpen && finding.Status != FindingFixed {
			return invalid(field+".status", "must be open or fixed")
		}
		if err := validatePaths(field+".paths", finding.Paths); err != nil {
			return err
		}
	}
	return nil
}

func validateFixes(prefix string, findings []Finding, fixes []Fix) error {
	ids := make(map[string]struct{}, len(findings))
	for _, finding := range findings {
		ids[finding.ID] = struct{}{}
	}
	for i, fix := range fixes {
		field := fmt.Sprintf("%s[%d]", prefix, i)
		if strings.TrimSpace(fix.Summary) == "" {
			return invalid(field+".summary", "must not be blank")
		}
		if len(fix.FindingIDs) == 0 {
			return invalid(field+".finding_ids", "must identify an addressed finding")
		}
		for _, findingID := range fix.FindingIDs {
			if _, ok := ids[findingID]; !ok {
				return invalid(field+".finding_ids", fmt.Sprintf("references unknown finding %q", findingID))
			}
		}
		if len(fix.Paths) == 0 {
			return invalid(field+".paths", "must identify changed paths")
		}
		if err := validatePaths(field+".paths", fix.Paths); err != nil {
			return err
		}
	}
	return nil
}

func validateChecks(prefix string, checks []Check) error {
	for i, check := range checks {
		field := fmt.Sprintf("%s[%d]", prefix, i)
		if strings.TrimSpace(check.Name) == "" || strings.TrimSpace(check.Command) == "" {
			return invalid(field, "name and command must not be blank")
		}
		if check.Result != CheckRed && check.Result != CheckGreen {
			return invalid(field+".result", "must be red or green")
		}
	}
	return nil
}

func validateSkips(prefix string, skips []Skip) error {
	for i, skip := range skips {
		field := fmt.Sprintf("%s[%d]", prefix, i)
		if strings.TrimSpace(skip.Activity) == "" {
			return invalid(field+".activity", "must not be blank")
		}
		if strings.TrimSpace(skip.Explanation) == "" {
			return invalid(field+".explanation", "must be actionable")
		}
	}
	return nil
}

func validatePaths(prefix string, paths []string) error {
	for i, path := range paths {
		if err := validatePath(fmt.Sprintf("%s[%d]", prefix, i), path); err != nil {
			return err
		}
	}
	return nil
}

func validatePath(field, path string) error {
	trimmed := strings.TrimSpace(path)
	clean := filepath.Clean(trimmed)
	if trimmed == "" || filepath.IsAbs(trimmed) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return invalid(field, "must be a nonblank repository-relative path")
	}
	return nil
}

func supportedKind(kind Kind) bool {
	switch kind {
	case KindExecute, KindCorrectnessReview, KindCleanCodeReview, KindLibrarian:
		return true
	default:
		return false
	}
}

func payloadCount(e Evidence) int {
	count := 0
	for _, present := range []bool{e.Execute != nil, e.CorrectnessReview != nil, e.CleanCodeReview != nil, e.Librarian != nil} {
		if present {
			count++
		}
	}
	return count
}

func hasSkip(skips []Skip, activity string) bool {
	for _, skip := range skips {
		if normalizeActivity(skip.Activity) == normalizeActivity(activity) && strings.TrimSpace(skip.Explanation) != "" {
			return true
		}
	}
	return false
}

func hasWorkKind(items []WorkItem, kind WorkKind) bool {
	for _, item := range items {
		if item.Kind == kind {
			return true
		}
	}
	return false
}

func normalizeActivity(activity string) string {
	return strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(activity)))
}

func openFindings(findings []Finding) []Finding {
	var open []Finding
	for _, finding := range findings {
		if finding.Status == FindingOpen {
			open = append(open, finding)
		}
	}
	return open
}

func invalid(field, message string) *ValidationError {
	return &ValidationError{Field: field, Message: message}
}

func clone[T any](value T) (T, error) {
	var copied T
	data, err := json.Marshal(value)
	if err != nil {
		return copied, fmt.Errorf("copy stage result: %w", err)
	}
	if err := json.Unmarshal(data, &copied); err != nil {
		return copied, fmt.Errorf("copy stage result: %w", err)
	}
	return copied, nil
}
