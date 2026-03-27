package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// skipIfNoFUSE skips the test if the FUSE kernel module is not available.
func skipIfNoFUSE(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("skipping: /dev/fuse not available")
	}
}

// configGitSafeDir adds a safe.directory entry for the mount path.
func configGitSafeDir(t *testing.T, path string) {
	t.Helper()
	exec.Command("git", "config", "--global", "--add", "safe.directory", path).Run()
}

// isMounted checks whether the given path is a FUSE mount point by reading /proc/mounts.
func isMounted(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		// Fall back to mount(8)
		out, err := exec.Command("mount").Output()
		if err != nil {
			return false
		}
		return strings.Contains(string(out), path)
	}
	return strings.Contains(string(data), path)
}
