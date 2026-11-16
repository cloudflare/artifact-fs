//go:build !windows

package gitstore

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureCommandProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateCommandProcessGroup sends SIGTERM to cmd's process group, then
// SIGKILL to whatever of it still runs gitCommandWaitDelay later. On SIGTERM
// git removes the lock files it created, such as index.lock. A git killed with
// SIGKILL leaves them, and every later git that takes the lock fails.
func terminateCommandProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	pid := cmd.Process.Pid
	err := cmd.Process.Signal(syscall.SIGTERM)
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		time.AfterFunc(gitCommandWaitDelay, func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	}
	return err
}
