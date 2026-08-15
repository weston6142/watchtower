package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/core"
	"github.com/weston6142/watchtower/internal/failure"
	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/retry"
	"github.com/weston6142/watchtower/internal/runner"
	"github.com/weston6142/watchtower/internal/stagelifecycle"
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
	if isCommittedInterruption(primary) {
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
	var policyErr *capability.PolicyError
	if errors.As(result.Err, &policyErr) {
		site, class, disposition, stateChange := classifyFailure(result.Err)
		return e.recordFailure(ctx, failureContext{
			IssueID: is.id, Stage: stage.Name, StageAttempt: attempt,
			Site: site, Class: class, Disposition: disposition, StateChange: stateChange,
			FingerprintInputs: stageFailureFingerprintInputs(e, is, stage, site, workdir),
			Primary:           result.Err,
		})
	}
	class := failure.Class(result.FailureClass)
	if result.FailureClass == "" {
		// A runner that reports an error without a more specific typed class
		// failed while executing the agent. Classify that boundary once when
		// recording it; retry authorization never reclassifies error text.
		class = failure.ClassExecution
	}
	class = failure.NormalizeClass(class)
	disposition, stateChange := runnerFailureRetryGuidance(class)
	return e.recordFailure(ctx, failureContext{
		IssueID: is.id, Stage: stage.Name, StageAttempt: attempt,
		Site: failure.SiteRunner, Class: class,
		Disposition: disposition, StateChange: stateChange,
		FingerprintInputs: stageFailureFingerprintInputs(e, is, stage, failure.SiteRunner, workdir),
		Primary:           result.Err,
	})
}

func runnerFailureRetryGuidance(class failure.Class) (failure.RetryDisposition, failure.StateChange) {
	switch class {
	case failure.ClassLaunch, failure.ClassExecution, failure.ClassTransport,
		failure.ClassProtocol, failure.ClassCancellation:
		return failure.RetryNow, failure.StateRunnerInput
	case failure.ClassAuthentication, failure.ClassAuthorization, failure.ClassConfiguration:
		return failure.RetryAfterStateChange, failure.StateConfiguration
	case failure.ClassResumeIdentity:
		return failure.RetryAfterStateChange, failure.StateOperator
	default:
		return failure.RetryUnknown, failure.StateUnknown
	}
}

func stageFailureFingerprintInputs(
	e *Engine, is *issueState, stage flow.Stage, site failure.Site, workdir string,
) failure.FingerprintInputs {
	var contextPaths []string
	inputs := failure.FingerprintInputs{
		IssueID:            is.id,
		Stage:              stage.Name,
		FailureSite:        site,
		WatchtowerIdentity: digestFailureIdentity("watchtower:" + runtime.Version()),
		EnvironmentIdentity: digestFailureIdentity(strings.Join([]string{
			runtime.GOOS, runtime.GOARCH, runtime.Version(), e.cfg.DataDir, e.cfg.CacheRoot,
		}, "\x00")),
		Verification: failure.VerificationIdentity{
			CommandIdentity: digestFailureIdentity("verification:none"),
			CacheIdentity:   digestFailureIdentity("verification-cache:none"),
		},
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
		RetryPolicy  retry.Policy    `json:"retry_policy"`
	}{
		Name: stage.Name, Agents: stage.Agents, Parallel: stage.Parallel,
		Completion: stage.Completion, Workspace: stage.Workspace, Gate: stage.Gate,
		Artifacts: stage.Artifacts, Retries: stage.Retries, HeavySlot: stage.HeavySlot,
		MergeBarrier: stage.MergeBarrier, RetryPolicy: e.cfg.RetryPolicy,
	})
	inputs.ConfigurationIdentity = digestFailureIdentity(string(config))

	if e.cfg.Store != nil {
		if required, _, err := e.stageContext(is.id); err == nil {
			for _, name := range required {
				contextPaths = append(contextPaths, name)
				digest := fileDigest(filepath.Join(workdir, name))
				if digest == failure.Unavailable {
					digest = fileDigest(filepath.Join(e.issueDir(is.id), "artifacts", name))
				}
				inputs.StageInputs = append(inputs.StageInputs, failure.InputIdentity{
					Identity: name, SHA256: digest,
				})
			}
		} else {
			inputs.StageInputs = []failure.InputIdentity{{
				Identity: failure.Unavailable, SHA256: failure.Unavailable,
			}}
		}
	} else {
		inputs.StageInputs = []failure.InputIdentity{{
			Identity: failure.Unavailable, SHA256: failure.Unavailable,
		}}
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
				inputs.Decisions = append(inputs.Decisions, failure.ContentIdentity{
					Identity: strconv.FormatInt(row.ID, 10), ContentHash: durableDecisionIdentity(row),
				})
			}
		} else {
			inputs.Decisions = []failure.ContentIdentity{{
				Identity: failure.Unavailable, ContentHash: failure.Unavailable,
			}}
		}
	} else {
		inputs.Decisions = []failure.ContentIdentity{{
			Identity: failure.Unavailable, ContentHash: failure.Unavailable,
		}}
	}

	if e.cfg.Train != nil && len(e.cfg.Train.TestCmd) > 0 {
		inputs.Verification.CommandIdentity = verificationcache.CommandDigest(e.cfg.Train.TestCmd)
		cacheRoot := e.cfg.CacheRoot
		if cacheRoot == "" {
			cacheRoot = e.cfg.DataDir
		}
		inputs.Verification.CacheIdentity = digestFailureIdentity(cacheRoot + "\x00" + e.cfg.Train.Repo)
	}
	if stage.Workspace == "none" {
		// A stage-local issue directory is still an authoritative tree. Bind
		// its identity to the materialized inputs and declared artifacts so a
		// missing Git repository is not confused with missing evidence.
		canonical, _ := json.Marshal(struct {
			Inputs    []failure.InputIdentity   `json:"inputs"`
			Artifacts []failure.ContentIdentity `json:"artifacts"`
		}{Inputs: inputs.StageInputs, Artifacts: inputs.Artifacts})
		inputs.Git.Repository = digestFailureIdentity("workspace:none")
		inputs.Git.BaseCommit = digestFailureIdentity("workspace:none:base")
		inputs.Git.BranchCommit = digestFailureIdentity("workspace:none:" + is.id)
		inputs.Git.TreeIdentity = digestFailureIdentity(string(canonical))
	}
	if head, branch, _ := repositoryState(workdir, contextPaths); head != "" {
		inputs.Git.Repository = gitRepositoryIdentity(workdir)
		inputs.Git.BranchCommit = digestFailureIdentity(head)
		inputs.Git.Branch = digestFailureIdentity(branch)
		inputs.Git.TreeIdentity = gitWorktreeIdentity(workdir, contextPaths)
	}
	return inputs
}

