//go:build unix

package runner

import (
	"fmt"
	"io"
	"os"
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
	cmd.ExtraFiles = append([]*os.File(nil), spec.ExtraFiles...)
	var stdin io.WriteCloser
	var stdout io.ReadCloser
	var stdoutWriter *os.File
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
		stdout, stdoutWriter, err = os.Pipe()
		if err != nil {
			return nil, err
		}
		cmd.Stdout = stdoutWriter
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		if stdout != nil {
			_ = stdout.Close()
		}
		if stdoutWriter != nil {
			_ = stdoutWriter.Close()
		}
		return nil, err
	}
	if stdoutWriter != nil {
		_ = stdoutWriter.Close()
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
	tree.groupAlive = func() bool {
		err := syscall.Kill(-pid, 0)
		return err == nil || err == syscall.EPERM
	}
	tree.stdin, tree.stdout = stdin, stdout
	return tree, nil
}
