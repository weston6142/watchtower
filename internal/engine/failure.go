package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/flow"
)

// failureContext is the transient adapter context. Only the typed metadata
// and final fingerprint leave this boundary; the primary error is returned to
// the operation and is never part of the durable failure contract.
type failureContext struct {
	IssueID           string
	Stage             string
	StageAttempt      int
	Site              failure.Site
	Class             failure.Class
	Disposition       failure.RetryDisposition
	StateChange       failure.StateChange
	FingerprintInputs failure.FingerprintInputs
	Fingerprint       string
	Primary           error
}

// FailureCorrelation is retry guidance derived from the latest applicable
// canonical occurrence. It correlates attempts but never deduplicates them or
// blocks an explicit retry.
type FailureCorrelation struct {
	PriorFingerprint    string
	RetryDisposition    failure.RetryDisposition
	RequiredStateChange failure.StateChange
	FingerprintMatches  bool
	Found               bool
}

func (e *Engine) recordStageFailure(ctx context.Context, is *issueState, stage flow.Stage, attempt, of int, workdir string, primary error) error {
	if primary == nil || errors.Is(primary, errDependenciesDiscovered) {
		return primary
	}
	site, class, disposition, stateChange := classifyFailure(primary)
	return e.recordFailure(ctx, failureContext{
		IssueID: is.id, Stage: stage.Name, StageAttempt: attempt,
		Site: site, Class: class, Disposition: disposition, StateChange: stateChange,
		FingerprintInputs: failure.FingerprintInputs{
			IssueID: is.id, Stage: stage.Name, FailureSite: site,
			WatchtowerIdentity:    digestFailureIdentity("watchtower"),
			ConfigurationIdentity: digestFailureIdentity(stage.Name + ":" + strconv.Itoa(attempt)),
			Git: failure.GitIdentity{
				Repository: digestFailureIdentity(workdir),
				BaseCommit: digestFailureIdentity(is.baseRef),
			},
		},
		Primary: primary,
	})
}

func (e *Engine) recordBoundaryFailure(ctx context.Context, issueID, stage string, attempt int,
	site failure.Site, class failure.Class, disposition failure.RetryDisposition,
	stateChange failure.StateChange, primary error) error {
	if primary == nil {
		return nil
	}
	return e.recordFailure(ctx, failureContext{
		IssueID: issueID, Stage: stage, StageAttempt: attempt,
		Site: site, Class: class, Disposition: disposition, StateChange: stateChange,
		FingerprintInputs: failure.FingerprintInputs{
			IssueID: issueID, Stage: stage, FailureSite: site,
			WatchtowerIdentity:    digestFailureIdentity("watchtower"),
			ConfigurationIdentity: digestFailureIdentity(stage),
		},
		Primary: primary,
	})
}

func digestFailureIdentity(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func classifyFailure(err error) (failure.Site, failure.Class, failure.RetryDisposition, failure.StateChange) {
	var runnerErr *runnerStageError
	if errors.As(err, &runnerErr) {
		return failure.SiteRunner, failure.NormalizeClass(failure.Class(runnerErr.Result.FailureClass)), failure.RetryNow, failure.StateRunnerInput
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "finaliz"), strings.Contains(message, "merge"), strings.Contains(message, "publish"), strings.Contains(message, "cleanup"):
		return failure.SiteFinalization, failure.ClassStateMismatch, failure.RetryAfterStateChange, failure.StateOperator
	case strings.Contains(message, "workspace"), strings.Contains(message, "worktree"), strings.Contains(message, "branch"), strings.Contains(message, "base"):
		return failure.SiteWorkspace, failure.ClassUnavailable, failure.RetryAfterStateChange, failure.StateWorkspace
	case strings.Contains(message, "artifact"), strings.Contains(message, "materialize"), strings.Contains(message, "archive"):
		return failure.SiteArtifact, failure.ClassValidation, failure.RetryAfterStateChange, failure.StateArtifact
	case strings.Contains(message, "planner"), strings.Contains(message, "exploration"), strings.Contains(message, "budget"):
		return failure.SitePlanner, failure.ClassTransport, failure.RetryAfterStateChange, failure.StatePlannerInput
	case strings.Contains(message, "verification"), strings.Contains(message, "receipt"):
		return failure.SiteVerification, failure.ClassValidation, failure.RetryAfterStateChange, failure.StateVerification
	case strings.Contains(message, "cache"):
		return failure.SiteCache, failure.ClassUnavailable, failure.RetryAfterStateChange, failure.StateCache
	case strings.Contains(message, "git"), strings.Contains(message, "revision"):
		return failure.SiteGit, failure.ClassIntegrity, failure.RetryAfterStateChange, failure.StateGit
	default:
		return failure.SiteStore, failure.ClassUnavailable, failure.RetryNow, failure.StateStore
	}
}

// CorrelateFailure returns the latest record for an issue, stage, and site.
func (e *Engine) CorrelateFailure(ctx context.Context, issueID, stage string, site failure.Site, fingerprint string) (FailureCorrelation, error) {
	if e == nil || e.cfg.FailureRecorder == nil {
		return FailureCorrelation{}, nil
	}
	records, err := e.cfg.FailureRecorder.FailureHistory(ctx, issueID)
	if err != nil {
		return FailureCorrelation{}, err
	}
	correlation := FailureCorrelation{}
	for _, record := range records {
		if record.Stage != stage || record.FailureSite != failure.NormalizeSite(site) {
			continue
		}
		correlation = FailureCorrelation{
			PriorFingerprint:    record.Fingerprint,
			RetryDisposition:    record.RetryDisposition,
			RequiredStateChange: record.RequiredStateChange,
			FingerprintMatches:  record.Fingerprint == fingerprint,
			Found:               true,
		}
	}
	return correlation, nil
}

// recordFailure performs durable-first diagnostic recording while preserving
// the operation's primary failure. Recording and event delivery are both
// best-effort, and neither can recursively create another failure record.
func (e *Engine) recordFailure(ctx context.Context, failureCtx failureContext) error {
	primary := failureCtx.Primary
	if e == nil || e.cfg.FailureRecorder == nil {
		return primary
	}
	inputs := failureCtx.FingerprintInputs
	if inputs.IssueID == "" {
		inputs.IssueID = failureCtx.IssueID
	}
	if inputs.Stage == "" {
		inputs.Stage = failureCtx.Stage
	}
	if inputs.FailureSite == "" {
		inputs.FailureSite = failureCtx.Site
	}
	fingerprint := failureCtx.Fingerprint
	if fingerprint == "" {
		fingerprint = failure.BuildFingerprint(inputs)
	}
	site := failure.NormalizeSite(failureCtx.Site)
	class := failure.NormalizeClass(failureCtx.Class)
	disposition := failure.NormalizeRetryDisposition(failureCtx.Disposition)
	stateChange := failure.NormalizeStateChange(failureCtx.StateChange)
	_, _ = e.CorrelateFailure(ctx, failureCtx.IssueID, failureCtx.Stage, site, fingerprint)
	record, err := e.cfg.FailureRecorder.AppendFailure(ctx, failure.RecordInput{
		IssueID: failureCtx.IssueID, Stage: failureCtx.Stage, StageAttempt: failureCtx.StageAttempt,
		FailureSite: site, FailureClass: class, RetryDisposition: disposition,
		RequiredStateChange: stateChange, Fingerprint: fingerprint,
	})
	if err != nil {
		return primary
	}
	if e.cfg.Store != nil {
		if _, err := e.appendEvent(core.EvFailureRecorded, record.IssueID, record); err != nil {
			return primary
		}
	}
	return primary
}
