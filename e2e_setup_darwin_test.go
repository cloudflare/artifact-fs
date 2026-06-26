package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// skipIfNoFUSE skips the test if macFUSE is not installed or the
// mount helper cannot be executed by this test process. Some sandboxed
// environments expose the macFUSE bundle but deny exec of mount_macfuse;
// without this preflight every e2e case waits for its mount timeout.
func skipIfNoFUSE(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/Library/Filesystems/macfuse.fs"); err != nil {
		t.Skip("skipping: macFUSE not installed")
	}
	helper := "/Library/Filesystems/macfuse.fs/Contents/Resources/mount_macfuse"
	cmd := exec.Command(helper, "--help")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "operation not permitted") || strings.Contains(string(out), "operation not permitted") {
		t.Skipf("skipping: macFUSE mount helper is not executable in this environment: %v", err)
	}
}

// configGitSafeDir adds safe.directory entries for the mount path.
// macOS resolves /tmp to /private/tmp, so we add both variants.
func configGitSafeDir(t *testing.T, path string) {
	t.Helper()
	for _, p := range []string{path, fmt.Sprintf("/private%s", path)} {
		exec.Command("git", "config", "--global", "--add", "safe.directory", p).Run()
	}
}

// isMounted checks whether the given path is a FUSE mount point.
// On macOS, mount(8) reports /private/tmp even for /tmp paths.
func isMounted(path string) bool {
	out, err := exec.Command("mount").Output()
	if err != nil {
		return false
	}
	s := string(out)
	return strings.Contains(s, path) || strings.Contains(s, "/private"+path)
}
