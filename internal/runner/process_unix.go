//go:build unix

package runner

import (
	"fmt"
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	return newProcessTree(cmd, func(force bool) error {
		signal := syscall.SIGTERM
		if force {
			signal = syscall.SIGKILL
		}
		if err := syscall.Kill(-pid, signal); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("terminate process group: %w", err)
		}
		return nil
	}), nil
}
