package runner

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"time"
)

type ProcessSpec struct {
	Path       string
	Args       []string
	Dir        string
	Env        []string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	PipeStdin  bool
	PipeStdout bool
}

type ProcessTree struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	done      chan struct{}
	waitErr   error
	waitOnce  sync.Once
	termMu    sync.Mutex
	terminate func(force bool) error
}

func StartProcessTree(ctx context.Context, spec ProcessSpec) (*ProcessTree, error) {
	tree, err := startProcessTree(spec)
	if err != nil {
		return nil, err
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = tree.TerminateAndWait(250 * time.Millisecond)
		case <-tree.done:
		}
	}()
	return tree, nil
}

func newProcessTree(cmd *exec.Cmd, terminate func(force bool) error) *ProcessTree {
	tree := &ProcessTree{cmd: cmd, done: make(chan struct{}), terminate: terminate}
	go func() {
		tree.waitErr = cmd.Wait()
		close(tree.done)
	}()
	return tree
}

// StdinPipe returns the provider input stream requested by ProcessSpec. The
// pipe belongs to the complete process tree and must be closed before waiting
// for a provider that consumes streaming input.
func (p *ProcessTree) StdinPipe() io.WriteCloser {
	if p == nil {
		return nil
	}
	return p.stdin
}

// StdoutPipe returns the provider output stream requested by ProcessSpec.
func (p *ProcessTree) StdoutPipe() io.ReadCloser {
	if p == nil {
		return nil
	}
	return p.stdout
}

func (p *ProcessTree) Wait() error {
	if p == nil || p.done == nil {
		return nil
	}
	<-p.done
	return p.waitErr
}

func (p *ProcessTree) TerminateAndWait(grace time.Duration) error {
	if p == nil {
		return nil
	}
	p.termMu.Lock()
	defer p.termMu.Unlock()
	select {
	case <-p.done:
		return p.waitErr
	default:
	}
	if p.terminate != nil {
		_ = p.terminate(false)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-p.done:
		return p.waitErr
	case <-timer.C:
		if p.terminate != nil {
			_ = p.terminate(true)
		}
		return p.Wait()
	}
}
