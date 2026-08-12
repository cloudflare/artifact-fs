//go:build !windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	r.stop(t)
	removeAllWithRetry(t, r.mountDir)
	removeAllWithRetry(t, r.root)
}

func (r *mountedE2ERepo) stop(t *testing.T) {
	t.Helper()
	if r.svc == nil {
		return
	}
	svc := r.svc
	r.svc = nil
	r.cancel()
	deadline := time.Now().Add(10 * time.Second)
	shutdownTimer := time.NewTimer(time.Until(deadline))
	defer shutdownTimer.Stop()
	closeErrCh := make(chan error, 1)
	go func() {
		closeErrCh <- svc.Close()
	}()

	select {
	case err := <-closeErrCh:
		if err != nil {
			t.Errorf("close daemon: %v", err)
		}
	case <-shutdownTimer.C:
		t.Fatal("Service.Close did not return within 10s")
	}
	for isMounted(r.mountPath) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if isMounted(r.mountPath) {
		t.Fatal("mount remained active after shutdown")
	}
	select {
	case err := <-r.errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("daemon exit: %v", err)
		}
	case <-shutdownTimer.C:
		t.Fatal("daemon did not exit within 10s")
	}
	time.Sleep(200 * time.Millisecond)
}

func (r *mountedE2ERepo) restart(t *testing.T) {
	t.Helper()
	r.stop(t)

	ctx, cancel := context.WithCancel(context.Background())
	svc, err := daemon.New(ctx, r.root, logging.NewJSONLogger(os.Stderr, slog.LevelWarn))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	svc.SetMountRoot(r.mountDir)
	errCh := make(chan error, 1)
	go func() { errCh <- svc.Start(ctx) }()
	if !waitForMount(t, r.mountPath, 60*time.Second) {
		cancel()
		_ = svc.Close()
		t.Fatal("FUSE mount did not reappear after restart")
	}
	r.svc = svc
	r.cancel = cancel
	r.errCh = errCh
}

