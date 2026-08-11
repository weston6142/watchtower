package runtime

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/runner"
)

type StartRequest struct {
	Contract        capability.CompiledContract
	Plan            capability.EnforcementPlan
	Worktree        string
	Backend         Backend
	EnginePaths     []string
	EngineSockets   []string
	Environment     []string
	PlannerArtifact func(any) error
	Audit           func(capability.AuditRecord)
}

// ProviderProcessRequest describes the model-transport process owned by a
// capability session. The session fixes the working directory, containment
// plan, and scratch scope so adapters cannot launch outside the compiled
// workspace boundary.
type ProviderProcessRequest struct {
	Path        string
	Args        []string
	Plan        capability.EnforcementPlan
	Environment []string
	Stderr      io.Writer
	PipeStdin   bool
	PipeStdout  bool
}

type Session struct {
	contract        capability.CompiledContract
	plan            capability.EnforcementPlan
	worktree        string
	root            *os.Root
	backend         Backend
	scratch         string
	agentScratch    string
	providerScratch string
	environment     []string
	plannerArtifact func(any) error
	audit           func(capability.AuditRecord)
	gatewayEndpoint string
	gatewayListener net.Listener
	gatewayServer   *http.Server

	mu        sync.Mutex
	processes map[*runner.ProcessTree]bool
	closed    bool
}

