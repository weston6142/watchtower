//go:build unix

package runner

import (
	"fmt"
	"io"
	"os/exec"
	"syscall"
)

func startProcessTree(spec ProcessSpec) (*ProcessTree, error) {
	if spec.Path == "" {
		return nil, fmt.Errorf("process path is required")
	}
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir, cmd.Env = spec.Dir, append([]string(nil), spec.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	var stdin io.WriteCloser
	var stdout io.ReadCloser
	var err error
	if spec.PipeStdin {
		if spec.Stdin != nil {
			return nil, fmt.Errorf("process stdin and piped stdin are mutually exclusive")
		}
		stdin, err = cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
	}
	if spec.PipeStdout {
		if spec.Stdout != nil {
			return nil, fmt.Errorf("process stdout and piped stdout are mutually exclusive")
		}
		stdout, err = cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	tree := newProcessTree(cmd, func(force bool) error {
		signal := syscall.SIGTERM
		if force {
			signal = syscall.SIGKILL
		}
		if err := syscall.Kill(-pid, signal); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("terminate process group: %w", err)
		}
		return nil
	})
	tree.stdin, tree.stdout = stdin, stdout
	return tree, nil
}