func TestE2EGitCleanState(t *testing.T) {
	repo := newMountedE2ERepo(t)

	assertGitStatus(t, repo.mountPath, map[string]string{})
	gitCmdQuiet(t, repo.mountPath, "diff", "--quiet")
	gitCmdQuiet(t, repo.mountPath, "diff", "--cached", "--quiet")
	info, err := os.Stat(filepath.Join(repo.mountPath, "vendor", "dependency"))
	if err != nil {
		t.Fatalf("stat gitlink placeholder: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("gitlink placeholder mode = %s, want directory", info.Mode())
	}
}

func TestE2EGitStatusDetectsSameSizeRewriteAfterMtimeRestore(t *testing.T) {
	repo := newMountedE2ERepo(t)

	readmePath := filepath.Join(repo.mountPath, "README.md")
	assertGitStatus(t, repo.mountPath, map[string]string{})
	st, err := os.Stat(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	indexedMtime := st.ModTime()

	updated := []byte(readFileEventually(t, readmePath))
	if len(updated) == 0 {
		t.Fatal("README.md is empty")
	}
	if updated[0] == 'x' {
		updated[0] = 'y'
	} else {
		updated[0] = 'x'
	}
	if err := os.WriteFile(readmePath, updated, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(readmePath, indexedMtime, indexedMtime); err != nil {
		t.Fatal(err)
	}

	assertGitStatus(t, repo.mountPath, map[string]string{"README.md": " M"})
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

func TestE2EGitRenameTrackedDirectory(t *testing.T) {
	repo := newMountedE2ERepo(t)
	previousHead := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	openDescendant, err := os.Open(filepath.Join(repo.mountPath, "packages", "wrangler", "src", "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	defer openDescendant.Close()

	gitCmd(t, repo.mountPath, "mv", "packages/wrangler", "packages/wrangler-renamed")

	assertPathMissing(t, filepath.Join(repo.mountPath, "packages", "wrangler"))
	renamedRoot := filepath.Join(repo.mountPath, "packages", "wrangler-renamed")
	renamedManifest := filepath.Join(renamedRoot, "package.json")
	if got := readFileStr(t, renamedManifest); !strings.Contains(got, `"name":"wrangler"`) {
		t.Fatalf("renamed package manifest = %q", got)
	}
	if got := readFileStr(t, filepath.Join(renamedRoot, "nested", "deep", "config.json")); !strings.Contains(got, "compatibility_date") {
		t.Fatalf("renamed nested config = %q", got)
	}
	if target, err := os.Readlink(filepath.Join(renamedRoot, "manifest-link")); err != nil {
		t.Fatal(err)
	} else if target != "package.json" {
		t.Fatalf("renamed symlink target = %q", target)
	}
	if info, err := os.Stat(filepath.Join(renamedRoot, "bin", "run.sh")); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("renamed executable mode = %#o", info.Mode().Perm())
	}
	if data, err := io.ReadAll(openDescendant); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(data), "wrangler") {
		t.Fatalf("open tracked descendant after git mv = %q", data)
	}
	assertGitStatus(t, repo.mountPath, map[string]string{
		"packages/wrangler/bin/run.sh -> packages/wrangler-renamed/bin/run.sh":                           "R ",
		"packages/wrangler/manifest-link -> packages/wrangler-renamed/manifest-link":                     "R ",
		"packages/wrangler/nested/deep/config.json -> packages/wrangler-renamed/nested/deep/config.json": "R ",
		"packages/wrangler/package.json -> packages/wrangler-renamed/package.json":                       "R ",
		"packages/wrangler/src/index.js -> packages/wrangler-renamed/src/index.js":                       "R ",
	})

	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "rename tracked directory",
	)
	waitForHeadAndStatus(t, repo.mountPath, previousHead, map[string]string{})
	waitForOverlayClean(t, repo)
	assertPathMissing(t, filepath.Join(repo.mountPath, "packages", "wrangler"))
	if got := readFileStr(t, renamedManifest); !strings.Contains(got, `"name":"wrangler"`) {
		t.Fatalf("renamed package manifest after reconcile = %q", got)
	}
	assertPathExists(t, filepath.Join(renamedRoot, "nested", "deep", "config.json"))
	assertPathExists(t, filepath.Join(renamedRoot, "bin", "run.sh"))
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

func TestE2EGitMergeAndRebase(t *testing.T) {
	repo := newMountedE2ERepo(t)

	gitCmd(t, repo.mountPath, "checkout", "-b", "feature")
	writeTestFile(t, repo.mountPath, "FEATURE.md", "feature\n")
	gitCmd(t, repo.mountPath, "add", "FEATURE.md")
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "add feature",
	)
	featureHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	waitForBranchAndStatus(t, repo.mountPath, "feature", map[string]string{})

	gitCmd(t, repo.mountPath, "checkout", "main")
	writeTestFile(t, repo.mountPath, "MAIN.md", "main\n")
	gitCmd(t, repo.mountPath, "add", "MAIN.md")
	preMainCommitHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "advance main",
	)
	waitForHeadAndStatus(t, repo.mountPath, preMainCommitHEAD, map[string]string{})
	preMergeHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"merge", "--no-ff", "-m", "merge feature", "feature",
	)
	waitForHeadAndStatus(t, repo.mountPath, preMergeHEAD, map[string]string{})
	if parents := strings.Fields(gitCmd(t, repo.mountPath, "show", "-s", "--format=%P", "HEAD")); len(parents) != 2 {
		t.Fatalf("merge parents = %v, want 2", parents)
	}
	assertPathExists(t, filepath.Join(repo.mountPath, "FEATURE.md"))
	assertPathExists(t, filepath.Join(repo.mountPath, "MAIN.md"))

	gitCmd(t, repo.mountPath, "checkout", "feature")
	writeTestFile(t, repo.mountPath, "FEATURE-2.md", "feature two\n")
	gitCmd(t, repo.mountPath, "add", "FEATURE-2.md")
	preFeatureCommitHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "extend feature",
	)
	waitForHeadAndStatus(t, repo.mountPath, preFeatureCommitHEAD, map[string]string{})
	preRebaseHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	if preRebaseHEAD == featureHEAD {
		t.Fatal("feature branch did not advance before rebase")
	}
	gitCmd(t, repo.mountPath, "rebase", "main")
	waitForHeadAndStatus(t, repo.mountPath, preRebaseHEAD, map[string]string{})
	if rebasedHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD")); rebasedHEAD == preRebaseHEAD {
		t.Fatal("rebase did not rewrite the feature commit")
	}
	gitCmdQuiet(t, repo.mountPath, "merge-base", "--is-ancestor", "main", "HEAD")
	assertPathExists(t, filepath.Join(repo.mountPath, "FEATURE.md"))
	assertPathExists(t, filepath.Join(repo.mountPath, "FEATURE-2.md"))
	assertPathExists(t, filepath.Join(repo.mountPath, "MAIN.md"))
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

	openSource, err := os.Open(filepath.Join(repo.mountPath, "SECURITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	defer openSource.Close()
	gitCmd(t, repo.mountPath, "mv", "SECURITY.md", "SECURITY-renamed.md")
	if _, err := openSource.Stat(); err != nil {
		t.Fatalf("stat open source after rename: %v", err)
	}
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

func TestE2EGitCommitExecutableBit(t *testing.T) {
	repo := newMountedE2ERepo(t)

	readmePath := filepath.Join(repo.mountPath, "README.md")
	stagedContent := readFileEventually(t, readmePath) + "staged before chmod\n"
	if err := os.WriteFile(readmePath, []byte(stagedContent), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.mountPath, "add", "README.md")
	if err := os.Chmod(readmePath, 0o755); err != nil {
		t.Fatal(err)
	}
	assertGitStatus(t, repo.mountPath, map[string]string{"README.md": "MM"})

	preContentCommitHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "update readme before mode",
	)
	waitForHeadAndStatus(t, repo.mountPath, preContentCommitHEAD, map[string]string{"README.md": " M"})
	if got := readFileStr(t, readmePath); got != stagedContent {
		t.Fatalf("README.md content = %q, want staged content", got)
	}
	headEntry := strings.Fields(gitCmd(t, repo.mountPath, "ls-tree", "HEAD", "README.md"))
	if len(headEntry) == 0 || headEntry[0] != "100644" {
		t.Fatalf("HEAD entry = %q, want mode 100644", headEntry)
	}
	st, err := os.Stat(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("worktree mode after content commit = %#o, want 0755", st.Mode().Perm())
	}

	assertGitStatus(t, repo.mountPath, map[string]string{"README.md": " M"})
	gitCmd(t, repo.mountPath, "add", "README.md")
	if summary := gitCmd(t, repo.mountPath, "diff", "--cached", "--summary"); !strings.Contains(summary, "mode change 100644 => 100755 README.md") {
		t.Fatalf("staged summary = %q, want executable-bit change", summary)
	}

	preCommitHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "make readme executable",
	)

	waitForHeadAndStatus(t, repo.mountPath, preCommitHEAD, map[string]string{})
	waitForOverlayClean(t, repo)
	st, err = os.Stat(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("README.md mode = %#o, want 0755", st.Mode().Perm())
	}
	if staged := gitCmd(t, repo.mountPath, "ls-files", "--stage", "README.md"); !strings.HasPrefix(staged, "100755 ") {
		t.Fatalf("index entry = %q, want mode 100755", staged)
	}
}