func Start(_ context.Context, request StartRequest) (*Session, error) {
	if request.Backend == nil {
		return nil, unsupported("containment backend is unavailable")
	}
	backendPlan, err := request.Backend.Preflight(request.Contract)
	if err != nil {
		return nil, err
	}
	if request.Plan.PlanID == "" {
		request.Plan = backendPlan
	}
	if request.Plan.ContractID != request.Contract.ContractID || !equivalentControls(request.Plan, backendPlan) {
		return nil, unsupported("containment plan identity mismatch")
	}
	if err := runner.ValidateEnforcementPlan(request.Contract, request.Plan); err != nil {
		return nil, err
	}
	rootPath, err := filepath.EvalSymlinks(request.Worktree)
	if err != nil {
		return nil, unsupported("workspace identity is unavailable")
	}
	rootPath, err = filepath.Abs(rootPath)
	contractRoot, contractErr := filepath.EvalSymlinks(request.Contract.Contract.WorkspaceRoot)
	if contractErr == nil {
		contractRoot, contractErr = filepath.Abs(contractRoot)
	}
	if err != nil || contractErr != nil || rootPath != contractRoot {
		return nil, unsupported("workspace identity mismatch")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp("", "watchtower-capability-")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	if err := os.Chmod(scratch, 0o700); err != nil {
		_ = root.Close()
		_ = os.RemoveAll(scratch)
		return nil, err
	}
	agentScratch := filepath.Join(scratch, "agent")
	providerScratch := filepath.Join(scratch, "provider")
	for _, path := range []string{agentScratch, providerScratch} {
		if err := os.Mkdir(path, 0o700); err != nil {
			_ = root.Close()
			_ = os.RemoveAll(scratch)
			return nil, err
		}
	}
	session := &Session{
		contract: request.Contract, plan: request.Plan, worktree: rootPath, root: root,
		backend: request.Backend, scratch: scratch, agentScratch: agentScratch, providerScratch: providerScratch,
		environment:     scrubEnvironment(request.Environment),
		plannerArtifact: request.PlannerArtifact, audit: request.Audit, processes: make(map[*runner.ProcessTree]bool),
	}
	session.environment = append(session.environment, "TMPDIR="+agentScratch)
	if err := session.startGateway(); err != nil {
		_ = root.Close()
		_ = os.RemoveAll(scratch)
		return nil, err
	}
	session.record("launch", "passed", "", "", nil)
	return session, nil
}

func equivalentControls(adapter, backend capability.EnforcementPlan) bool {
	if len(adapter.Controls) != len(backend.Controls) {
		return false
	}
	proofs := make(map[capability.EnforcementControl]bool, len(backend.Controls))
	for _, proof := range backend.Controls {
		proofs[proof.Control] = proof.Proven
	}
	for _, proof := range adapter.Controls {
		if !proof.Proven || !proofs[proof.Control] {
			return false
		}
	}
	return true
}

func (s *Session) ScratchRoot() string {
	if s == nil {
		return ""
	}
	return s.scratch
}

func (s *Session) ProviderWorkdir() string {
	if s == nil {
		return ""
	}
	return s.providerScratch
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	processes := make([]*runner.ProcessTree, 0, len(s.processes))
	for process := range s.processes {
		processes = append(processes, process)
	}
	s.mu.Unlock()
	for _, process := range processes {
		_ = process.TerminateAndWait(250 * time.Millisecond)
	}
	gatewayErr := s.stopGateway()
	rootErr := s.root.Close()
	removeErr := os.RemoveAll(s.scratch)
	s.record("reap", "passed", "", "", nil)
	if rootErr != nil {
		return rootErr
	}
	if gatewayErr != nil {
		return gatewayErr
	}
	return removeErr
}

// StartProvider launches the provider transport through the same immutable
// containment plan used for mediated operations. Provider transport may use
// the network; descendant agent operations remain separately routed through
// the network-denied gateway methods.
func (s *Session) StartProvider(ctx context.Context, request ProviderProcessRequest) (*runner.ProcessTree, error) {
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	if request.Plan.ContractID != s.plan.ContractID || request.Plan.PlanID != s.plan.PlanID {
		return nil, unsupported("provider launch plan identity mismatch")
	}
	path := request.Path
	if s.plan.Provider == "codex" {
		if err := s.prepareCodexAuth(request.Environment); err != nil {
			return nil, err
		}
	}
	environment := scrubProviderEnvironment(request.Environment, s.plan.Provider)
	replacements := []string{
		"TMPDIR=" + s.providerScratch,
		"HOME=" + s.providerScratch,
		"WATCHTOWER_CAPABILITY_GATEWAY_URL=" + s.gatewayEndpoint,
	}
	if s.plan.Provider == "codex" {
		replacements = append(replacements, "CODEX_HOME="+filepath.Join(s.providerScratch, ".codex"))
	}
	environment = replaceEnvironment(environment, replacements...)
	if !filepath.IsAbs(path) {
		resolved, err := resolveExecutable(path, environment)
		if err != nil {
			return nil, unsupported("provider executable is unavailable")
		}
		path = resolved
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, unsupported("provider executable identity is unavailable")
	}
	path, err = filepath.Abs(canonicalPath)
	if err != nil {
		return nil, unsupported("provider executable identity is unavailable")
	}
	providerReads, providerExecutables := providerRuntimePaths(path, environment)
	contained, err := s.backend.Wrap(ProcessRequest{
		Contract: s.contract, Plan: s.plan, Mode: ModeProviderTransport,
		Path: path, Args: append([]string(nil), request.Args...), Dir: s.providerScratch,
		Env: environment, Scratch: s.providerScratch,
		ProviderReads: providerReads, ProviderExecutables: providerExecutables,
	})
	if err != nil {
		return nil, err
	}
	process, err := runner.StartProcessTree(ctx, runner.ProcessSpec{
		Path: contained.Path, Args: contained.Args, Dir: contained.Dir, Env: contained.Env,
		Stderr: request.Stderr, PipeStdin: request.PipeStdin, PipeStdout: request.PipeStdout,
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = process.TerminateAndWait(250 * time.Millisecond)
		return nil, unsupported("runtime session is closed")
	}
	s.processes[process] = true
	s.mu.Unlock()
	s.record("provider-launch", "passed", "", "", nil)
	return process, nil
}

func (s *Session) ensureOpen() error {
	if s == nil || s.root == nil {
		return unsupported("runtime session is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return unsupported("runtime session is closed")
	}
	return nil
}

func (s *Session) deny(operation capability.OperationClass, paths ...string) error {
	canonical := make([]string, 0, len(paths))
	for _, path := range paths {
		if path != "" {
			canonical = append(canonical, filepath.ToSlash(path))
		}
	}
	s.record("runtime", "denied", capability.ReasonRuntimeDenied, operation, canonical)
	s.terminateProcesses()
	return &capability.PolicyError{Phase: "runtime", Reason: capability.ReasonRuntimeDenied, Operation: operation, Paths: canonical, Diagnostic: "operation is outside compiled contract"}
}

func (s *Session) terminateProcesses() {
	if s == nil {
		return
	}
	s.mu.Lock()
	processes := make([]*runner.ProcessTree, 0, len(s.processes))
	for process := range s.processes {
		processes = append(processes, process)
	}
	s.mu.Unlock()
	for _, process := range processes {
		_ = process.TerminateAndWait(250 * time.Millisecond)
	}
}

func (s *Session) record(phase, outcome string, reason capability.FailureReason, operation capability.OperationClass, paths []string) {
	if s == nil || s.audit == nil {
		return
	}
	s.audit(capability.AuditRecord{
		Attempt: capability.AttemptIdentity{
			IssueID: s.contract.Contract.IssueID, Stage: s.contract.Contract.Stage, AttemptID: s.contract.Contract.AttemptID,
		},
		ContractID: s.contract.ContractID, Phase: phase, Outcome: outcome, Reason: reason,
		Provider: s.plan.Provider, Implementation: s.plan.Implementation, Operation: operation, Paths: append([]string(nil), paths...),
	})
}

func unsupported(diagnostic string) error {
	return &capability.PolicyError{Phase: "preflight", Reason: capability.ReasonProviderUnsupported, Diagnostic: diagnostic}
}

func scrubEnvironment(values []string) []string {
	result := make([]string, 0, len(values)+1)
	hasPath := false
	for _, value := range values {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			continue
		}
		lower := strings.ToLower(key)
		if key == "TMPDIR" {
			continue
		}
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") ||
			strings.Contains(lower, "credential") || strings.Contains(lower, "ssh") || strings.Contains(lower, "gpg") || strings.HasPrefix(lower, "watchtower_") {
			continue
		}
		hasPath = hasPath || key == "PATH"
		result = append(result, value)
	}
	if !hasPath {
		result = append(result, "PATH=/usr/bin:/bin")
	}
	return result
}

func scrubProviderEnvironment(values []string, provider string) []string {
	result := make([]string, 0, len(values)+1)
	hasPath := false
	for _, value := range values {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" || key == "TMPDIR" {
			continue
		}
		switch strings.ToUpper(key) {
		case "SSH_AUTH_SOCK", "SSH_AGENT_PID", "GIT_ASKPASS", "SSH_ASKPASS", "GPG_AGENT_INFO":
			continue
		}
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "WATCHTOWER_") || strings.HasPrefix(upper, "VERIFICATION_") {
			continue
		}
		lower := strings.ToLower(key)
		providerCredential := (provider == "codex" && upper == "OPENAI_API_KEY") ||
			(provider == "claude" && upper == "ANTHROPIC_API_KEY")
		foreignProviderCredential := (upper == "OPENAI_API_KEY" || upper == "ANTHROPIC_API_KEY") && !providerCredential
		cloudCredential := foreignProviderCredential || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") ||
			strings.Contains(lower, "credential") || strings.HasPrefix(upper, "AWS_") || strings.HasPrefix(upper, "GOOGLE_") ||
			strings.HasPrefix(upper, "AZURE_") || strings.HasPrefix(upper, "GITHUB_") || strings.HasPrefix(upper, "GH_") ||
			strings.HasPrefix(upper, "VERCEL_") || strings.HasPrefix(upper, "CLOUDFLARE_") || strings.HasPrefix(upper, "NPM_")
		if cloudCredential && !providerCredential {
			continue
		}
		hasPath = hasPath || key == "PATH"
		result = append(result, value)
	}
	if !hasPath {
		result = append(result, "PATH=/usr/bin:/bin")
	}
	return result
}

