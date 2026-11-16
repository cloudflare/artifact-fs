//go:build windows

package gitstore

import (
	"os"
	"os/exec"
)

func configureCommandProcessGroup(_ *exec.Cmd) {}

// terminateCommandProcessGroup kills cmd: Windows has no SIGTERM.
func terminateCommandProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
