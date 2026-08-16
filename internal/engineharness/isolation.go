package engineharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/weston6142/watchtower/internal/flow"
	"github.com/weston6142/watchtower/internal/recoverymatrix"
	"github.com/weston6142/watchtower/internal/runner"
)

const workerEnvironment = "WATCHTOWER_ENGINE_HARNESS_WORKER"

const (
	workerCapabilityDescriptor = 3
	workerRootEnvironment      = "WATCHTOWER_ENGINE_HARNESS_ROOT"
	workerCapabilityMarker     = ".watchtower-worker-capability"
)

type workerRequest struct {
	ProductionFlow flow.Flow
	Scenario       recoverymatrix.Scenario
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
	root     *os.File
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
	rootCapability, err := os.Open(root)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, nil, fmt.Errorf("open scenario isolation capability: %w", err)
	}
	executor := &processExecutor{
		request: workerRequest{ProductionFlow: cloneFlow(f.ProductionFlow), Scenario: scenario},
		root:    rootCapability,
		started: make(chan struct{}),
	}
	cleanup := func() error {
		executor.mu.Lock()
		workerCleanup := executor.response.CleanupError
		executor.mu.Unlock()
		closeErr := rootCapability.Close()
		removeErr := os.RemoveAll(root)
		if workerCleanup != "" {
			return errors.Join(errors.New(workerCleanup), closeErr, removeErr)
		}
		return errors.Join(closeErr, removeErr)
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
		Path: executable,
		Env: append(os.Environ(),
			fmt.Sprintf("%s=%d", workerEnvironment, workerCapabilityDescriptor),
			workerRootEnvironment+"="+e.root.Name(),
		),
		Stdin:  bytes.NewReader(request),
		Stdout: &stdout,
		Stderr: &stderr,
		ExtraFiles: []*os.File{
			e.root,
		},
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
	return os.Getenv(workerEnvironment) == fmt.Sprint(workerCapabilityDescriptor)
}

func RunWorker(input io.Reader, output io.Writer) error {
	root, err := consumeWorkerRootCapability()
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	var request workerRequest
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode worker request: %w", err)
	}
	response := executeWorker(request, root)
	return json.NewEncoder(output).Encode(response)
}

func consumeWorkerRootCapability() (string, error) {
	root := os.Getenv(workerRootEnvironment)
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", fmt.Errorf("scenario worker root capability is invalid")
	}
	if filepath.Dir(root) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(root), "watchtower-matrix-") {
		return "", fmt.Errorf("scenario worker root is outside the matrix temporary directory")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("scenario worker root is not a private directory")
	}
	capability := os.NewFile(workerCapabilityDescriptor, "watchtower-worker-root")
	if capability == nil {
		return "", fmt.Errorf("scenario worker root capability is unavailable")
	}
	defer capability.Close()
	capabilityInfo, err := capability.Stat()
	if err != nil || !capabilityInfo.IsDir() || !os.SameFile(rootInfo, capabilityInfo) {
		return "", fmt.Errorf("scenario worker root capability does not match the requested directory")
	}
	entries, err := capability.ReadDir(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("inspect scenario worker root: %w", err)
	}
	if len(entries) != 0 {
		return "", fmt.Errorf("scenario worker root is not fresh")
	}
	marker, err := os.OpenFile(filepath.Join(root, workerCapabilityMarker), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("consume scenario worker root capability: %w", err)
	}
	if err := marker.Close(); err != nil {
		return "", fmt.Errorf("consume scenario worker root capability: %w", err)
	}
	return root, nil
}

func executeWorker(request workerRequest, root string) (response workerResponse) {
	factory := &EnvironmentFactory{ProductionFlow: request.ProductionFlow}
	executor, cleanup, err := factory.newInProcess(context.Background(), request.Scenario, root)
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