func TestE2EGitCommitSymlink(t *testing.T) {
	repo := newMountedE2ERepo(t)

	linkPath := filepath.Join(repo.mountPath, "readme-link")
	if err := os.Symlink("README.md", linkPath); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(linkPath); err != nil || target != "README.md" {
		t.Fatalf("readlink before commit = %q, %v", target, err)
	}
	assertGitStatus(t, repo.mountPath, map[string]string{"readme-link": "??"})
	gitCmd(t, repo.mountPath, "add", "readme-link")

	preCommitHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "add readme symlink",
	)

	waitForHeadAndStatus(t, repo.mountPath, preCommitHEAD, map[string]string{})
	waitForOverlayClean(t, repo)
	if target, err := os.Readlink(linkPath); err != nil || target != "README.md" {
		t.Fatalf("readlink after commit = %q, %v", target, err)
	}
	if staged := gitCmd(t, repo.mountPath, "ls-files", "--stage", "readme-link"); !strings.HasPrefix(staged, "120000 ") {
		t.Fatalf("index entry = %q, want mode 120000", staged)
	}

	renamedPath := filepath.Join(repo.mountPath, "renamed-readme-link")
	gitCmd(t, repo.mountPath, "mv", "readme-link", "renamed-readme-link")
	assertGitStatus(t, repo.mountPath, map[string]string{"readme-link -> renamed-readme-link": "R "})
	preRenameHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "rename readme symlink",
	)
	waitForHeadAndStatus(t, repo.mountPath, preRenameHEAD, map[string]string{})
	waitForOverlayClean(t, repo)
	assertPathMissing(t, linkPath)
	if target, err := os.Readlink(renamedPath); err != nil || target != "README.md" {
		t.Fatalf("renamed readlink = %q, %v", target, err)
	}
	if staged := gitCmd(t, repo.mountPath, "ls-files", "--stage", "renamed-readme-link"); !strings.HasPrefix(staged, "120000 ") {
		t.Fatalf("renamed index entry = %q, want mode 120000", staged)
	}
}

