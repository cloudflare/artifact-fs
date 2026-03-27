//go:build !windows

package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
)

const (
	defaultRepo = "https://github.com/cloudflare/workers-sdk.git"
	repoName    = "e2e-test"
)

func TestE2E(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)

	remoteURL := os.Getenv("AFS_E2E_REPO")
	if remoteURL == "" {
		remoteURL = defaultRepo
	}

	root := t.TempDir()
	mountDir := t.TempDir()
	mountPath := filepath.Join(mountDir, repoName)
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Platform-specific git safe.directory config
	configGitSafeDir(t, mountPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := logging.NewJSONLogger(os.Stderr, slog.LevelWarn)
	svc, err := daemon.New(ctx, root, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
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
		t.Fatalf("add-repo: %v", err)
	}

	// Start the daemon in background (it blocks on ctx.Done)
	errCh := make(chan error, 1)
	go func() { errCh <- svc.Start(ctx) }()

	// Wait for the FUSE mount to appear
	if !waitForMount(t, mountPath, 60*time.Second) {
		cancel()
		t.Fatal("FUSE mount did not appear within timeout")
	}
	t.Logf("mount active at %s", mountPath)

	// ---- Filesystem read operations ----

	t.Run("fs/ls_root", func(t *testing.T) {
		entries := lsDir(t, mountPath)
		if len(entries) < 5 {
			t.Fatalf("expected >5 root entries, got %d", len(entries))
		}
		assertContains(t, entries, ".git")
	})

	t.Run("fs/cat_gitfile", func(t *testing.T) {
		data := readFileStr(t, filepath.Join(mountPath, ".git"))
		if !strings.HasPrefix(data, "gitdir:") {
			t.Fatalf("expected gitdir:..., got %q", data)
		}
	})

	t.Run("fs/read_tracked_file", func(t *testing.T) {
		data := readFileStr(t, filepath.Join(mountPath, "README.md"))
		if len(data) == 0 {
			t.Fatal("README.md is empty")
		}
	})

	t.Run("fs/ls_subdir", func(t *testing.T) {
		entries := lsDir(t, filepath.Join(mountPath, "packages"))
		if len(entries) < 3 {
			t.Fatalf("expected >3 packages/ entries, got %d", len(entries))
		}
	})

	t.Run("fs/stat_size", func(t *testing.T) {
		fi, err := os.Stat(filepath.Join(mountPath, "package.json"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() == 0 {
			t.Fatal("package.json has size 0 -- sizes not resolved")
		}
	})

	t.Run("fs/read_nested", func(t *testing.T) {
		data := readFileStr(t, filepath.Join(mountPath, "packages", "wrangler", "package.json"))
		if !strings.Contains(data, "wrangler") {
			t.Fatalf("nested file doesn't contain expected content")
		}
	})

	// ---- Filesystem write operations ----

	t.Run("fs/create_file", func(t *testing.T) {
		p := filepath.Join(mountPath, "e2e-test-file.txt")
		if err := os.WriteFile(p, []byte("hello e2e\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := readFileStr(t, p)
		if got != "hello e2e\n" {
			t.Fatalf("expected 'hello e2e\\n', got %q", got)
		}
	})

	t.Run("fs/mkdir", func(t *testing.T) {
		p := filepath.Join(mountPath, "e2e-test-dir")
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if !fi.IsDir() {
			t.Fatal("expected directory")
		}
	})

	t.Run("fs/write_nested", func(t *testing.T) {
		p := filepath.Join(mountPath, "e2e-test-dir", "nested.txt")
		if err := os.WriteFile(p, []byte("nested\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := readFileStr(t, p)
		if got != "nested\n" {
			t.Fatalf("expected 'nested\\n', got %q", got)
		}
	})

	t.Run("fs/rename", func(t *testing.T) {
		src := filepath.Join(mountPath, "e2e-test-file.txt")
		dst := filepath.Join(mountPath, "e2e-renamed.txt")
		if err := os.Rename(src, dst); err != nil {
			t.Fatal(err)
		}
		got := readFileStr(t, dst)
		if got != "hello e2e\n" {
			t.Fatalf("renamed file content wrong: %q", got)
		}
		if _, err := os.Stat(src); !os.IsNotExist(err) {
			t.Fatal("original file still exists after rename")
		}
	})

	t.Run("fs/unlink", func(t *testing.T) {
		p := filepath.Join(mountPath, "e2e-renamed.txt")
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("file still exists after unlink")
		}
	})

	t.Run("fs/rmdir", func(t *testing.T) {
		os.Remove(filepath.Join(mountPath, "e2e-test-dir", "nested.txt"))
		p := filepath.Join(mountPath, "e2e-test-dir")
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("dir still exists after rmdir")
		}
	})

	t.Run("fs/modify_tracked", func(t *testing.T) {
		p := filepath.Join(mountPath, "README.md")
		orig := readFileStr(t, p)
		appended := orig + "# e2e test marker\n"
		if err := os.WriteFile(p, []byte(appended), 0o644); err != nil {
			t.Fatal(err)
		}
		got := readFileStr(t, p)
		if !strings.HasSuffix(got, "# e2e test marker\n") {
			t.Fatal("modified file doesn't end with marker")
		}
	})

	t.Run("fs/rename_tracked", func(t *testing.T) {
		src := filepath.Join(mountPath, "SECURITY.md")
		dst := filepath.Join(mountPath, "SECURITY.bak")
		if err := os.Rename(src, dst); err != nil {
			t.Fatal(err)
		}
		data := readFileStr(t, dst)
		if len(data) == 0 {
			t.Fatal("renamed tracked file is empty")
		}
		if _, err := os.Stat(src); !os.IsNotExist(err) {
			t.Fatal("original still exists after rename")
		}
	})

	t.Run("fs/truncate_tracked", func(t *testing.T) {
		p := filepath.Join(mountPath, "LICENSE-MIT")
		if err := os.Truncate(p, 0); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != 0 {
			t.Fatalf("expected size 0 after truncate, got %d", fi.Size())
		}
	})

	// ---- Git read operations ----

	t.Run("git/log", func(t *testing.T) {
		out := gitCmd(t, mountPath, "log", "--oneline", "-3")
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) < 3 {
			t.Fatalf("expected 3 log lines, got %d", len(lines))
		}
	})

	t.Run("git/branch", func(t *testing.T) {
		out := gitCmd(t, mountPath, "branch")
		if !strings.Contains(out, "main") {
			t.Fatalf("expected 'main' in branch output, got %q", out)
		}
	})

	t.Run("git/rev-parse", func(t *testing.T) {
		out := gitCmd(t, mountPath, "rev-parse", "HEAD")
		if len(strings.TrimSpace(out)) != 40 {
			t.Fatalf("expected 40-char SHA, got %q", out)
		}
	})

	t.Run("git/show", func(t *testing.T) {
		out := gitCmd(t, mountPath, "show", "HEAD", "--stat", "--format=%H")
		if len(out) == 0 {
			t.Fatal("git show returned empty output")
		}
	})

	t.Run("git/remote", func(t *testing.T) {
		out := gitCmd(t, mountPath, "remote", "-v")
		if !strings.Contains(out, "origin") {
			t.Fatalf("expected origin remote, got %q", out)
		}
	})

	t.Run("git/stash_list", func(t *testing.T) {
		_ = gitCmd(t, mountPath, "stash", "list")
	})

	// ---- Git write operations ----

	t.Run("git/diff", func(t *testing.T) {
		out := gitCmd(t, mountPath, "diff", "README.md")
		if !strings.Contains(out, "e2e test marker") {
			t.Fatalf("expected diff to contain marker, got %q", out)
		}
	})

	t.Run("git/add", func(t *testing.T) {
		gitCmd(t, mountPath, "add", "README.md")
		out := gitCmd(t, mountPath, "status", "--short", "README.md")
		if !strings.HasPrefix(strings.TrimSpace(out), "M") {
			t.Fatalf("expected staged M, got %q", out)
		}
	})

	t.Run("git/reset", func(t *testing.T) {
		gitCmd(t, mountPath, "reset", "HEAD", "README.md")
	})

	t.Run("git/status", func(t *testing.T) {
		out := gitCmd(t, mountPath, "status", "--short")
		if len(out) == 0 {
			t.Fatal("expected non-empty status output after modifications")
		}
	})

	// ---- Teardown ----
	cancel()
	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Log("daemon did not exit within 10s")
	}
}

// --- helpers (platform-independent) ---

func waitForMount(t *testing.T, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isMounted(path) {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func lsDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}

func readFileStr(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s failed: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

func assertContains(t *testing.T, items []string, want string) {
	t.Helper()
	for _, item := range items {
		if item == want {
			return
		}
	}
	t.Fatalf("expected %q in %v", want, items)
}
