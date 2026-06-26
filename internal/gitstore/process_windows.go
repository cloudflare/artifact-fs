//go:build windows

package gitstore

import "os/exec"

func configureBatchCommand(_ *exec.Cmd) {}

func killBatchCommand(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
