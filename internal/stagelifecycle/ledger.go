// Package stagelifecycle defines the versioned, committed lifecycle state
// contract used by stage recovery.
package stagelifecycle

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

type Substate string

const (
	RunnerSucceeded    Substate = "runner_succeeded"
	ArtifactsValidated Substate = "artifacts_validated"
	ArtifactsArchived  Substate = "artifacts_archived"
	GateResolved       Substate = "gate_resolved"
	VerificationPassed Substate = "verification_passed"
	FinalizationReady  Substate = "finalization_ready"
)

const SchemaVersion = 1

type ArtifactRef struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type Record struct {
	SchemaVersion      int           `json:"schema_version"`
	IssueID            string        `json:"issue_id"`
	Stage              string        `json:"stage"`
	AttemptID          string        `json:"attempt_id"`
	Version            int           `json:"version"`
	Substate           Substate      `json:"substate"`
	PredecessorVersion int           `json:"predecessor_version"`
	TransitionID       string        `json:"transition_id"`
	PayloadDigest      string        `json:"payload_digest"`
	ResultRef          string        `json:"result_ref"`
	ResultDigest       string        `json:"result_digest"`
	Artifacts          []ArtifactRef `json:"artifacts"`
	Committed          bool          `json:"committed"`
}

type DiagnosticCode string

const (
	CodeUnknownVersion         DiagnosticCode = "unknown_checkpoint_version"
	CodeInvalidState           DiagnosticCode = "invalid_checkpoint_state"
	CodeConflict               DiagnosticCode = "conflicting_retry_data"
	CodeIntegrity              DiagnosticCode = "integrity_validation_halt"
	CodeMissingResult          DiagnosticCode = "missing_model_result"
	CodeLegacyNormalization    DiagnosticCode = "legacy_normalization_failure"
	CodeCheckpointFinalization DiagnosticCode = "checkpoint_finalization_failure"
)

type DiagnosticError struct {
	Code    DiagnosticCode
	Message string
}

