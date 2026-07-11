//go:build windows

package gitstore

import (
	"os"
	"os/exec"
)

func configureCommandProcessGroup(_ *exec.Cmd) {}

func killCommandProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	return cmd.Process.Kill()
}
