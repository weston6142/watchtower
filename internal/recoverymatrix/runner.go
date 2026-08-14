package recoverymatrix

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

const defaultScenarioTimeout = 30 * time.Second

// ScenarioExecutor drives one scenario in one freshly allocated environment.
type ScenarioExecutor interface {
	Execute(context.Context, Scenario) (Observation, error)
	VerifyConsumed() error
}

// EnvironmentFactory constructs an isolated executor for each scenario.
type EnvironmentFactory interface {
	New(context.Context, Scenario) (ScenarioExecutor, func() error, error)
}

type RunOptions struct {
	ManifestIdentity string
	Revision         string
	ScenarioID       string
	ScenarioTimeout  time.Duration
}

type InfrastructureErrorKind string

const (
	InfrastructureUnexpectedCall InfrastructureErrorKind = "unexpected-call"
	InfrastructureUnconsumed     InfrastructureErrorKind = "unconsumed-script"
	InfrastructureFixture        InfrastructureErrorKind = "fixture"
	InfrastructureCleanup        InfrastructureErrorKind = "cleanup"
)

// InfrastructureError distinguishes harness defects from observable workflow
// outcomes. Expected workflow failures are returned as observations instead.
type InfrastructureError struct {
	Kind InfrastructureErrorKind
	Err  error
}

func (e InfrastructureError) Error() string {
	if e.Err == nil {
		return string(e.Kind)
	}
	return fmt.Sprintf("%s: %v", e.Kind, e.Err)
}

func (e InfrastructureError) Unwrap() error { return e.Err }

// Run executes scenarios serially in fresh environments. A selected scenario is
// diagnostic evidence only and is always marked filtered and partial.
func Run(ctx context.Context, inventory []Scenario, factory EnvironmentFactory, options RunOptions) RunSummary {
	summary := RunSummary{
		ManifestIdentity: options.ManifestIdentity,
		Revision:         options.Revision,
		Compiled:         len(inventory),
		Filtered:         strings.TrimSpace(options.ScenarioID) != "",
	}
	deadline := options.ScenarioTimeout
	if deadline <= 0 {
		deadline = defaultScenarioTimeout
	}

	for _, scenario := range inventory {
		if summary.Filtered && scenario.ID != options.ScenarioID {
			continue
		}
		summary.Executed++
		result, counters := runScenario(ctx, scenario, factory, deadline)
		summary.Results = append(summary.Results, result)
		summary.Panics += counters.panics
		summary.Timeouts += counters.timeouts
		summary.UnexpectedCalls += counters.unexpectedCalls
		summary.UnconsumedScripts += counters.unconsumedScripts
		summary.MissingResults += counters.missingResults
		switch result.Status {
		case ResultPassed:
			summary.Passed++
		case ResultSkipped:
			summary.Skipped++
		default:
			summary.Failed++
		}
	}

	summary.Partial = summary.Filtered || summary.Executed != summary.Compiled
	if summary.Filtered && summary.Executed == 0 {
		summary.MissingResults++
	}
	return summary
}

type runCounters struct {
	panics            int
	timeouts          int
	unexpectedCalls   int
	unconsumedScripts int
	missingResults    int
}

