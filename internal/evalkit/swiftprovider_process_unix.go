//go:build unix

package evalkit

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// SwiftPM evaluates manifests in child processes that can outlive the CLI.
func swiftProviderProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
}
