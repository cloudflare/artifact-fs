package daemon

import (
	"os/exec"
	"strings"
)

// isMounted checks whether the given path is an active mount point.
// On macOS, mount(8) output format is: <device> on <path> (<options>)
// We match the " on <path> (" pattern to avoid substring false positives.
// macOS reports /private/tmp even for /tmp paths.
func isMounted(path string) bool {
	out, err := exec.Command("mount").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, " on "+path+" (") || strings.Contains(line, " on /private"+path+" (") {
			return true
		}
	}
	return false
}