func TestE2EGitResetHardCleanAndStash(t *testing.T) {
	repo := newMountedE2ERepo(t)

	readmePath := filepath.Join(repo.mountPath, "README.md")
	original := readFileEventually(t, readmePath)
	if err := os.WriteFile(readmePath, []byte(original+"reset me\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(readmePath, 0o755); err != nil {
		t.Fatal(err)
	}
	addedPath := filepath.Join(repo.mountPath, "added-before-reset.txt")
	if err := os.WriteFile(addedPath, []byte("discard me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.mountPath, "add", "README.md", "added-before-reset.txt")
	gitCmd(t, repo.mountPath, "reset", "--hard", "HEAD")
	assertGitStatus(t, repo.mountPath, map[string]string{})
	assertPathMissing(t, addedPath)
	if got := readFileStr(t, readmePath); got != original {
		t.Fatalf("README.md after reset --hard = %q, want original", got)
	}
	st, err := os.Stat(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("README.md mode after reset --hard = %#o, want 0644", st.Mode().Perm())
	}

	untrackedDir := filepath.Join(repo.mountPath, "untracked-dir")
	if err := os.MkdirAll(untrackedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, untrackedDir, "nested.txt", "remove me\n")
	gitCmd(t, repo.mountPath, "clean", "-fd")
	assertPathMissing(t, untrackedDir)
	assertGitStatus(t, repo.mountPath, map[string]string{})

	if err := os.WriteFile(readmePath, []byte(original+"stash me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	untrackedPath := filepath.Join(repo.mountPath, "stash-untracked.txt")
	if err := os.WriteFile(untrackedPath, []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.mountPath, "stash", "push", "--include-untracked", "-m", "artifact-fs e2e")
	assertGitStatus(t, repo.mountPath, map[string]string{})
	assertPathMissing(t, untrackedPath)
	if list := gitCmd(t, repo.mountPath, "stash", "list"); !strings.Contains(list, "artifact-fs e2e") {
		t.Fatalf("stash list = %q", list)
	}
	gitCmd(t, repo.mountPath, "stash", "pop")
	assertGitStatus(t, repo.mountPath, map[string]string{
		"README.md":           " M",
		"stash-untracked.txt": "??",
	})
	if got := readFileStr(t, readmePath); got != original+"stash me\n" {
		t.Fatalf("README.md after stash pop = %q", got)
	}
	if got := readFileStr(t, untrackedPath); got != "untracked\n" {
		t.Fatalf("untracked file after stash pop = %q", got)
	}
}

func TestE2EGitPullFastForward(t *testing.T) {
	if os.Getenv("AFS_E2E_REPO") != "" {
		t.Skip("skipping remote advance against AFS_E2E_REPO")
	}
	repo := newMountedE2ERepo(t)

	remoteURL := strings.TrimSpace(gitCmd(t, repo.mountPath, "remote", "get-url", "origin"))
	writer := filepath.Join(t.TempDir(), "writer")
	run(t, "", "git", "clone", "--branch", "main", remoteURL, writer)
	writeTestFile(t, writer, "REMOTE-ONLY.md", "remote fast-forward content\n")
	run(t, writer, "git", "add", "REMOTE-ONLY.md")
	run(t, writer,
		"git",
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "advance remote for pull",
	)
	run(t, writer, "git", "push", "origin", "main")

	prePullHEAD := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath, "pull", "--ff-only", "origin", "main")
	waitForHeadAndStatus(t, repo.mountPath, prePullHEAD, map[string]string{})
	waitForOverlayClean(t, repo)
	if got := readFileStr(t, filepath.Join(repo.mountPath, "REMOTE-ONLY.md")); got != "remote fast-forward content\n" {
		t.Fatalf("REMOTE-ONLY.md after pull = %q", got)
	}
	if tracking := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "origin/main")); tracking == prePullHEAD {
		t.Fatalf("origin/main did not advance from %s", prePullHEAD)
	}
}

func TestE2EGitPush(t *testing.T) {
	if os.Getenv("AFS_E2E_REPO") != "" {
		t.Skip("skipping push against AFS_E2E_REPO")
	}
	repo := newMountedE2ERepo(t)

	writeTestFile(t, repo.mountPath, "PUSHED.md", "pushed from ArtifactFS\n")
	gitCmd(t, repo.mountPath, "add", "PUSHED.md")
	gitCmd(t, repo.mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit", "-m", "add pushed fixture",
	)
	head := strings.TrimSpace(gitCmd(t, repo.mountPath, "rev-parse", "HEAD"))
	gitCmd(t, repo.mountPath, "push", "origin", "HEAD:refs/heads/e2e-push")

	advertised := strings.Fields(gitCmd(t, repo.mountPath, "ls-remote", "origin", "refs/heads/e2e-push"))
	if len(advertised) != 2 || advertised[0] != head {
		t.Fatalf("pushed ref = %q, want %s", advertised, head)
	}
}

func assertGitStatus(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := gitStatusShortMap(t, dir)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("git status mismatch:\n got: %v\nwant: %v", got, want)
	}
}

func waitForOverlayClean(t *testing.T, repo *mountedE2ERepo) {
	t.Helper()
	waitForCondition(t, 10*time.Second, "overlay reconciliation", func() (bool, string) {
		status, err := repo.svc.Status(context.Background(), repoName)
		if err != nil {
			return false, err.Error()
		}
		if !status.DirtyOverlay {
			return true, ""
		}
		return false, "overlay remains dirty"
	})
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