func replaceEnvironment(values []string, replacements ...string) []string {
	replace := make(map[string]string, len(replacements))
	order := make([]string, 0, len(replacements))
	for _, value := range replacements {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			continue
		}
		if _, exists := replace[key]; !exists {
			order = append(order, key)
		}
		replace[key] = value
	}
	result := make([]string, 0, len(values)+len(order))
	for _, value := range values {
		key, _, ok := strings.Cut(value, "=")
		if ok {
			if _, exists := replace[key]; exists {
				continue
			}
		}
		result = append(result, value)
	}
	for _, key := range order {
		result = append(result, replace[key])
	}
	return result
}

func (s *Session) prepareCodexAuth(environment []string) error {
	home := environmentValue(environment, "CODEX_HOME")
	if home == "" {
		if userHome := environmentValue(environment, "HOME"); userHome != "" {
			home = filepath.Join(userHome, ".codex")
		}
	}
	if home == "" {
		return nil
	}
	body, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return unsupported("provider authentication is unavailable")
	}
	destination := filepath.Join(s.providerScratch, ".codex")
	if err := os.Mkdir(destination, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	if err := os.WriteFile(filepath.Join(destination, "auth.json"), body, 0o600); err != nil {
		return err
	}
	return nil
}

func environmentValue(environment []string, want string) string {
	for index := len(environment) - 1; index >= 0; index-- {
		key, value, ok := strings.Cut(environment[index], "=")
		if ok && key == want {
			return value
		}
	}
	return ""
}

