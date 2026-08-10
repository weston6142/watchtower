// Package failure defines the durable, privacy-safe failure contract.
package failure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type Site string
type Class string
type RetryDisposition string
type StateChange string

const (
	SiteRunner       Site = "runner"
	SiteWorkspace    Site = "workspace"
	SiteArtifact     Site = "artifact"
	SitePlanner      Site = "planner"
	SiteGit          Site = "git"
	SiteVerification Site = "verification"
	SiteCache        Site = "cache"
	SiteStore        Site = "store"
	SiteFinalization Site = "finalization"
	SiteUnknown      Site = "unknown"
	SiteOther        Site = "other"

	ClassLaunch         Class = "launch"
	ClassExecution      Class = "execution"
	ClassTransport      Class = "transport"
	ClassProtocol       Class = "protocol"
	ClassCancellation   Class = "cancellation"
	ClassAuthentication Class = "authentication"
	ClassAuthorization  Class = "authorization"
	ClassResumeIdentity Class = "resume_identity"
	ClassConfiguration  Class = "configuration"
	ClassValidation     Class = "validation"
	ClassIntegrity      Class = "integrity"
	ClassStateMismatch  Class = "state_mismatch"
	ClassUnavailable    Class = "unavailable"
	ClassUnknown        Class = "unknown"
	ClassOther          Class = "other"

	RetryNow              RetryDisposition = "retry_now"
	RetryAfterStateChange RetryDisposition = "retry_after_state_change"
	DoNotRetry            RetryDisposition = "do_not_retry"
	RetryUnknown          RetryDisposition = "unknown"
	RetryOther            RetryDisposition = "other"

	StateNone          StateChange = "none"
	StateRunnerInput   StateChange = "runner_input"
	StateWorkspace     StateChange = "workspace"
	StateArtifact      StateChange = "artifact"
	StatePlannerInput  StateChange = "planner_input"
	StateGit           StateChange = "git"
	StateVerification  StateChange = "verification"
	StateCache         StateChange = "cache"
	StateStore         StateChange = "store"
	StateConfiguration StateChange = "configuration"
	StateOperator      StateChange = "operator"
	StateUnknown       StateChange = "unknown"
	StateOther         StateChange = "other"
)

const (
	SchemaVersion = 1
	Unavailable   = "unavailable"
)

// RecordInput is the safe input accepted by the durable recorder. It contains
// no raw exception, path, command, configuration, artifact, or decision data.
type RecordInput struct {
	IssueID             string
	Stage               string
	StageAttempt        int
	FailureSite         Site
	FailureClass        Class
	RetryDisposition    RetryDisposition
	RequiredStateChange StateChange
	Fingerprint         string
}

// FailureRecord is the canonical occurrence retained by the store and
// projected by events and issue detail.
type FailureRecord struct {
	RecordID            int64            `json:"record_id"`
	SchemaVersion       int              `json:"schema_version"`
	IssueID             string           `json:"issue_id"`
	Stage               string           `json:"stage"`
	StageAttempt        int              `json:"stage_attempt"`
	FailureSite         Site             `json:"failure_site"`
	FailureClass        Class            `json:"failure_class"`
	RetryDisposition    RetryDisposition `json:"retry_disposition"`
	RequiredStateChange StateChange      `json:"required_state_change"`
	Fingerprint         string           `json:"fingerprint"`
	OccurredAt          time.Time        `json:"occurred_at"`
}

// Recorder is the durable failure-record boundary used by engine adapters.
type Recorder interface {
	AppendFailure(context.Context, RecordInput) (FailureRecord, error)
	FailureHistory(context.Context, string) ([]FailureRecord, error)
}

// InputIdentity is a transient identity/digest pair used by the fingerprint
// builder. The values are never stored in a FailureRecord.
type InputIdentity struct {
	Identity string
	SHA256   string
	Digest   string
}

type StageInputIdentity = InputIdentity

type ContentIdentity struct {
	Identity    string
	SHA256      string
	ContentHash string
}

type ArtifactIdentity = ContentIdentity
type DecisionIdentity = ContentIdentity

type GitIdentity struct {
	Repository   string
	Base         string
	Branch       string
	Tree         string
	BaseCommit   string
	BranchCommit string
	TreeIdentity string
}

type VerificationIdentity struct {
	Command         string
	Cache           string
	CommandIdentity string
	CacheIdentity   string
}

