//go:build !windows

package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
)

const repoName = "e2e-test"

// createLocalTestRepo creates a bare git repo seeded with enough content
// to exercise all e2e test assertions. Returns a file:// URL suitable for
// blobless clone (file:// forces the smart transport which supports --filter).
func createLocalTestRepo(t *testing.T) string {
	t.Helper()

	bareDir := filepath.Join(t.TempDir(), "test-repo.git")
	workDir := filepath.Join(t.TempDir(), "work")

	// Initialize bare repo.
	run(t, "", "git", "init", "--bare", bareDir)

	// Create a working tree and seed content across 3 commits.
	run(t, "", "git", "clone", bareDir, workDir)
	run(t, workDir, "git", "config", "user.name", "E2E Setup")
	run(t, workDir, "git", "config", "user.email", "e2e@test")
	// Some git versions default to "master"; force "main" for consistency.
	run(t, workDir, "git", "checkout", "-b", "main")

	// Commit 1: readme and license files.
	writeTestFile(t, workDir, "README.md", "# Test Repo\n\nA test repository for ArtifactFS e2e tests.\n")
	writeTestFile(t, workDir, "LICENSE-MIT", "MIT License\n\nCopyright 2024 Test\n\nPermission is hereby granted, free of charge.\n")
	writeTestFile(t, workDir, "SECURITY.md", "# Security\n\nReport security issues responsibly.\n")
	run(t, workDir, "git", "add", "-A")
	run(t, workDir, "git", "commit", "-m", "add readme and license")

	// Commit 2: root package manifest.
	writeTestFile(t, workDir, "package.json", `{"name":"e2e-test-repo","version":"1.0.0"}`+"\n")
	run(t, workDir, "git", "add", "-A")
	run(t, workDir, "git", "commit", "-m", "add package manifests")

	// Commit 3: packages directory with 4 subdirectories.
	for _, pkg := range []string{"wrangler", "miniflare", "vitest-pool", "workers-shared"} {
		dir := filepath.Join(workDir, "packages", pkg)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, dir, "package.json", `{"name":"`+pkg+`","version":"0.0.1"}`+"\n")
	}
	run(t, workDir, "git", "add", "-A")
	run(t, workDir, "git", "commit", "-m", "add packages directory")

	run(t, workDir, "git", "push", "origin", "main")

	return "file://" + bareDir
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s failed: %v\nstderr: %s", name, strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

func TestE2E(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)

	remoteURL := os.Getenv("AFS_E2E_REPO")
	if remoteURL == "" {
		remoteURL = createLocalTestRepo(t)
		t.Logf("using local test repo: %s", remoteURL)
	}

	root := t.TempDir()
	mountDir := t.TempDir()
	mountPath := filepath.Join(mountDir, repoName)
	if err := os.MkdirAll(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

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

	// ---- Git commit + reconciliation ----
	// Tests the full post-commit flow: watcher detects HEAD change ->
	// daemon re-indexes tree -> overlay reconciliation cleans up committed
	// entries -> new snapshot generation is visible to FUSE.

	t.Run("git/commit", func(t *testing.T) {
		preCommitHEAD := strings.TrimSpace(gitCmd(t, mountPath, "rev-parse", "HEAD"))

		// Create and stage a new file (isolated from prior dirty state).
		commitFile := filepath.Join(mountPath, "e2e-commit.txt")
		if err := os.WriteFile(commitFile, []byte("committed content\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, mountPath, "add", "e2e-commit.txt")

		// Commit (use -c flags to avoid needing global git config).
		gitCmd(t, mountPath,
			"-c", "user.name=E2E Test",
			"-c", "user.email=e2e@test",
			"commit", "-m", "e2e commit test",
		)

		// Poll until reconciliation completes. The watcher polls HEAD at
		// 500ms; after detecting the change, onHEADChanged re-indexes the
		// tree, reconciles the overlay, refreshes the git index, and swaps
		// the snapshot generation. When git status reports the committed
		// file as clean, all of that has finished.
		deadline := time.Now().Add(10 * time.Second)
		reconciled := false
		for time.Now().Before(deadline) {
			out := gitCmd(t, mountPath, "status", "--short", "e2e-commit.txt")
			if strings.TrimSpace(out) == "" {
				reconciled = true
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !reconciled {
			t.Fatal("overlay reconciliation did not complete within timeout")
		}

		// HEAD should have advanced.
		postCommitHEAD := strings.TrimSpace(gitCmd(t, mountPath, "rev-parse", "HEAD"))
		if postCommitHEAD == preCommitHEAD {
			t.Fatalf("HEAD did not change: still %s", preCommitHEAD)
		}

		// Log should contain our commit message.
		logOut := gitCmd(t, mountPath, "log", "--oneline", "-1")
		if !strings.Contains(logOut, "e2e commit test") {
			t.Fatalf("expected commit message in log, got %q", logOut)
		}

		// File content should still be readable (now served from the base
		// snapshot after overlay reconciliation removed the entry).
		got := readFileStr(t, commitFile)
		if got != "committed content\n" {
			t.Fatalf("expected 'committed content\\n', got %q", got)
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
	cmd := exec.Command("git", gitArgsWithSafeDirectory(dir, args...)...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s failed: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

func gitArgsWithSafeDirectory(dir string, args ...string) []string {
	if dir == "" {
		return args
	}
	out := []string{"-c", "safe.directory=" + dir}
	if strings.HasPrefix(dir, "/tmp/") {
		out = append(out, "-c", "safe.directory=/private"+dir)
	}
	return append(out, args...)
}

func assertContains(t *testing.T, items []string, want string) {
	t.Helper()
	if slices.Contains(items, want) {
		return
	}
	t.Fatalf("expected %q in %v", want, items)
}
