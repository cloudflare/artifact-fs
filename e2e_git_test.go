//go:build !windows

package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
)

type mountedE2ERepo struct {
	root      string
	mountDir  string
	mountPath string
	svc       *daemon.Service
	cancel    context.CancelFunc
	errCh     chan error
}

func newMountedE2ERepo(t *testing.T) *mountedE2ERepo {
	t.Helper()
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)

	remoteURL := os.Getenv("AFS_E2E_REPO")
	if remoteURL == "" {
		remoteURL = createLocalTestRepo(t)
	}

	root, err := os.MkdirTemp("", "artifact-fs-e2e-root-*")
	if err != nil {
		t.Fatal(err)
	}
	mountDir, err := os.MkdirTemp("", "artifact-fs-e2e-mount-*")
	if err != nil {
		_ = os.RemoveAll(root)
		t.Fatal(err)
	}
	mountPath := filepath.Join(mountDir, repoName)
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		_ = os.RemoveAll(mountDir)
		_ = os.RemoveAll(root)
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	logger := logging.NewJSONLogger(os.Stderr, slog.LevelWarn)
	svc, err := daemon.New(ctx, root, logger)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	svc.SetMountRoot(mountDir)

	cfg := model.RepoConfig{
		Name:              repoName,
		ID:                model.RepoID(repoName),
		RemoteURL:         remoteURL,
		RemoteURLRedacted: auth.RedactRemoteURL(remoteURL),
		Branch:            "main",
		RefreshInterval:   5 * time.Minute,
		MountRoot:         mountDir,
		Enabled:           true,
	}
	if err := svc.AddRepo(ctx, cfg); err != nil {
		cancel()
		_ = svc.Close()
		t.Fatalf("add-repo: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- svc.Start(ctx) }()

	if !waitForMount(t, mountPath, 60*time.Second) {
		cancel()
		_ = svc.Close()
		t.Fatal("FUSE mount did not appear within timeout")
	}

	repo := &mountedE2ERepo{
		root:      root,
		mountDir:  mountDir,
		mountPath: mountPath,
		svc:       svc,
		cancel:    cancel,
		errCh:     errCh,
	}
	t.Cleanup(func() {
		repo.close(t)
	})
	return repo
}

func (r *mountedE2ERepo) close(t *testing.T) {
	t.Helper()
	r.cancel()
	if err := r.svc.Close(); err != nil {
		t.Errorf("close daemon: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for isMounted(r.mountPath) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case <-r.errCh:
	case <-time.After(10 * time.Second):
		t.Log("daemon did not exit within 10s")
	}
	time.Sleep(200 * time.Millisecond)
	removeAllWithRetry(t, r.mountDir)
	removeAllWithRetry(t, r.root)
}

func TestE2EGitCleanState(t *testing.T) {
	repo := newMountedE2ERepo(t)

	assertGitStatus(t, repo.mountPath, map[string]string{})
	gitCmdQuiet(t, repo.mountPath, "diff", "--quiet")
	gitCmdQuiet(t, repo.mountPath, "diff", "--cached", "--quiet")
}

func TestE2EGitStatusPorcelain(t *testing.T) {
	repo := newMountedE2ERepo(t)

	readmePath := filepath.Join(repo.mountPath, "README.md")
	readmeOrig := readFileEventually(t, readmePath)
	if err := os.WriteFile(readmePath, []byte(readmeOrig+"unstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	packagePath := filepath.Join(repo.mountPath, "package.json")
	if err := os.WriteFile(packagePath, []byte(`{"name":"e2e-test-repo","version":"2.0.0"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.mountPath, "add", "package.json")

	licensePath := filepath.Join(repo.mountPath, "LICENSE-MIT")
	licenseOrig := readFileEventually(t, licensePath)
	if err := os.WriteFile(licensePath, []byte(licenseOrig+"staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.mountPath, "add", "LICENSE-MIT")
	if err := os.WriteFile(licensePath, []byte(licenseOrig+"staged\nunstaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gitCmd(t, repo.mountPath, "mv", "SECURITY.md", "SECURITY-renamed.md")

	if err := os.WriteFile(filepath.Join(repo.mountPath, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	assertGitStatus(t, repo.mountPath, map[string]string{
		"README.md":                          " M",
		"package.json":                       "M ",
		"LICENSE-MIT":                        "MM",
		"SECURITY.md -> SECURITY-renamed.md": "R ",
		"untracked.txt":                      "??",
	})
}

func TestE2EGitCheckoutBranchSwitch(t *testing.T) {
	repo := newMountedE2ERepo(t)

	mainReadme := readFileEventually(t, filepath.Join(repo.mountPath, "README.md"))
	gitCmd(t, repo.mountPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo.mountPath, "README.md"), []byte(mainReadme+"feature branch line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.mountPath, "rm", "LICENSE-MIT")
	if err := os.WriteFile(filepath.Join(repo.mountPath, "feature.txt"), []byte("feature branch file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.mountPath, "add", "README.md", "feature.txt")
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "feature branch commit",
	)
	waitForBranchAndStatus(t, repo.mountPath, "feature", map[string]string{})

	gitCmd(t, repo.mountPath, "checkout", "main")
	waitForBranchAndStatus(t, repo.mountPath, "main", map[string]string{})
	if got := readFileStr(t, filepath.Join(repo.mountPath, "README.md")); got != mainReadme {
		t.Fatalf("README.md after checkout main = %q, want original content", got)
	}
	assertPathExists(t, filepath.Join(repo.mountPath, "LICENSE-MIT"))
	assertPathMissing(t, filepath.Join(repo.mountPath, "feature.txt"))

	gitCmd(t, repo.mountPath, "checkout", "feature")
	waitForBranchAndStatus(t, repo.mountPath, "feature", map[string]string{})
	if got := readFileStr(t, filepath.Join(repo.mountPath, "README.md")); got != mainReadme+"feature branch line\n" {
		t.Fatalf("README.md after checkout feature = %q", got)
	}
	assertPathMissing(t, filepath.Join(repo.mountPath, "LICENSE-MIT"))
	assertPathExists(t, filepath.Join(repo.mountPath, "feature.txt"))
}

func TestE2EGitCheckoutConflictKeepsLocalChanges(t *testing.T) {
	repo := newMountedE2ERepo(t)

	mainReadme := readFileEventually(t, filepath.Join(repo.mountPath, "README.md"))
	gitCmd(t, repo.mountPath, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo.mountPath, "README.md"), []byte(mainReadme+"feature branch line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.mountPath, "add", "README.md")
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "feature readme change",
	)
	waitForBranchAndStatus(t, repo.mountPath, "feature", map[string]string{})

	gitCmd(t, repo.mountPath, "checkout", "main")
	waitForBranchAndStatus(t, repo.mountPath, "main", map[string]string{})

	localChange := mainReadme + "local main change\n"
	if err := os.WriteFile(filepath.Join(repo.mountPath, "README.md"), []byte(localChange), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := gitCmdResult(repo.mountPath, "checkout", "feature")
	if err == nil {
		t.Fatal("git checkout feature unexpectedly succeeded")
	}
	if !strings.Contains(stderr, "would be overwritten by checkout") {
		t.Fatalf("expected checkout conflict, got stderr %q", stderr)
	}

	if branch := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "--abbrev-ref", "HEAD")); branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
	if got := readFileStr(t, filepath.Join(repo.mountPath, "README.md")); got != localChange {
		t.Fatalf("README.md changed after failed checkout: %q", got)
	}
	assertGitStatus(t, repo.mountPath, map[string]string{"README.md": " M"})
	assertPathExists(t, filepath.Join(repo.mountPath, "LICENSE-MIT"))
	assertPathMissing(t, filepath.Join(repo.mountPath, "feature.txt"))
}

func TestE2EGitCheckoutRestorePath(t *testing.T) {
	repo := newMountedE2ERepo(t)

	readmePath := filepath.Join(repo.mountPath, "README.md")
	readmeOrig := readFileEventually(t, readmePath)
	if err := os.WriteFile(readmePath, []byte(readmeOrig+"local change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertGitStatus(t, repo.mountPath, map[string]string{"README.md": " M"})

	gitCmd(t, repo.mountPath, "checkout", "--", "README.md")
	assertGitStatus(t, repo.mountPath, map[string]string{})
	if got := readFileStr(t, readmePath); got != readmeOrig {
		t.Fatalf("README.md after checkout -- = %q, want original content", got)
	}
}

func TestE2EGitCommitModifyTrackedFile(t *testing.T) {
	repo := newMountedE2ERepo(t)

	readmePath := filepath.Join(repo.mountPath, "README.md")
	updated := readFileEventually(t, readmePath) + "committed tracked change\n"
	if err := os.WriteFile(readmePath, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.mountPath, "add", "README.md")
	preCommitHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "modify readme in e2e",
	)

	waitForHeadAndStatus(t, repo.mountPath, preCommitHEAD, map[string]string{})
	if got := readFileStr(t, readmePath); got != updated {
		t.Fatalf("README.md after commit = %q", got)
	}
	if logOut := gitCmd(t, repo.mountPath, "log", "--oneline", "-1"); !strings.Contains(logOut, "modify readme in e2e") {
		t.Fatalf("expected commit message in log, got %q", logOut)
	}
}

func TestE2EGitCommitPreservesUnstagedAfterStagedCommit(t *testing.T) {
	repo := newMountedE2ERepo(t)

	readmePath := filepath.Join(repo.mountPath, "README.md")
	base := readFileEventually(t, readmePath)
	staged := base + "staged line\n"
	worktree := staged + "unstaged line\n"
	if err := os.WriteFile(readmePath, []byte(staged), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.mountPath, "add", "README.md")
	if err := os.WriteFile(readmePath, []byte(worktree), 0o644); err != nil {
		t.Fatal(err)
	}
	assertGitStatus(t, repo.mountPath, map[string]string{"README.md": "MM"})

	preCommitHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "commit staged readme change",
	)

	waitForHeadAndStatus(t, repo.mountPath, preCommitHEAD, map[string]string{"README.md": " M"})
	if got := readFileStr(t, readmePath); got != worktree {
		t.Fatalf("README.md worktree content = %q", got)
	}
	if headReadme := gitCmd(t, repo.mountPath, "show", "HEAD:README.md"); headReadme != staged {
		t.Fatalf("HEAD README.md = %q, want staged content", headReadme)
	}
}

func TestE2EGitCommitTrackedRename(t *testing.T) {
	repo := newMountedE2ERepo(t)

	gitCmd(t, repo.mountPath, "mv", "SECURITY.md", "SECURITY-renamed.md")
	assertGitStatus(t, repo.mountPath, map[string]string{"SECURITY.md -> SECURITY-renamed.md": "R "})

	preCommitHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "rename security doc",
	)

	waitForHeadAndStatus(t, repo.mountPath, preCommitHEAD, map[string]string{})
	assertPathMissing(t, filepath.Join(repo.mountPath, "SECURITY.md"))
	if got := readFileStr(t, filepath.Join(repo.mountPath, "SECURITY-renamed.md")); !strings.Contains(got, "Security") {
		t.Fatalf("renamed file content = %q", got)
	}
}

func TestE2EGitCommitTrackedDelete(t *testing.T) {
	repo := newMountedE2ERepo(t)

	gitCmd(t, repo.mountPath, "rm", "LICENSE-MIT")
	assertGitStatus(t, repo.mountPath, map[string]string{"LICENSE-MIT": "D "})

	preCommitHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "delete license in e2e",
	)

	waitForHeadAndStatus(t, repo.mountPath, preCommitHEAD, map[string]string{})
	assertPathMissing(t, filepath.Join(repo.mountPath, "LICENSE-MIT"))
	assertPathExists(t, filepath.Join(repo.mountPath, "README.md"))
}

func assertGitStatus(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := gitStatusShortMap(t, dir)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("git status mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func gitStatusShortMap(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := gitCmd(t, dir, "status", "--short", "--untracked-files=all")
	status := map[string]string{}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return status
	}
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		if len(line) < 4 {
			t.Fatalf("unexpected git status line %q", line)
		}
		status[strings.TrimSpace(line[3:])] = line[:2]
	}
	return status
}

func gitCmdQuiet(t *testing.T, dir string, args ...string) {
	t.Helper()
	_, stderr, err := gitCmdResult(dir, args...)
	if err != nil {
		t.Fatalf("git %s failed: %v\nstderr: %s", strings.Join(args, " "), err, stderr)
	}
}

func gitCmdResult(dir string, args ...string) (string, string, error) {
	cmd := exec.Command("git", gitArgsWithSafeDirectory(dir, args...)...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func waitForBranchAndStatus(t *testing.T, dir, wantBranch string, wantStatus map[string]string) {
	t.Helper()
	waitForCondition(t, 10*time.Second, fmt.Sprintf("branch=%s status=%v", wantBranch, wantStatus), func() (bool, string) {
		stdout, stderr, err := gitCmdResult(dir, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return false, fmt.Sprintf("rev-parse failed: %v stderr=%s", err, stderr)
		}
		branch := strings.TrimSpace(stdout)
		statusStdout, statusStderr, statusErr := gitCmdResult(dir, "status", "--short", "--untracked-files=all")
		if statusErr != nil {
			return false, fmt.Sprintf("status failed: %v stderr=%s", statusErr, statusStderr)
		}
		gotStatus, err := parseStatusOutput(statusStdout)
		if err != nil {
			return false, err.Error()
		}
		if branch == wantBranch && reflect.DeepEqual(gotStatus, wantStatus) {
			return true, ""
		}
		return false, fmt.Sprintf("branch=%q status=%v", branch, gotStatus)
	})
}

func waitForHeadAndStatus(t *testing.T, dir, prevHead string, wantStatus map[string]string) {
	t.Helper()
	waitForCondition(t, 10*time.Second, fmt.Sprintf("head changed from %s and status=%v", prevHead, wantStatus), func() (bool, string) {
		stdout, stderr, err := gitCmdResult(dir, "rev-parse", "HEAD")
		if err != nil {
			return false, fmt.Sprintf("rev-parse failed: %v stderr=%s", err, stderr)
		}
		head := strings.TrimSpace(stdout)
		statusStdout, statusStderr, statusErr := gitCmdResult(dir, "status", "--short", "--untracked-files=all")
		if statusErr != nil {
			return false, fmt.Sprintf("status failed: %v stderr=%s", statusErr, statusStderr)
		}
		gotStatus, err := parseStatusOutput(statusStdout)
		if err != nil {
			return false, err.Error()
		}
		if head != prevHead && reflect.DeepEqual(gotStatus, wantStatus) {
			return true, ""
		}
		return false, fmt.Sprintf("head=%q status=%v", head, gotStatus)
	})
}

func parseStatusOutput(out string) (map[string]string, error) {
	status := map[string]string{}
	out = strings.TrimRight(out, "\n")
	if out == "" {
		return status, nil
	}
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		if len(line) < 4 {
			return nil, fmt.Errorf("unexpected git status line %q", line)
		}
		status[strings.TrimSpace(line[3:])] = line[:2]
	}
	return status, nil
}

func waitForCondition(t *testing.T, timeout time.Duration, description string, fn func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := "condition not evaluated"
	for time.Now().Before(deadline) {
		ok, state := fn()
		if ok {
			return
		}
		last = state
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %s", description, last)
}

func assertPathExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be missing, got err=%v", path, err)
	}
}

func removeAllWithRetry(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := os.RemoveAll(path); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr != nil && !os.IsNotExist(lastErr) {
		t.Errorf("remove %s: %v", path, lastErr)
	}
}

func readFileEventually(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("read %s: %v", path, lastErr)
	return ""
}