func providerRuntimePaths(path string, environment []string) ([]string, []string) {
	reads := []string{path}
	executables := []string{path}
	body, err := os.ReadFile(path)
	if err != nil {
		return reads, executables
	}
	line, _, _ := strings.Cut(string(body), "\n")
	if !strings.HasPrefix(line, "#!") {
		return reads, executables
	}
	fields := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(fields) == 0 || !filepath.IsAbs(fields[0]) {
		return reads, executables
	}
	interpreter := fields[0]
	if filepath.Base(interpreter) == "env" && len(fields) > 1 {
		executables = appendCanonicalPath(executables, interpreter)
		reads = appendCanonicalPath(reads, interpreter)
		if resolved, resolveErr := resolveExecutable(fields[1], environment); resolveErr == nil {
			interpreter = resolved
		}
	}
	executables = appendCanonicalPath(executables, interpreter)
	reads = appendCanonicalPath(reads, interpreter)
	if interpreter == "/bin/sh" {
		executables = appendCanonicalPath(executables, "/bin/bash")
		reads = appendCanonicalPath(reads, "/bin/bash")
	}
	if filepath.Base(interpreter) == "node" {
		if root := nodeModulesRoot(path); root != "" {
			reads = append(reads, root)
			count := 0
			_ = filepath.WalkDir(root, func(candidate string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil || count > 10000 {
					return filepath.SkipDir
				}
				count++
				if entry.IsDir() || entry.Name() != "codex" {
					return nil
				}
				if info, statErr := entry.Info(); statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
					executables = appendCanonicalPath(executables, candidate)
				}
				return nil
			})
		}
	}
	return reads, executables
}

func appendCanonicalPath(paths []string, path string) []string {
	canonical, err := filepath.EvalSymlinks(path)
	if err == nil {
		path, err = filepath.Abs(canonical)
	}
	if err != nil || path == "" {
		return paths
	}
	for _, existing := range paths {
		if existing == path {
			return paths
		}
	}
	return append(paths, path)
}

func nodeModulesRoot(path string) string {
	clean := filepath.Clean(path)
	separator := string(filepath.Separator)
	marker := separator + "node_modules" + separator
	index := strings.Index(clean, marker)
	if index < 0 {
		return ""
	}
	return clean[:index+len(marker)-1]
}

func (s *Session) PlannerApply(value any) error {
	if !s.hasOperation(capability.OpPlannerArtifactApply) || s.plannerArtifact == nil {
		return s.deny(capability.OpPlannerArtifactApply)
	}
	if err := s.plannerArtifact(value); err != nil {
		return fmt.Errorf("planner artifact apply: %w", err)
	}
	s.record("runtime", "passed", "", capability.OpPlannerArtifactApply, nil)
	return nil
}

func (s *Session) hasOperation(want capability.OperationClass) bool {
	for _, operation := range s.contract.Contract.Operations {
		if operation == want {
			return true
		}
	}
	return false
}
