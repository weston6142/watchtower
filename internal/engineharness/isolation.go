package engineharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/recoverymatrix"
	"github.com/weston6142/watchtower/internal/runner"
)

const workerEnvironment = "WATCHTOWER_ENGINE_HARNESS_WORKER"

type workerRequest struct {
	ProductionFlow flow.Flow
	Scenario       recoverymatrix.Scenario
	Root           string
}

type workerResponse struct {
	Observation        recoverymatrix.Observation
	ExecuteError       string
	InfrastructureKind recoverymatrix.InfrastructureErrorKind
	VerifyError        string
	CleanupError       string
	Panic              string
}

type processExecutor struct {
	request workerRequest

	mu       sync.Mutex
	tree     *runner.ProcessTree
	started  chan struct{}
	response workerResponse
}

func (f *EnvironmentFactory) New(ctx context.Context, scenario recoverymatrix.Scenario) (recoverymatrix.ScenarioExecutor, func() error, error) {
	if f == nil || len(f.ProductionFlow.Stages) == 0 {
		return nil, nil, fmt.Errorf("production flow is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := validateDriver(scenario); err != nil {
		return nil, nil, err
	}
	if err := validateCrossCutInputs(scenario); err != nil {
		return nil, nil, err
	}
	root, err := os.MkdirTemp("", "watchtower-matrix-")
	if err != nil {
		return nil, nil, fmt.Errorf("create scenario isolation: %w", err)
	}
	executor := &processExecutor{
		request: workerRequest{ProductionFlow: cloneFlow(f.ProductionFlow), Scenario: scenario, Root: root},
		started: make(chan struct{}),
	}
	cleanup := func() error {
		executor.mu.Lock()
		workerCleanup := executor.response.CleanupError
		executor.mu.Unlock()
		removeErr := os.RemoveAll(root)
		if workerCleanup != "" {
			return errors.Join(errors.New(workerCleanup), removeErr)
		}
		return removeErr
	}
	return executor, cleanup, nil
}

func (e *processExecutor) Execute(ctx context.Context, _ recoverymatrix.Scenario) (recoverymatrix.Observation, error) {
	request, err := json.Marshal(e.request)
	if err != nil {
		return recoverymatrix.Observation{}, fmt.Errorf("encode scenario worker request: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return recoverymatrix.Observation{}, fmt.Errorf("resolve scenario worker: %w", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	tree, err := runner.StartProcessTree(context.Background(), runner.ProcessSpec{
		Path:   executable,
		Env:    append(os.Environ(), workerEnvironment+"=1"),
		Stdin:  bytes.NewReader(request),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		e.mu.Lock()
		close(e.started)
		e.mu.Unlock()
		return recoverymatrix.Observation{}, fmt.Errorf("start scenario worker: %w", err)
	}
	e.mu.Lock()
	e.tree = tree
	close(e.started)
	e.mu.Unlock()
	if err := tree.Wait(); err != nil {
		if ctx.Err() != nil {
			return recoverymatrix.Observation{}, ctx.Err()
		}
		return recoverymatrix.Observation{}, recoverymatrix.InfrastructureError{
			Kind: recoverymatrix.InfrastructureFixture,
			Err:  fmt.Errorf("scenario worker failed: %w: %s", err, stderr.String()),
		}
	}
	var response workerResponse
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return recoverymatrix.Observation{}, recoverymatrix.InfrastructureError{
			Kind: recoverymatrix.InfrastructureFixture,
			Err:  fmt.Errorf("decode scenario worker response: %w: %s", err, stderr.String()),
		}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return recoverymatrix.Observation{}, recoverymatrix.InfrastructureError{
			Kind: recoverymatrix.InfrastructureFixture, Err: fmt.Errorf("decode scenario worker response: trailing value"),
		}
	}
	e.mu.Lock()
	e.response = response
	e.mu.Unlock()
	if response.Panic != "" {
		panic(response.Panic)
	}
	if response.ExecuteError != "" {
		err := errors.New(response.ExecuteError)
		if response.InfrastructureKind != "" {
			err = recoverymatrix.InfrastructureError{Kind: response.InfrastructureKind, Err: err}
		}
		return response.Observation, err
	}
	return response.Observation, nil
}

func (e *processExecutor) VerifyConsumed() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.response.VerifyError == "" {
		return nil
	}
	return errors.New(e.response.VerifyError)
}

func (e *processExecutor) Terminate(ctx context.Context) error {
	select {
	case <-e.started:
	case <-ctx.Done():
		return ctx.Err()
	}
	e.mu.Lock()
	tree := e.tree
	e.mu.Unlock()
	if tree == nil {
		return nil
	}
	terminated := make(chan error, 1)
	go func() {
		terminated <- tree.EnsureReaped(runner.DefaultTerminationGrace)
	}()
	select {
	case err := <-terminated:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func WorkerRequested() bool {
	return os.Getenv(workerEnvironment) == "1"
}

func RunWorker(input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	var request workerRequest
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode worker request: %w", err)
	}
	response := executeWorker(request)
	return json.NewEncoder(output).Encode(response)
}

func executeWorker(request workerRequest) (response workerResponse) {
	factory := &EnvironmentFactory{ProductionFlow: request.ProductionFlow}
	executor, cleanup, err := factory.newInProcess(context.Background(), request.Scenario, request.Root)
	if err != nil {
		response.ExecuteError = err.Error()
		response.InfrastructureKind = recoverymatrix.InfrastructureFixture
		return response
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			response.Panic = fmt.Sprint(recovered)
		}
		if err := cleanup(); err != nil {
			response.CleanupError = err.Error()
		}
	}()
	response.Observation, err = executor.Execute(context.Background(), request.Scenario)
	if err != nil {
		response.ExecuteError = err.Error()
		var infrastructure recoverymatrix.InfrastructureError
		if errors.As(err, &infrastructure) {
			response.InfrastructureKind = infrastructure.Kind
		}
		return response
	}
	if err := executor.VerifyConsumed(); err != nil {
		response.VerifyError = err.Error()
	}
	return response
}

var _ recoverymatrix.ScenarioExecutor = (*processExecutor)(nil)
