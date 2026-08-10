package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/verificationcache"
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

func (e *Engine) recordStageFailure(ctx context.Context, is *issueState, stage flow.Stage, attempt int, workdir string, primary error) error {
	if primary == nil || errors.Is(primary, errDependenciesDiscovered) {
		return primary
	}
	var runnerErr *runnerStageError
	if errors.As(primary, &runnerErr) {
		// Each runner result is recorded at its result boundary. The stage
		// wrapper only records failures that were not already site-adapted.
		return primary
	}
	site, class, disposition, stateChange := classifyFailure(primary)
	return e.recordFailure(ctx, failureContext{
		IssueID: is.id, Stage: stage.Name, StageAttempt: attempt,
		Site: site, Class: class, Disposition: disposition, StateChange: stateChange,
		FingerprintInputs: stageFailureFingerprintInputs(e, is, stage, site, workdir),
		Primary:           primary,
	})
}

func (e *Engine) recordRunnerFailure(
	ctx context.Context, is *issueState, stage flow.Stage, attempt int, workdir string, result runner.Result,
) error {
	if result.Err == nil {
		return nil
	}
	return e.recordFailure(ctx, failureContext{
		IssueID: is.id, Stage: stage.Name, StageAttempt: attempt,
		Site: failure.SiteRunner, Class: failure.NormalizeClass(failure.Class(result.FailureClass)),
		Disposition: failure.RetryNow, StateChange: failure.StateRunnerInput,
		FingerprintInputs: stageFailureFingerprintInputs(e, is, stage, failure.SiteRunner, workdir),
		Primary:           result.Err,
	})
}

func stageFailureFingerprintInputs(
	e *Engine, is *issueState, stage flow.Stage, site failure.Site, workdir string,
) failure.FingerprintInputs {
	inputs := failure.FingerprintInputs{
		IssueID:            is.id,
		Stage:              stage.Name,
		FailureSite:        site,
		WatchtowerIdentity: digestFailureIdentity("watchtower:" + runtime.Version()),
		Git: failure.GitIdentity{
			Repository: digestFailureIdentity(workdir),
			BaseCommit: digestFailureIdentity(is.baseRef),
		},
	}

	config, _ := json.Marshal(struct {
		Name         string          `json:"name"`
		Agents       []flow.AgentRef `json:"agents"`
		Parallel     bool            `json:"parallel"`
		Completion   string          `json:"completion"`
		Workspace    string          `json:"workspace"`
		Gate         flow.Gate       `json:"gate"`
		Artifacts    []string        `json:"artifacts"`
		Retries      int             `json:"retries"`
		HeavySlot    bool            `json:"heavy_slot"`
		MergeBarrier bool            `json:"merge_barrier"`
	}{
		Name: stage.Name, Agents: stage.Agents, Parallel: stage.Parallel,
		Completion: stage.Completion, Workspace: stage.Workspace, Gate: stage.Gate,
		Artifacts: stage.Artifacts, Retries: stage.Retries, HeavySlot: stage.HeavySlot,
		MergeBarrier: stage.MergeBarrier,
	})
	inputs.ConfigurationIdentity = digestFailureIdentity(string(config))

	if e.cfg.Store != nil {
		if required, _, err := e.stageContext(is.id); err == nil {
			for _, name := range required {
				inputs.StageInputs = append(inputs.StageInputs, failure.InputIdentity{
					Identity: name, SHA256: fileDigest(filepath.Join(workdir, name)),
				})
			}
		}
	}
	for _, name := range stage.Artifacts {
		inputs.Artifacts = append(inputs.Artifacts, failure.ContentIdentity{
			Identity: name, ContentHash: fileDigest(filepath.Join(workdir, name)),
		})
	}

	if e.cfg.Store != nil {
		if rows, err := e.cfg.Store.AllDecisionRows(); err == nil {
			for _, row := range rows {
				if row.IssueID != is.id {
					continue
				}
				canonical, _ := json.Marshal(struct {
					ID        int64    `json:"id"`
					Stage     string   `json:"stage"`
					Question  string   `json:"question"`
					Options   []string `json:"options"`
					Status    string   `json:"status"`
					Response  any      `json:"response"`
					CreatedAt string   `json:"created_at"`
				}{
					ID: row.ID, Stage: row.Stage, Question: row.Question, Options: row.Options,
					Status: row.Status, Response: row.Response, CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339Nano),
				})
				inputs.Decisions = append(inputs.Decisions, failure.ContentIdentity{
					Identity: strconv.FormatInt(row.ID, 10), ContentHash: digestFailureIdentity(string(canonical)),
				})
			}
		}
	}

	if e.cfg.Train != nil && len(e.cfg.Train.TestCmd) > 0 {
		inputs.Verification.CommandIdentity = verificationcache.CommandDigest(e.cfg.Train.TestCmd)
		cacheRoot := e.cfg.CacheRoot
		if cacheRoot == "" {
			cacheRoot = e.cfg.DataDir
		}
		inputs.Verification.CacheIdentity = digestFailureIdentity(cacheRoot + "\x00" + e.cfg.Train.Repo)
	}
	if head, branch, _ := repositoryState(workdir, nil); head != "" {
		inputs.Git.BranchCommit = digestFailureIdentity(head)
		inputs.Git.Branch = digestFailureIdentity(branch)
		if tree, err := gitRevision(workdir, "HEAD^{tree}"); err == nil {
			inputs.Git.TreeIdentity = digestFailureIdentity(tree)
		}
	}
	return inputs
}

