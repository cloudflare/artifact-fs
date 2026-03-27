package daemon

import (
	"os"
	"os/exec"
	"strings"
)

// isMounted checks whether the given path is an active mount point.
// Reads /proc/mounts first (fast, no subprocess), falls back to mount(8).
func isMounted(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		out, err := exec.Command("mount").Output()
		if err != nil {
			return false
		}
		return matchMountOutput(string(out), path)
	}
	return matchProcMounts(string(data), path)
}

// matchProcMounts checks /proc/mounts where each line is:
// <device> <mountpoint> <fstype> <options> <dump> <pass>
func matchProcMounts(output, path string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return true
		}
	}
	return false
}

// matchMountOutput checks mount(8) output where each line is:
// <device> on <mountpoint> type <fstype> (<options>)
func matchMountOutput(output, path string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, " on "+path+" type ") {
			return true
		}
	}
	return false
}
