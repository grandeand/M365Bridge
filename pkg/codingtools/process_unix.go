//go:build !windows

package codingtools

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "/bin/sh", "-c", command)
}

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return errors.New("process not started")
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func terminateProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
