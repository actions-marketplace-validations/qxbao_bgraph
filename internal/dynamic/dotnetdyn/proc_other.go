//go:build !windows

package dotnetdyn

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the command in a process group of its own, so that killTree reaches its children.
func setProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killTree kills a dotnet process and every process it started (vstest.console, the data collector, the test hosts).
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