// gitRepositoryIdentity remains stable when the same repository state is
// materialized in a different linked-worktree or lease directory.
func gitRepositoryIdentity(workdir string) string {
	commonDir, err := verificationcache.CanonicalRepositoryIdentity(workdir)
	if err != nil {
		return failure.Unavailable
	}
	return digestFailureIdentity("git-common-dir:" + commonDir)
}

// gitWorktreeIdentity binds retry evidence to both HEAD and non-context
// tracked or untracked changes. Only the resulting digest leaves this helper;
// paths, status records, and file contents remain transient.
func gitWorktreeIdentity(workdir string, contextPaths []string) string {
	tree, err := gitRevision(workdir, "HEAD^{tree}")
	if err != nil {
		return failure.Unavailable
	}
	args := []string{"-C", workdir, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", ".",
		":(exclude)ISSUE.md", ":(exclude)STAGE.md", ":(exclude)decisions.md", ":(exclude)attachments/**"}
	for _, name := range contextPaths {
		if clean := filepath.ToSlash(filepath.Clean(name)); clean != "." && clean != "" {
			args = append(args, ":(exclude)"+clean)
		}
	}
	status, err := exec.Command("git", args...).Output()
	if err != nil {
		return failure.Unavailable
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(tree))
	for _, record := range strings.Split(string(status), "\x00") {
		if record == "" {
			continue
		}
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(record))
		path := record
		if len(record) >= 4 && record[2] == ' ' {
			path = record[3:]
		}
		body, readErr := os.ReadFile(filepath.Join(workdir, filepath.FromSlash(path)))
		if readErr != nil {
			_, _ = hash.Write([]byte("\x00missing"))
			continue
		}
		content := sha256.Sum256(body)
		_, _ = hash.Write(content[:])
	}
	return hex.EncodeToString(hash.Sum(nil))
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
	if isCommittedInterruption(primary) {
		return primary
	}
	if primary == nil {
		return nil
	}
	inputs := failure.FingerprintInputs{
		IssueID: issueID, Stage: stage, FailureSite: site,
		WatchtowerIdentity:    digestFailureIdentity("watchtower"),
		ConfigurationIdentity: digestFailureIdentity(stage),
	}
	if staged, ok := e.boundaryStageFingerprintInputs(issueID, stage, site); ok {
		inputs = staged
	}
	return e.recordFailure(ctx, failureContext{
		IssueID: issueID, Stage: stage, StageAttempt: attempt,
		Site: site, Class: class, Disposition: disposition, StateChange: stateChange,
		FingerprintInputs: inputs,
		Primary:           primary,
	})
}

