package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/weston6142/watchtower/internal/capability"
	"github.com/weston6142/watchtower/internal/runner"
)

func (s *Session) Run(ctx context.Context, argv []string) ([]byte, error) {
	if len(argv) == 0 || !s.hasOperation(capability.OpLocalProcess) || deniedExecutable(argv) {
		return nil, s.deny(capability.OpLocalProcess)
	}
	if err := s.ensureOpen(); err != nil {
		return nil, err
	}
	path := argv[0]
	if !filepath.IsAbs(path) {
		resolved, err := resolveExecutable(path, s.environment)
		if err != nil {
			return nil, s.deny(capability.OpLocalProcess)
		}
		path = resolved
	}
	request, err := s.backend.Wrap(ProcessRequest{
		Contract: s.contract, Plan: s.plan, Path: path, Args: append([]string(nil), argv[1:]...),
		Dir: s.worktree, Env: append([]string(nil), s.environment...), Scratch: s.scratch,
	})
	if err != nil {
		return nil, err
	}
	output := boundedOutput{remaining: 1 << 20}
	process, err := runner.StartProcessTree(ctx, runner.ProcessSpec{
		Path: request.Path, Args: request.Args, Dir: request.Dir, Env: request.Env,
		Stdout: &output, Stderr: &output,
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.processes[process] = true
	s.mu.Unlock()
	err = process.Wait()
	s.mu.Lock()
	delete(s.processes, process)
	s.mu.Unlock()
	if err != nil {
		return nil, s.deny(capability.OpLocalProcess)
	}
	s.record("runtime", "passed", "", capability.OpLocalProcess, nil)
	return output.Bytes(), nil
}

type boundedOutput struct {
	bytes.Buffer
	remaining int
}

func (b *boundedOutput) Write(value []byte) (int, error) {
	original := len(value)
	if len(value) > b.remaining {
		value = value[:b.remaining]
	}
	if len(value) > 0 {
		_, _ = b.Buffer.Write(value)
		b.remaining -= len(value)
	}
	return original, nil
}

func deniedExecutable(argv []string) bool {
	base := strings.ToLower(filepath.Base(argv[0]))
	switch base {
	case "sh", "bash", "zsh", "fish", "dash", "curl", "wget", "ssh", "scp", "nc", "netcat", "git":
		return true
	}
	return false
}

func resolveExecutable(name string, environment []string) (string, error) {
	pathValue := "/usr/bin:/bin"
	for _, value := range environment {
		if strings.HasPrefix(value, "PATH=") {
			pathValue = strings.TrimPrefix(value, "PATH=")
			break
		}
	}
	for _, directory := range filepath.SplitList(pathValue) {
		candidate := filepath.Join(directory, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable unavailable")
}