// FingerprintInputs contains transient, already-sanitized identity material.
// Aliased fields support callers that use the terminology from the repository
// contracts while the canonical builder emits one stable representation.
type FingerprintInputs struct {
	IssueID               string
	Stage                 string
	FailureSite           Site
	StageInputs           []InputIdentity
	StageInputIdentities  []InputIdentity
	WatchtowerIdentity    string
	ConfigurationIdentity string
	ConfigIdentity        string
	Git                   GitIdentity
	Artifacts             []ContentIdentity
	ArtifactIdentities    []ContentIdentity
	Decisions             []ContentIdentity
	DecisionIdentities    []ContentIdentity
	Verification          VerificationIdentity
}

type fingerprintIdentity struct {
	Identity string `json:"identity"`
	SHA256   string `json:"sha256"`
}

type fingerprintGit struct {
	Repository string `json:"repository"`
	Base       string `json:"base"`
	Branch     string `json:"branch"`
	Tree       string `json:"tree"`
}

type fingerprintVerification struct {
	Command string `json:"command"`
	Cache   string `json:"cache"`
}

type fingerprintV1 struct {
	Version       int                     `json:"version"`
	IssueID       string                  `json:"issue_id"`
	Stage         string                  `json:"stage"`
	FailureSite   Site                    `json:"failure_site"`
	StageInputs   []fingerprintIdentity   `json:"stage_inputs"`
	Watchtower    string                  `json:"watchtower"`
	Configuration string                  `json:"configuration"`
	Git           fingerprintGit          `json:"git"`
	Artifacts     []fingerprintIdentity   `json:"artifacts"`
	Decisions     []fingerprintIdentity   `json:"decisions"`
	Verification  fingerprintVerification `json:"verification"`
}

func identityName(identity string) string {
	if strings.TrimSpace(identity) == "" {
		return Unavailable
	}
	return identity
}

func identityDigest(sha, digest string) string {
	if strings.TrimSpace(sha) != "" {
		return sha
	}
	if strings.TrimSpace(digest) != "" {
		return digest
	}
	return Unavailable
}