func (e *Engine) boundaryStageFingerprintInputs(
	issueID, stage string, site failure.Site,
) (failure.FingerprintInputs, bool) {
	if e == nil || strings.TrimSpace(stage) == "" {
		return failure.FingerprintInputs{}, false
	}
	e.mu.Lock()
	current, ok := e.issues[issueID]
	if !ok {
		e.mu.Unlock()
		return failure.FingerprintInputs{}, false
	}
	snapshot := *current
	e.mu.Unlock()
	configuredFlow, ok := e.cfg.Flows[snapshot.flowName]
	if !ok {
		return failure.FingerprintInputs{}, false
	}
	for _, configuredStage := range configuredFlow.Stages {
		if configuredStage.Name == stage {
			return stageFailureFingerprintInputs(
				e, &snapshot, configuredStage, site, e.stageWorkdir(&snapshot, configuredStage),
			), true
		}
	}
	return failure.FingerprintInputs{}, false
}

func (e *Engine) recordClassifiedBoundaryFailure(ctx context.Context, issueID, stage string, primary error) error {
	if isCommittedInterruption(primary) {
		return primary
	}
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
	var policyErr *capability.PolicyError
	if errors.As(err, &policyErr) {
		switch policyErr.Reason {
		case capability.ReasonContractInvalid, capability.ReasonProviderUnsupported:
			return failure.SiteCapability, failure.ClassPolicy, failure.RetryAfterStateChange, failure.StateConfiguration
		case capability.ReasonRuntimeDenied:
			return failure.SiteCapability, failure.ClassPolicy, failure.RetryAfterStateChange, failure.StateTrustedWorkspace
		case capability.ReasonPostStageViolation:
			return failure.SiteCapability, failure.ClassPolicy, failure.RetryAfterStateChange, failure.StateTrustedWorkspace
		}
	}
	var retryableResult *stageResultRetryableError
	if errors.As(err, &retryableResult) {
		return failure.SiteLifecycle, failure.ClassExecution, failure.RetryNow, failure.StateRunnerInput
	}
	var resultPersistence *stageResultPersistenceError
	if errors.As(err, &resultPersistence) {
		return failure.SiteStore, failure.ClassUnavailable, failure.RetryNow, failure.StateStore
	}
	var lifecycleErr *stagelifecycle.DiagnosticError
	if errors.As(err, &lifecycleErr) {
		switch lifecycleErr.Code {
		case stagelifecycle.CodeCheckpointFinalization:
			return failure.SiteLifecycle, failure.ClassStateMismatch, failure.RetryAfterStateChange, failure.StateStore
		case stagelifecycle.CodeIntegrity, stagelifecycle.CodeConflict:
			return failure.SiteLifecycle, failure.ClassIntegrity, failure.RetryAfterStateChange, failure.StateStore
		case stagelifecycle.CodeMissingResult, stagelifecycle.CodeLegacyNormalization:
			return failure.SiteLifecycle, failure.ClassValidation, failure.RetryAfterStateChange, failure.StateStore
		}
	}
	var runnerErr *runnerStageError
	if errors.As(err, &runnerErr) {
		class := failure.NormalizeClass(failure.Class(runnerErr.Result.FailureClass))
		disposition, stateChange := runnerFailureRetryGuidance(class)
		return failure.SiteRunner, class, disposition, stateChange
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "artifacts require revision"):
		return failure.SiteArtifact, failure.ClassValidation, failure.RetryNow, failure.StateOperator
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
		fingerprint = e.authorizedFailureFingerprint(failureCtx.IssueID, failureCtx.Stage, site, class)
	}
	if fingerprint == "" {
		fingerprint = failure.BuildFingerprint(inputs)
	}
	stateVector := retry.BuildStateVector(inputs)
	record, err := e.cfg.FailureRecorder.AppendFailure(diagnosticCtx, failure.RecordInput{
		IssueID: failureCtx.IssueID, Stage: failureCtx.Stage, StageAttempt: failureCtx.StageAttempt,
		FailureSite: site, FailureClass: class, RetryDisposition: disposition,
		RequiredStateChange: stateChange, Fingerprint: fingerprint,
		StateVector: &stateVector,
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

func (e *Engine) authorizedFailureFingerprint(issueID, stage string, site failure.Site, class failure.Class) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	is, ok := e.issues[issueID]
	if !ok || is.retryFailure.stage != stage || is.retryFailure.site != site || is.retryFailure.class != class {
		return ""
	}
	return is.retryFailure.fingerprint
}