func (e *DiagnosticError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

func diagnostic(code DiagnosticCode, format string, args ...any) error {
	return &DiagnosticError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func Next(current Substate) (Substate, bool) {
	switch current {
	case "":
		return RunnerSucceeded, true
	case RunnerSucceeded:
		return ArtifactsValidated, true
	case ArtifactsValidated:
		return ArtifactsArchived, true
	case ArtifactsArchived:
		return GateResolved, true
	case GateResolved:
		return VerificationPassed, true
	case VerificationPassed:
		return FinalizationReady, true
	default:
		return "", false
	}
}

func ValidateRecord(record Record) error {
	if record.SchemaVersion != SchemaVersion {
		return diagnostic(CodeUnknownVersion, "checkpoint schema version %d is not supported", record.SchemaVersion)
	}
	if !knownSubstate(record.Substate) || record.Version < 1 {
		return diagnostic(CodeInvalidState, "invalid checkpoint substate or version")
	}
	if record.PredecessorVersion < 0 || record.PredecessorVersion >= record.Version {
		return diagnostic(CodeInvalidState, "invalid checkpoint predecessor version")
	}
	if strings.TrimSpace(record.TransitionID) == "" {
		return diagnostic(CodeInvalidState, "checkpoint transition identity is empty")
	}
	if strings.TrimSpace(record.ResultRef) == "" {
		return diagnostic(CodeMissingResult, "checkpoint model result reference is empty")
	}
	if !safeRelativePath(record.ResultRef) {
		return diagnostic(CodeIntegrity, "checkpoint model result reference is unsafe")
	}
	if !validDigest(record.PayloadDigest) {
		return diagnostic(CodeIntegrity, "checkpoint payload digest is invalid")
	}
	if !validDigest(record.ResultDigest) {
		return diagnostic(CodeIntegrity, "checkpoint result digest is invalid")
	}
	if record.AttemptID != "" && !safeComponent(record.AttemptID) {
		return diagnostic(CodeIntegrity, "checkpoint attempt identity is unsafe")
	}
	if record.AttemptID != "" && record.ResultRef != path.Join(
		"artifacts", "attempts", record.AttemptID, "result", "manifest.json",
	) {
		return diagnostic(CodeIntegrity, "checkpoint model result reference is outside the attempt slot")
	}
	for _, artifact := range record.Artifacts {
		if err := validateArtifact(record.AttemptID, artifact); err != nil {
			return err
		}
	}
	if _, err := canonicalArtifacts(record.Artifacts); err != nil {
		return err
	}
	return nil
}

func ValidateTransition(predecessor *Record, next Record) error {
	if err := ValidateRecord(next); err != nil {
		return err
	}
	if predecessor == nil {
		if next.Substate != RunnerSucceeded || next.Version != 1 || next.PredecessorVersion != 0 {
			return diagnostic(CodeInvalidState, "runner_succeeded must be the first checkpoint")
		}
		return nil
	}
	if err := ValidateRecord(*predecessor); err != nil {
		return err
	}
	want, ok := Next(predecessor.Substate)
	if !ok || next.Substate != want || next.Version != predecessor.Version+1 ||
		next.PredecessorVersion != predecessor.Version {
		return diagnostic(CodeInvalidState, "checkpoint transition skips or repeats a predecessor")
	}
	if predecessor.IssueID != "" && next.IssueID != "" && predecessor.IssueID != next.IssueID {
		return diagnostic(CodeConflict, "checkpoint issue identity conflicts with predecessor")
	}
	if predecessor.Stage != "" && next.Stage != "" && predecessor.Stage != next.Stage {
		return diagnostic(CodeConflict, "checkpoint stage identity conflicts with predecessor")
	}
	if predecessor.AttemptID != "" && next.AttemptID != "" && predecessor.AttemptID != next.AttemptID {
		return diagnostic(CodeConflict, "checkpoint attempt identity conflicts with predecessor")
	}
	return nil
}

func SameTransition(left, right Record) bool {
	left.Committed = false
	right.Committed = false
	leftArtifacts, leftErr := canonicalArtifacts(left.Artifacts)
	rightArtifacts, rightErr := canonicalArtifacts(right.Artifacts)
	if leftErr != nil || rightErr != nil {
		return false
	}
	left.Artifacts = leftArtifacts
	right.Artifacts = rightArtifacts
	return left.SchemaVersion == right.SchemaVersion &&
		left.IssueID == right.IssueID && left.Stage == right.Stage &&
		left.AttemptID == right.AttemptID && left.Version == right.Version &&
		left.Substate == right.Substate &&
		left.PredecessorVersion == right.PredecessorVersion &&
		left.TransitionID == right.TransitionID &&
		left.PayloadDigest == right.PayloadDigest &&
		left.ResultRef == right.ResultRef && left.ResultDigest == right.ResultDigest &&
		stringifyArtifacts(left.Artifacts) == stringifyArtifacts(right.Artifacts)
}

func ValidateReplay(existing, replay Record) error {
	if err := ValidateRecord(existing); err != nil {
		return err
	}
	if err := ValidateRecord(replay); err != nil {
		return err
	}
	if !SameTransition(existing, replay) {
		return diagnostic(CodeConflict, "checkpoint replay conflicts with committed transition")
	}
	return nil
}

func CanonicalArtifacts(values []ArtifactRef) ([]ArtifactRef, error) {
	return canonicalArtifacts(values)
}

func knownSubstate(value Substate) bool {
	switch value {
	case RunnerSucceeded, ArtifactsValidated, ArtifactsArchived, GateResolved, VerificationPassed, FinalizationReady:
		return true
	default:
		return false
	}
}

func validateArtifact(attemptID string, artifact ArtifactRef) error {
	if !safeRelativePath(artifact.Name) || strings.Contains(artifact.Name, "/") {
		return diagnostic(CodeIntegrity, "artifact name is unsafe")
	}
	if !validDigest(artifact.SHA256) {
		return diagnostic(CodeIntegrity, "artifact digest is invalid")
	}
	clean := path.Clean(strings.ReplaceAll(artifact.Path, "\\", "/"))
	if clean == "." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return diagnostic(CodeIntegrity, "artifact path is unsafe")
	}
	parts := strings.Split(clean, "/")
	if len(parts) < 3 {
		return diagnostic(CodeIntegrity, "artifact path is outside the attempt namespace")
	}
	start := 0
	if parts[0] == "artifacts" {
		start = 1
	}
	if len(parts) < start+3 || parts[start] != "attempts" {
		return diagnostic(CodeIntegrity, "artifact path is outside the attempt namespace")
	}
	if attemptID != "" && parts[start+1] != attemptID {
		return diagnostic(CodeIntegrity, "artifact path does not match attempt identity")
	}
	if parts[len(parts)-1] != artifact.Name {
		return diagnostic(CodeIntegrity, "artifact path does not match artifact name")
	}
	return nil
}

func canonicalArtifacts(values []ArtifactRef) ([]ArtifactRef, error) {
	result := append([]ArtifactRef(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	for i := 1; i < len(result); i++ {
		if result[i-1].Name == result[i].Name {
			return nil, diagnostic(CodeConflict, "duplicate artifact name %q", result[i].Name)
		}
	}
	return result, nil
}

func stringifyArtifacts(values []ArtifactRef) string {
	var builder strings.Builder
	for _, value := range values {
		builder.WriteString(value.Name)
		builder.WriteByte(0)
		builder.WriteString(value.Path)
		builder.WriteByte(0)
		builder.WriteString(value.SHA256)
		builder.WriteByte(0)
	}
	return builder.String()
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func safeComponent(value string) bool {
	return value != "" && value != "." && value != ".." &&
		!strings.ContainsAny(value, `/\\`) && !strings.Contains(value, "\x00")
}

func safeRelativePath(value string) bool {
	if value == "" || strings.Contains(value, "\x00") || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	clean := path.Clean(value)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}