func runScenario(parent context.Context, scenario Scenario, factory EnvironmentFactory, timeout time.Duration) (result ScenarioResult, counters runCounters) {
	result = ScenarioResult{ScenarioID: scenario.ID, Status: ResultFailed, FailureClass: FailureHarnessInfrastructure}
	if factory == nil {
		counters.missingResults++
		result.Failure = "environment factory is nil"
		return result, counters
	}

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	executor, cleanup, err := factory.New(ctx, scenario)
	if err != nil {
		counters.missingResults++
		result.Failure = fmt.Sprintf("construct environment: %v", err)
		return result, counters
	}
	if cleanup == nil {
		cleanup = func() error { return nil }
	}
	if executor == nil {
		counters.missingResults++
		result.Failure = "environment returned a nil executor"
		if err := cleanup(); err != nil {
			result.Failure += fmt.Sprintf("; cleanup: %v", err)
		}
		return result, counters
	}

	observation, executeErr, panicValue := executeSafely(ctx, executor, scenario)
	result.Observation = normalizeObservation(observation)
	if panicValue != nil {
		counters.panics++
		result.Failure = fmt.Sprintf("scenario panic: %v", panicValue)
	} else if executeErr != nil {
		result.Failure = executeErr.Error()
		if errors.Is(executeErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			counters.timeouts++
		}
		var infrastructure InfrastructureError
		if errors.As(executeErr, &infrastructure) && infrastructure.Kind == InfrastructureUnexpectedCall {
			counters.unexpectedCalls++
		}
	} else if err := executor.VerifyConsumed(); err != nil {
		counters.unconsumedScripts++
		result.Failure = InfrastructureError{Kind: InfrastructureUnconsumed, Err: err}.Error()
	} else if err := EvaluateContract(scenario, result.Observation); err != nil {
		result.FailureClass = FailureWorkflowContract
		result.Failure = err.Error()
	} else {
		result.Status = ResultPassed
		result.FailureClass = ""
	}

	if err := cleanup(); err != nil {
		if result.Status == ResultPassed {
			result.Status = ResultFailed
			result.FailureClass = FailureHarnessInfrastructure
			result.Failure = InfrastructureError{Kind: InfrastructureCleanup, Err: err}.Error()
		} else {
			result.Failure += fmt.Sprintf("; cleanup: %v", err)
		}
	}
	return result, counters
}

func executeSafely(ctx context.Context, executor ScenarioExecutor, scenario Scenario) (observation Observation, err error, panicValue any) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicValue = recovered
		}
	}()
	observation, err = executor.Execute(ctx, scenario)
	return observation, err, nil
}

func normalizeObservation(observation Observation) Observation {
	observation.ArtifactIdentities = sortedCopy(observation.ArtifactIdentities)
	observation.Effects = sortedCopy(observation.Effects)
	if len(observation.DiagnosticCheckpoints) > 20 {
		observation.DiagnosticCheckpoints = append([]string(nil), observation.DiagnosticCheckpoints[len(observation.DiagnosticCheckpoints)-20:]...)
	} else {
		observation.DiagnosticCheckpoints = append([]string(nil), observation.DiagnosticCheckpoints...)
	}
	return observation
}

// EvaluateContract compares only observable production behavior. Diagnostic
// checkpoints deliberately do not participate in pass/fail evaluation.
func EvaluateContract(scenario Scenario, actual Observation) error {
	if actual.PublicOutcome != scenario.Expected.PublicOutcome {
		return contractMismatch(scenario.ID, "public outcome", scenario.Expected.PublicOutcome, actual.PublicOutcome)
	}
	if actual.DurableState != scenario.Expected.DurableState {
		return contractMismatch(scenario.ID, "durable state", scenario.Expected.DurableState, actual.DurableState)
	}
	if actual.NormalizedClassification != scenario.Expected.NormalizedClassification {
		return contractMismatch(scenario.ID, "classification", scenario.Expected.NormalizedClassification, actual.NormalizedClassification)
	}
	if !slices.Equal(sortedCopy(actual.ArtifactIdentities), sortedCopy(scenario.Expected.ArtifactIdentities)) {
		return contractMismatch(scenario.ID, "artifact identities", scenario.Expected.ArtifactIdentities, actual.ArtifactIdentities)
	}
	if duplicate, ok := firstDuplicate(actual.Effects); ok {
		return fmt.Errorf("scenario %q effects contain duplicate %q", scenario.ID, duplicate)
	}
	if !slices.Equal(sortedCopy(actual.Effects), sortedCopy(scenario.AllowedEffects)) {
		return contractMismatch(scenario.ID, "effects", scenario.AllowedEffects, actual.Effects)
	}
	return nil
}

func contractMismatch(scenarioID, field string, expected, actual any) error {
	return fmt.Errorf("scenario %q %s mismatch: expected %v, actual %v", scenarioID, field, expected, actual)
}

func firstDuplicate(values []string) (string, bool) {
	copyOfValues := append([]string(nil), values...)
	sort.Strings(copyOfValues)
	for index := 1; index < len(copyOfValues); index++ {
		if copyOfValues[index] == copyOfValues[index-1] {
			return copyOfValues[index], true
		}
	}
	return "", false
}