func copyIdentities(values []InputIdentity) []fingerprintIdentity {
	out := make([]fingerprintIdentity, 0, len(values))
	for _, value := range values {
		out = append(out, fingerprintIdentity{
			Identity: identityName(value.Identity),
			SHA256:   identityDigest(value.SHA256, value.Digest),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Identity == out[j].Identity {
			return out[i].SHA256 < out[j].SHA256
		}
		return out[i].Identity < out[j].Identity
	})
	return out
}

func copyContentIdentities(values []ContentIdentity) []fingerprintIdentity {
	out := make([]fingerprintIdentity, 0, len(values))
	for _, value := range values {
		out = append(out, fingerprintIdentity{
			Identity: identityName(value.Identity),
			SHA256:   identityDigest(value.SHA256, value.ContentHash),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Identity == out[j].Identity {
			return out[i].SHA256 < out[j].SHA256
		}
		return out[i].Identity < out[j].Identity
	})
	return out
}

func chooseInputs(primary, alias []InputIdentity) []InputIdentity {
	if primary != nil {
		return primary
	}
	return alias
}

func chooseContent(primary, alias []ContentIdentity) []ContentIdentity {
	if primary != nil {
		return primary
	}
	return alias
}

// BuildFingerprint returns a deterministic version-1 fingerprint. Missing
// domains are represented by the fixed unavailable sentinel, never by an
// error or raw source value.
func BuildFingerprint(inputs FingerprintInputs) string {
	git := fingerprintGit{
		Repository: identityName(inputs.Git.Repository),
		Base:       identityName(firstNonEmpty(inputs.Git.BaseCommit, inputs.Git.Base)),
		Branch:     identityName(firstNonEmpty(inputs.Git.BranchCommit, inputs.Git.Branch)),
		Tree:       identityName(firstNonEmpty(inputs.Git.TreeIdentity, inputs.Git.Tree)),
	}
	verification := fingerprintVerification{
		Command: identityName(firstNonEmpty(inputs.Verification.CommandIdentity, inputs.Verification.Command)),
		Cache:   identityName(firstNonEmpty(inputs.Verification.CacheIdentity, inputs.Verification.Cache)),
	}
	canonical := fingerprintV1{
		Version:       SchemaVersion,
		IssueID:       identityName(inputs.IssueID),
		Stage:         identityName(inputs.Stage),
		FailureSite:   NormalizeSite(inputs.FailureSite),
		StageInputs:   copyIdentities(chooseInputs(inputs.StageInputs, inputs.StageInputIdentities)),
		Watchtower:    identityName(inputs.WatchtowerIdentity),
		Configuration: identityName(firstNonEmpty(inputs.ConfigurationIdentity, inputs.ConfigIdentity)),
		Git:           git,
		Artifacts:     copyContentIdentities(chooseContent(inputs.Artifacts, inputs.ArtifactIdentities)),
		Decisions:     copyContentIdentities(chooseContent(inputs.Decisions, inputs.DecisionIdentities)),
		Verification:  verification,
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		// All fields in the canonical form are JSON-safe primitive values. Keep
		// this fallback deterministic if that invariant is ever changed.
		raw = []byte(`{"version":1,"fingerprint":"unavailable"}`)
	}
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func NormalizeSite(value Site) Site {
	if value == "" {
		return SiteUnknown
	}
	if isSite(value) {
		return value
	}
	return SiteOther
}

func NormalizeClass(value Class) Class {
	if value == "" {
		return ClassUnknown
	}
	if isClass(value) {
		return value
	}
	return ClassOther
}

func NormalizeRetryDisposition(value RetryDisposition) RetryDisposition {
	if value == "" {
		return RetryUnknown
	}
	if isRetryDisposition(value) {
		return value
	}
	return RetryOther
}

func NormalizeStateChange(value StateChange) StateChange {
	if value == "" {
		return StateUnknown
	}
	if isStateChange(value) {
		return value
	}
	return StateOther
}

func isSite(value Site) bool {
	switch value {
	case SiteRunner, SiteWorkspace, SiteArtifact, SitePlanner, SiteGit, SiteVerification,
		SiteCache, SiteStore, SiteFinalization, SiteUnknown, SiteOther:
		return true
	default:
		return false
	}
}

func isClass(value Class) bool {
	switch value {
	case ClassLaunch, ClassExecution, ClassTransport, ClassProtocol, ClassCancellation,
		ClassAuthentication, ClassAuthorization, ClassResumeIdentity, ClassConfiguration,
		ClassValidation, ClassIntegrity, ClassStateMismatch, ClassUnavailable, ClassUnknown, ClassOther:
		return true
	default:
		return false
	}
}

func isRetryDisposition(value RetryDisposition) bool {
	switch value {
	case RetryNow, RetryAfterStateChange, DoNotRetry, RetryUnknown, RetryOther:
		return true
	default:
		return false
	}
}

func isStateChange(value StateChange) bool {
	switch value {
	case StateNone, StateRunnerInput, StateWorkspace, StateArtifact, StatePlannerInput,
		StateGit, StateVerification, StateCache, StateStore, StateConfiguration,
		StateOperator, StateUnknown, StateOther:
		return true
	default:
		return false
	}
}

func validFingerprint(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}

func ValidateRecordInput(input RecordInput) error {
	if strings.TrimSpace(input.IssueID) == "" {
		return fmt.Errorf("failure issue identity is required")
	}
	if input.StageAttempt < 0 {
		return fmt.Errorf("failure stage attempt cannot be negative")
	}
	if !isSite(input.FailureSite) {
		return fmt.Errorf("invalid failure site %q", input.FailureSite)
	}
	if !isClass(input.FailureClass) {
		return fmt.Errorf("invalid failure class %q", input.FailureClass)
	}
	if !isRetryDisposition(input.RetryDisposition) {
		return fmt.Errorf("invalid retry disposition %q", input.RetryDisposition)
	}
	if !isStateChange(input.RequiredStateChange) {
		return fmt.Errorf("invalid required state change %q", input.RequiredStateChange)
	}
	if !validFingerprint(input.Fingerprint) {
		return fmt.Errorf("invalid failure fingerprint")
	}
	return nil
}

func ValidateRecord(record FailureRecord) error {
	if record.RecordID <= 0 {
		return fmt.Errorf("failure record ID must be positive")
	}
	if record.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported failure record schema version %d", record.SchemaVersion)
	}
	if record.OccurredAt.IsZero() || record.OccurredAt.Location() != time.UTC {
		return fmt.Errorf("failure record timestamp must be UTC")
	}
	return ValidateRecordInput(RecordInput{
		IssueID: record.IssueID, Stage: record.Stage, StageAttempt: record.StageAttempt,
		FailureSite: record.FailureSite, FailureClass: record.FailureClass,
		RetryDisposition: record.RetryDisposition, RequiredStateChange: record.RequiredStateChange,
		Fingerprint: record.Fingerprint,
	})
}
