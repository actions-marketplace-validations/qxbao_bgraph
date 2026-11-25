//go:build !unix

package evalkit

import "os/exec"

func swiftProviderProcessGroup(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		killTree(cmd)
		return nil
	}
}
