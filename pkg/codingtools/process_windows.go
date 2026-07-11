//go:build windows

package codingtools

import (
	"context"
	"os/exec"
	"strconv"
)

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "cmd.exe", "/d", "/s", "/c", command)
}

func configureProcess(cmd *exec.Cmd) {}

func terminateProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
	}
}