func fileDigest(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return failure.Unavailable
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
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

func (e *Engine) recordClassifiedBoundaryFailure(ctx context.Context, issueID, stage string, primary error) error {
	if primary == nil {
		return nil
	}
	site, class, disposition, stateChange := classifyFailure(primary)
	return e.recordBoundaryFailure(ctx, issueID, stage, 0, site, class, disposition, stateChange, primary)
}

func digestFailureIdentity(value string) string {
	if strings.TrimSpace(value) == "" {
		return failure.Unavailable
	}
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
	case strings.Contains(message, "cache"):
		return failure.SiteCache, failure.ClassUnavailable, failure.RetryAfterStateChange, failure.StateCache
	case strings.Contains(message, "verification"):
		return failure.SiteVerification, failure.ClassValidation, failure.RetryAfterStateChange, failure.StateVerification
	case strings.Contains(message, "git "), strings.Contains(message, "git:"), strings.Contains(message, "revision"), strings.Contains(message, "merge-base"):
		return failure.SiteGit, failure.ClassIntegrity, failure.RetryAfterStateChange, failure.StateGit
	case strings.Contains(message, "workspace"), strings.Contains(message, "worktree"), strings.Contains(message, "branch"), strings.Contains(message, "base"):
		return failure.SiteWorkspace, failure.ClassUnavailable, failure.RetryAfterStateChange, failure.StateWorkspace
	case strings.Contains(message, "artifact"), strings.Contains(message, "materialize"), strings.Contains(message, "archive"):
		return failure.SiteArtifact, failure.ClassValidation, failure.RetryAfterStateChange, failure.StateArtifact
	case strings.Contains(message, "planner"), strings.Contains(message, "exploration"), strings.Contains(message, "budget"):
		return failure.SitePlanner, failure.ClassTransport, failure.RetryAfterStateChange, failure.StatePlannerInput
	case strings.Contains(message, "receipt"):
		return failure.SiteVerification, failure.ClassValidation, failure.RetryAfterStateChange, failure.StateVerification
	case strings.Contains(message, "store"), strings.Contains(message, "stage run"):
		return failure.SiteStore, failure.ClassUnavailable, failure.RetryAfterStateChange, failure.StateStore
	default:
		return failure.SiteUnknown, failure.ClassUnknown, failure.RetryUnknown, failure.StateUnknown
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
	if ctx == nil {
		ctx = context.Background()
	}
	diagnosticCtx := context.WithoutCancel(ctx)
	site := failure.NormalizeSite(failureCtx.Site)
	class := failure.NormalizeClass(failureCtx.Class)
	disposition := failure.NormalizeRetryDisposition(failureCtx.Disposition)
	stateChange := failure.NormalizeStateChange(failureCtx.StateChange)
	inputs := failureCtx.FingerprintInputs
	inputs.IssueID = failureCtx.IssueID
	inputs.Stage = failureCtx.Stage
	inputs.FailureSite = site
	fingerprint := failureCtx.Fingerprint
	if fingerprint == "" {
		fingerprint = failure.BuildFingerprint(inputs)
	}
	record, err := e.cfg.FailureRecorder.AppendFailure(diagnosticCtx, failure.RecordInput{
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
