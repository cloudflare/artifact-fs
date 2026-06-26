//go:build !windows

package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestE2EAsyncPreparedGitDirBlocksUntilReady(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)

	remoteURL := os.Getenv("AFS_E2E_REPO")
	if remoteURL == "" {
		remoteURL = createLocalTestRepo(t)
	}
	preparedGitDir, preparedWorktree := createPreparedGitDir(t, remoteURL)
	_ = preparedWorktree

	unblock := filepath.Join(t.TempDir(), "unblock-fetch")
	installBlockingGitFetchWrapper(t, unblock)

	repo := newAsyncPreparedE2ERepo(t, preparedGitDir, "main")

	waitForCondition(t, 10*time.Second, "async repo preparing", func() (bool, string) {
		st, err := repo.svc.Status(context.Background(), repoName)
		if err != nil {
			return false, err.Error()
		}
		if st.State == model.PrepareStatePreparing {
			return true, ""
		}
		return false, "state=" + st.State
	})

	done := make(chan error, 1)
	go func() {
		entries, err := os.ReadDir(repo.mountPath)
		if err == nil && len(entries) == 0 {
			err = os.ErrNotExist
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("ReadDir returned before async prepare was released: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	if err := os.WriteFile(unblock, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReadDir after prepare release: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ReadDir did not unblock after async prepare completed")
	}

	entries := lsDir(t, repo.mountPath)
	assertContains(t, entries, ".git")
	assertContains(t, entries, "README.md")
	assertGitStatus(t, repo.mountPath, map[string]string{})
}

func TestE2EAsyncPreparedGitDirFailureThenRetry(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)

	remoteURL := os.Getenv("AFS_E2E_REPO")
	if remoteURL == "" {
		remoteURL = createLocalTestRepo(t)
	}
	preparedGitDir, preparedWorktree := createPreparedGitDir(t, "file://"+filepath.Join(t.TempDir(), "missing.git"))
	repo := newAsyncPreparedE2ERepo(t, preparedGitDir, "main")

	waitForCondition(t, 10*time.Second, "async prepare failure", func() (bool, string) {
		st, err := repo.svc.Status(context.Background(), repoName)
		if err != nil {
			return false, err.Error()
		}
		if st.State == model.PrepareStateFailed && st.PrepareError != "" {
			return true, ""
		}
		return false, "state=" + st.State + " prepare_error=" + st.PrepareError
	})

	if _, err := os.ReadDir(repo.mountPath); err == nil {
		t.Fatal("ReadDir unexpectedly succeeded after prepare failure")
	}

	gitCmd(t, preparedWorktree, "remote", "set-url", "origin", remoteURL)
	if err := repo.svc.Prepare(context.Background(), repoName); err != nil {
		t.Fatalf("prepare retry: %v", err)
	}

	waitForCondition(t, 30*time.Second, "async prepare retry ready", func() (bool, string) {
		st, err := repo.svc.Status(context.Background(), repoName)
		if err != nil {
			return false, err.Error()
		}
		if st.State == "mounted" && st.PrepareError == "" {
			return true, ""
		}
		return false, "state=" + st.State + " prepare_error=" + st.PrepareError
	})

	entries := lsDir(t, repo.mountPath)
	assertContains(t, entries, "README.md")
	assertGitStatus(t, repo.mountPath, map[string]string{})
}

func createPreparedGitDir(t *testing.T, remoteURL string) (gitDir string, worktree string) {
	t.Helper()
	tmp := t.TempDir()
	gitDir = filepath.Join(tmp, "prepared.git")
	worktree = filepath.Join(tmp, "prepared")
	run(t, "", "git", "init", "--separate-git-dir", gitDir, "--initial-branch", "main", worktree)
	run(t, worktree, "git", "remote", "add", "origin", remoteURL)
	return gitDir, worktree
}

func installBlockingGitFetchWrapper(t *testing.T, unblockPath string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapperPath := filepath.Join(wrapperDir, "git")
	script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "fetch" ]; then
    while [ ! -f "$AFS_ASYNC_GIT_UNBLOCK" ]; do
      sleep 0.05
    done
    break
  fi
done
exec "$AFS_REAL_GIT" "$@"
`
	if err := os.WriteFile(wrapperPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AFS_REAL_GIT", realGit)
	t.Setenv("AFS_ASYNC_GIT_UNBLOCK", unblockPath)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func newAsyncPreparedE2ERepo(t *testing.T, preparedGitDir string, fetchRef string) *mountedE2ERepo {
	t.Helper()
	root, err := os.MkdirTemp("", "artifact-fs-e2e-async-root-*")
	if err != nil {
		t.Fatal(err)
	}
	mountDir, err := os.MkdirTemp("", "artifact-fs-e2e-async-mount-*")
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
		Name:            repoName,
		ID:              model.RepoID(repoName),
		Branch:          "main",
		RefreshInterval: 5 * time.Minute,
		MountRoot:       mountDir,
		GitDir:          preparedGitDir,
		PreparedGitDir:  true,
		FetchRef:        fetchRef,
		Enabled:         true,
	}
	if err := svc.AddRepoWithOptions(ctx, cfg, daemon.AddRepoOptions{Async: true}); err != nil {
		cancel()
		_ = svc.Close()
		t.Fatalf("add-repo async prepared-gitdir: %v", err)
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
