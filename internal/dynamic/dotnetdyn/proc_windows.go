package dotnetdyn

import (
	"os/exec"
	"strconv"
)

func setProcessGroup(*exec.Cmd) {}

// killTree kills a dotnet process and every process it started (vstest.console, the data collector, the test hosts).
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() == nil {
		return nil
	}
	return cmd.Process.Kill()
}
