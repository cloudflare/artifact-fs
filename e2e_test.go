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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/daemon"
	"github.com/cloudflare/artifact-fs/internal/logging"
	"github.com/cloudflare/artifact-fs/internal/model"
	"golang.org/x/sys/unix"
)

const repoName = "e2e-test"

func TestFUSEMountSmoke(t *testing.T) {
	repo := newMountedE2ERepo(t)
	if got := readFileStr(t, filepath.Join(repo.mountPath, "README.md")); !strings.Contains(got, "Test Repo") {
		t.Fatalf("mounted README.md = %q", got)
	}
}

// createLocalTestRepo creates a bare git repo seeded with enough content
// to exercise all e2e test assertions. Returns a file:// URL suitable for
// blobless clone (file:// forces the smart transport which supports --filter).
func createLocalTestRepo(t *testing.T) string {
	t.Helper()

	bareDir := filepath.Join(t.TempDir(), "test-repo.git")
	workDir := filepath.Join(t.TempDir(), "work")

	// Initialize bare repo.
	run(t, "", "git", "init", "--bare", bareDir)

	// Create a working tree and seed content across 4 commits.
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
	wranglerDir := filepath.Join(workDir, "packages", "wrangler")
	for _, dir := range []string{
		filepath.Join(wranglerDir, "bin"),
		filepath.Join(wranglerDir, "nested", "deep"),
		filepath.Join(wranglerDir, "src"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(wranglerDir, "src"), "index.js", "export function main() {\n  return 'wrangler';\n}\n")
	writeTestFile(t, filepath.Join(wranglerDir, "nested", "deep"), "config.json", `{"compatibility_date":"2026-07-30"}`+"\n")
	scriptPath := filepath.Join(wranglerDir, "bin", "run.sh")
	writeTestFile(t, filepath.Dir(scriptPath), filepath.Base(scriptPath), "#!/bin/sh\necho wrangler\n")
	if err := os.Chmod(scriptPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("package.json", filepath.Join(wranglerDir, "manifest-link")); err != nil {
		t.Fatal(err)
	}
	run(t, workDir, "git", "add", "-A")
	run(t, workDir, "git", "commit", "-m", "add packages directory")

	// Commit 4: a gitlink without cloning submodule contents.
	gitlinkOID := strings.TrimSpace(run(t, workDir, "git", "rev-parse", "HEAD"))
	run(t, workDir, "git", "update-index", "--add", "--cacheinfo", "160000,"+gitlinkOID+",vendor/dependency")
	run(t, workDir, "git", "commit", "-m", "add gitlink")

	run(t, workDir, "git", "push", "origin", "main")
	run(t, bareDir, "git", "config", "uploadpack.allowFilter", "true")

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

	t.Run("fs/rename_nonempty_overlay_directory", func(t *testing.T) {
		oldPath := filepath.Join(mountPath, "e2e-test-dir")
		newPath := filepath.Join(mountPath, "e2e-renamed-dir")
		if err := os.Rename(oldPath, newPath); err != nil {
			t.Fatal(err)
		}
		assertPathMissing(t, oldPath)
		if got := readFileStr(t, filepath.Join(newPath, "nested.txt")); got != "nested\n" {
			t.Fatalf("renamed nested file = %q", got)
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
		os.Remove(filepath.Join(mountPath, "e2e-renamed-dir", "nested.txt"))
		p := filepath.Join(mountPath, "e2e-renamed-dir")
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

func TestE2EFilesystemDirectoryMoveWorkflows(t *testing.T) {
	t.Run("cli_move_preserves_tree_and_open_handles", testE2ECLIDirectoryMove)
	t.Run("recursive_copy_move_and_replace_empty_destination", testE2ERecursiveCopyMoveAndReplace)
	t.Run("replace_visible_empty_tracked_destination", testE2EReplaceVisibleEmptyTrackedDirectory)
	t.Run("replace_deleted_tracked_destination", testE2EReplaceDeletedTrackedDirectory)
	t.Run("reject_nonempty_destination", testE2ERejectNonemptyDirectoryReplacement)
	t.Run("native_rename_error_contract", testE2ENativeDirectoryRenameErrors)
}

func TestE2EFilesystemDirectoryRenamePersistsAcrossRestart(t *testing.T) {
	repo := newMountedE2ERepo(t)

	renamedRoot := filepath.Join(repo.mountPath, "packages", "wrangler-pending")
	if err := unix.Rename(filepath.Join(repo.mountPath, "packages", "wrangler"), renamedRoot); err != nil {
		t.Fatal(err)
	}
	trackedDestination := filepath.Join(repo.mountPath, "packages", "miniflare")
	if err := os.Remove(filepath.Join(trackedDestination, "package.json")); err != nil {
		t.Fatal(err)
	}
	replacementSource := filepath.Join(repo.mountPath, "replacement-source")
	if err := os.MkdirAll(filepath.Join(replacementSource, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacementSource, "nested", "incoming.txt"), []byte("incoming\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Rename(replacementSource, trackedDestination); err != nil {
		t.Fatal(err)
	}

	beforeStatus := gitCmd(t, repo.mountPath, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	assertPathMissing(t, filepath.Join(repo.mountPath, "packages", "wrangler"))
	assertPathExists(t, filepath.Join(renamedRoot, "nested", "deep", "config.json"))
	assertPathMissing(t, filepath.Join(trackedDestination, "package.json"))
	assertPathExists(t, filepath.Join(trackedDestination, "nested", "incoming.txt"))

	repo.restart(t)

	afterStatus := gitCmd(t, repo.mountPath, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if afterStatus != beforeStatus {
		t.Fatalf("Git status changed across restart:\nbefore: %q\nafter:  %q", beforeStatus, afterStatus)
	}
	assertPathMissing(t, filepath.Join(repo.mountPath, "packages", "wrangler"))
	if got := readFileStr(t, filepath.Join(renamedRoot, "nested", "deep", "config.json")); !strings.Contains(got, "compatibility_date") {
		t.Fatalf("renamed tracked content after restart = %q", got)
	}
	if target, err := os.Readlink(filepath.Join(renamedRoot, "manifest-link")); err != nil {
		t.Fatal(err)
	} else if target != "package.json" {
		t.Fatalf("renamed symlink target after restart = %q", target)
	}
	if info, err := os.Stat(filepath.Join(renamedRoot, "bin", "run.sh")); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("renamed executable mode after restart = %#o", info.Mode().Perm())
	}
	assertPathMissing(t, filepath.Join(trackedDestination, "package.json"))
	if got := readFileStr(t, filepath.Join(trackedDestination, "nested", "incoming.txt")); got != "incoming\n" {
		t.Fatalf("replacement content after restart = %q", got)
	}
}

func TestE2EFilesystemDirectoryRenameConcurrentAccess(t *testing.T) {
	repo := newMountedE2ERepo(t)
	left := filepath.Join(repo.mountPath, "concurrent-left")
	right := filepath.Join(repo.mountPath, "concurrent-right")
	if err := os.MkdirAll(filepath.Join(left, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(left, "nested", "value.txt"), []byte("stable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(left)
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	errCh := make(chan error, 1)
	var workers sync.WaitGroup
	var started sync.WaitGroup
	started.Add(5)
	report := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}
	for range 4 {
		workers.Go(func() {
			started.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, root := range []string{left, right} {
					if _, err := os.Stat(root); err != nil && !os.IsNotExist(err) {
						report(fmt.Errorf("stat %q during rename: %w", root, err))
						return
					}
					if _, err := os.ReadDir(root); err != nil && !os.IsNotExist(err) {
						report(fmt.Errorf("readdir %q during rename: %w", root, err))
						return
					}
					data, err := os.ReadFile(filepath.Join(root, "nested", "value.txt"))
					if err != nil {
						if !os.IsNotExist(err) {
							report(fmt.Errorf("read %q during rename: %w", root, err))
							return
						}
					} else if string(data) != "stable\n" {
						report(fmt.Errorf("read %q during rename = %q", root, data))
						return
					}
				}
			}
		})
	}
	workers.Go(func() {
		started.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			cmd := exec.Command("git", "status", "--porcelain=v1", "--untracked-files=all")
			cmd.Dir = repo.mountPath
			if output, err := cmd.CombinedOutput(); err != nil {
				report(fmt.Errorf("git status during rename: %w: %s", err, output))
				return
			}
		}
	})
	started.Wait()

	current, next := left, right
	var renameErr error
	for range 40 {
		if renameErr = unix.Rename(current, next); renameErr != nil {
			break
		}
		current, next = next, current
		time.Sleep(time.Millisecond)
	}
	close(stop)
	workers.Wait()
	if renameErr != nil {
		t.Fatalf("concurrent rename: %v", renameErr)
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
	assertPathMissing(t, next)
	if got := readFileStr(t, filepath.Join(current, "nested", "value.txt")); got != "stable\n" {
		t.Fatalf("final concurrent content = %q", got)
	}
	final, err := os.Stat(current)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(original, final) {
		t.Fatal("directory inode identity changed during concurrent rename stress")
	}
}

func testE2ECLIDirectoryMove(t *testing.T) {
	repo := newMountedE2ERepo(t)
	source := filepath.Join(repo.mountPath, "tree")
	deep := filepath.Join(source, "nested", "deeper")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(deep, "payload.bin")
	initialBinary := []byte{0x00, 0x01, 0x02, 0xfe, 0xff}
	if err := os.WriteFile(binaryPath, initialBinary, 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(source, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho moved\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(scriptPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("nested", "deeper", "payload.bin"), filepath.Join(source, "payload-link")); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(repo.mountPath, "tree-sibling")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "untouched.txt"), []byte("sibling\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	binaryInfo, err := os.Stat(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	openFile, err := os.OpenFile(binaryPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer openFile.Close()
	openDir, err := os.Open(deep)
	if err != nil {
		t.Fatal(err)
	}
	defer openDir.Close()
	dirFD, err := unix.Open(deep, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(dirFD)

	run(t, repo.mountPath, "mv", "tree", "tree-moved")
	moved := filepath.Join(repo.mountPath, "tree-moved")
	movedBinary := filepath.Join(moved, "nested", "deeper", "payload.bin")
	assertPathMissing(t, source)
	assertPathExists(t, movedBinary)
	movedInfo, err := os.Stat(moved)
	if err != nil {
		t.Fatal(err)
	}
	movedBinaryInfo, err := os.Stat(movedBinary)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sourceInfo, movedInfo) {
		t.Fatal("directory inode identity changed across CLI mv")
	}
	if !os.SameFile(binaryInfo, movedBinaryInfo) {
		t.Fatal("descendant inode identity changed across CLI mv")
	}
	if _, err := openFile.WriteAt([]byte{0xaa, 0xbb}, 1); err != nil {
		t.Fatalf("write through moved open handle: %v", err)
	}
	if err := openFile.Sync(); err != nil {
		t.Fatalf("sync moved open handle: %v", err)
	}
	if _, err := openFile.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	gotThroughHandle, err := io.ReadAll(openFile)
	if err != nil {
		t.Fatal(err)
	}
	wantBinary := []byte{0x00, 0xaa, 0xbb, 0xfe, 0xff}
	if !bytes.Equal(gotThroughHandle, wantBinary) {
		t.Fatalf("open handle content = %v, want %v", gotThroughHandle, wantBinary)
	}
	if got, err := os.ReadFile(movedBinary); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, wantBinary) {
		t.Fatalf("moved binary content = %v, want %v", got, wantBinary)
	}
	names, err := openDir.Readdirnames(-1)
	if err != nil {
		t.Fatalf("read moved open directory handle: %v", err)
	}
	if !slices.Contains(names, "payload.bin") {
		t.Fatalf("moved directory handle entries = %v", names)
	}
	childFD, err := unix.Openat(dirFD, "payload.bin", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("openat through moved directory descriptor: %v", err)
	}
	childFile := os.NewFile(uintptr(childFD), "moved-payload")
	gotThroughDirFD, err := io.ReadAll(childFile)
	closeErr := childFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if !bytes.Equal(gotThroughDirFD, wantBinary) {
		t.Fatalf("openat content = %v, want %v", gotThroughDirFD, wantBinary)
	}
	if target, err := os.Readlink(filepath.Join(moved, "payload-link")); err != nil {
		t.Fatal(err)
	} else if target != filepath.Join("nested", "deeper", "payload.bin") {
		t.Fatalf("moved symlink target = %q", target)
	}
	if info, err := os.Stat(filepath.Join(moved, "run.sh")); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("moved executable mode = %#o", info.Mode().Perm())
	}
	if got := readFileStr(t, filepath.Join(sibling, "untouched.txt")); got != "sibling\n" {
		t.Fatalf("prefix sibling content = %q", got)
	}
}

func testE2ERecursiveCopyMoveAndReplace(t *testing.T) {
	repo := newMountedE2ERepo(t)
	run(t, repo.mountPath, "cp", "-R", "packages/wrangler", "wrangler-copy")
	copied := filepath.Join(repo.mountPath, "wrangler-copy")
	if got := readFileStr(t, filepath.Join(copied, "nested", "deep", "config.json")); !strings.Contains(got, "compatibility_date") {
		t.Fatalf("recursive copy nested content = %q", got)
	}
	if target, err := os.Readlink(filepath.Join(copied, "manifest-link")); err != nil {
		t.Fatal(err)
	} else if target != "package.json" {
		t.Fatalf("recursive copy symlink target = %q", target)
	}
	run(t, repo.mountPath, "mv", "wrangler-copy", "wrangler-copy-moved")
	copiedMoved := filepath.Join(repo.mountPath, "wrangler-copy-moved")
	assertPathMissing(t, copied)
	assertPathExists(t, filepath.Join(copiedMoved, "src", "index.js"))

	emptyDestination := filepath.Join(repo.mountPath, "empty-destination")
	if err := os.Mkdir(emptyDestination, 0o755); err != nil {
		t.Fatal(err)
	}
	replacedHandle, err := os.Open(emptyDestination)
	if err != nil {
		t.Fatal(err)
	}
	defer replacedHandle.Close()
	replacedInfo, err := os.Stat(emptyDestination)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Rename(copiedMoved, emptyDestination); err != nil {
		sourceEntries, sourceReadErr := os.ReadDir(copiedMoved)
		destinationEntries, destinationReadErr := os.ReadDir(emptyDestination)
		t.Fatalf(
			"replace empty destination: %v (source entries=%v, source read error=%v, destination entries=%v, destination read error=%v)",
			err,
			dirEntryNames(sourceEntries),
			sourceReadErr,
			dirEntryNames(destinationEntries),
			destinationReadErr,
		)
	}
	replacementInfo, err := os.Stat(emptyDestination)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(replacedInfo, replacementInfo) {
		t.Fatal("replaced destination retained its stale inode identity")
	}
	assertPathExists(t, filepath.Join(emptyDestination, "bin", "run.sh"))
	assertReplacedDirectoryHandleMatchesNative(t, replacedHandle, filepath.Join("bin", "run.sh"))
}

func assertReplacedDirectoryHandleMatchesNative(t *testing.T, mountedHandle *os.File, incomingChild string) {
	t.Helper()
	nativeRoot := t.TempDir()
	nativeSource := filepath.Join(nativeRoot, "source")
	nativeDestination := filepath.Join(nativeRoot, "destination")
	if err := os.MkdirAll(filepath.Join(nativeSource, filepath.Dir(incomingChild)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nativeSource, incomingChild), []byte("native\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(nativeDestination, 0o755); err != nil {
		t.Fatal(err)
	}
	nativeHandle, err := os.Open(nativeDestination)
	if err != nil {
		t.Fatal(err)
	}
	defer nativeHandle.Close()
	if err := unix.Rename(nativeSource, nativeDestination); err != nil {
		t.Fatalf("native replace empty destination: %v", err)
	}

	nativeNames, nativeReadErr := nativeHandle.Readdirnames(-1)
	mountedNames, mountedReadErr := mountedHandle.Readdirnames(-1)
	if nativeMissing, mountedMissing := os.IsNotExist(nativeReadErr), os.IsNotExist(mountedReadErr); nativeMissing != mountedMissing {
		t.Fatalf(
			"readdir replaced destination differs from native filesystem: native names=%v error=%v, mounted names=%v error=%v",
			nativeNames,
			nativeReadErr,
			mountedNames,
			mountedReadErr,
		)
	}
	if nativeReadErr != nil && !os.IsNotExist(nativeReadErr) {
		t.Fatalf("native readdir replaced destination: %v", nativeReadErr)
	}
	if mountedReadErr != nil && !os.IsNotExist(mountedReadErr) {
		t.Fatalf("mounted readdir replaced destination: %v", mountedReadErr)
	}
	if nativeReadErr == nil && !slices.Equal(nativeNames, mountedNames) {
		t.Fatalf("replaced destination entries differ from native filesystem: native=%v mounted=%v", nativeNames, mountedNames)
	}
	if len(nativeNames) != 0 || len(mountedNames) != 0 {
		t.Fatalf("replaced destination handle exposed entries: native=%v mounted=%v", nativeNames, mountedNames)
	}

	nativeChildFD, nativeOpenErr := unix.Openat(int(nativeHandle.Fd()), incomingChild, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if nativeChildFD >= 0 {
		_ = unix.Close(nativeChildFD)
	}
	mountedChildFD, mountedOpenErr := unix.Openat(int(mountedHandle.Fd()), incomingChild, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if mountedChildFD >= 0 {
		_ = unix.Close(mountedChildFD)
	}
	if !errors.Is(nativeOpenErr, unix.ENOENT) {
		t.Fatalf("native openat replaced destination = %v, want ENOENT", nativeOpenErr)
	}
	if !errors.Is(mountedOpenErr, unix.ENOENT) {
		t.Fatalf("mounted openat replaced destination = %v, want native ENOENT", mountedOpenErr)
	}
}

func testE2EReplaceVisibleEmptyTrackedDirectory(t *testing.T) {
	repo := newMountedE2ERepo(t)
	destination := filepath.Join(repo.mountPath, "packages", "miniflare")
	if err := os.Remove(filepath.Join(destination, "package.json")); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(destination); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("tracked destination entries = %v, want empty", dirEntryNames(entries))
	}
	replacedInfo, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(repo.mountPath, "visible-tracked-replacement")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "incoming.txt"), []byte("incoming\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Rename(source, destination); err != nil {
		t.Fatal(err)
	}
	assertPathMissing(t, source)
	assertPathMissing(t, filepath.Join(destination, "package.json"))
	if got := readFileStr(t, filepath.Join(destination, "nested", "incoming.txt")); got != "incoming\n" {
		t.Fatalf("visible tracked replacement content = %q", got)
	}
	replacementInfo, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(replacedInfo, replacementInfo) {
		t.Fatal("visible tracked destination retained its stale inode identity")
	}
}

func testE2EReplaceDeletedTrackedDirectory(t *testing.T) {
	repo := newMountedE2ERepo(t)
	run(t, repo.mountPath, "rm", "-rf", "packages/miniflare")
	trackedReplacementSource := filepath.Join(repo.mountPath, "tracked-replacement-source")
	if err := os.Mkdir(trackedReplacementSource, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trackedReplacementSource, "incoming.txt"), []byte("incoming\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repo.mountPath, "mv", "tracked-replacement-source", "packages/miniflare")
	assertPathExists(t, filepath.Join(repo.mountPath, "packages", "miniflare", "incoming.txt"))
	assertPathMissing(t, filepath.Join(repo.mountPath, "packages", "miniflare", "package.json"))
}

func testE2ERejectNonemptyDirectoryReplacement(t *testing.T) {
	repo := newMountedE2ERepo(t)
	rejectedSource := filepath.Join(repo.mountPath, "rejected-source")
	rejectedDestination := filepath.Join(repo.mountPath, "rejected-destination")
	if err := os.MkdirAll(rejectedSource, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rejectedSource, "source.txt"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rejectedDestination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rejectedDestination, "destination.txt"), []byte("destination\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Rename(rejectedSource, rejectedDestination); !errors.Is(err, unix.ENOTEMPTY) {
		t.Fatalf("rename over nonempty destination error = %v, want ENOTEMPTY", err)
	}
	if got := readFileStr(t, filepath.Join(rejectedSource, "source.txt")); got != "source\n" {
		t.Fatalf("rejected source content = %q", got)
	}
	if got := readFileStr(t, filepath.Join(rejectedDestination, "destination.txt")); got != "destination\n" {
		t.Fatalf("rejected destination content = %q", got)
	}
}

func testE2ENativeDirectoryRenameErrors(t *testing.T) {
	repo := newMountedE2ERepo(t)

	fileSource := filepath.Join(repo.mountPath, "file-source")
	directoryDestination := filepath.Join(repo.mountPath, "directory-destination")
	if err := os.WriteFile(fileSource, []byte("file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directoryDestination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Rename(fileSource, directoryDestination); !errors.Is(err, unix.EISDIR) {
		t.Fatalf("file over directory error = %v, want EISDIR", err)
	}

	directorySource := filepath.Join(repo.mountPath, "directory-source")
	fileDestination := filepath.Join(repo.mountPath, "file-destination")
	if err := os.Mkdir(directorySource, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileDestination, []byte("destination\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Rename(directorySource, fileDestination); !errors.Is(err, unix.ENOTDIR) {
		t.Fatalf("directory over file error = %v, want ENOTDIR", err)
	}

	parent := filepath.Join(repo.mountPath, "self-parent")
	if err := os.MkdirAll(filepath.Join(parent, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := unix.Rename(parent, filepath.Join(parent, "child", "moved")); !errors.Is(err, unix.EINVAL) {
		t.Fatalf("directory into descendant error = %v, want EINVAL", err)
	}

	missing := filepath.Join(repo.mountPath, "missing")
	if err := unix.Rename(missing, missing); !errors.Is(err, unix.ENOENT) {
		t.Fatalf("missing same-path rename error = %v, want ENOENT", err)
	}
	if err := unix.Rename(fileSource, fileSource); err != nil {
		t.Fatalf("existing same-path rename: %v", err)
	}
	if got := readFileStr(t, fileSource); got != "file\n" {
		t.Fatalf("same-path file content = %q", got)
	}

	oldParent := filepath.Join(repo.mountPath, "old-parent")
	newParent := filepath.Join(repo.mountPath, "new-parent")
	if err := os.MkdirAll(filepath.Join(oldParent, "tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newParent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldParent, "tree", "value.txt"), []byte("cross-parent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Rename(filepath.Join(oldParent, "tree"), filepath.Join(newParent, "tree")); err != nil {
		t.Fatalf("cross-parent directory rename: %v", err)
	}
	assertPathMissing(t, filepath.Join(oldParent, "tree"))
	if got := readFileStr(t, filepath.Join(newParent, "tree", "value.txt")); got != "cross-parent\n" {
		t.Fatalf("cross-parent content = %q", got)
	}
}

func dirEntryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestE2EVerifiedSource(t *testing.T) {
	if os.Getenv("AFS_RUN_E2E_TESTS") != "1" {
		t.Skip("skipping e2e tests (set AFS_RUN_E2E_TESTS=1 to run)")
	}
	skipIfNoFUSE(t)

	repoRoot := t.TempDir()
	remoteDir := filepath.Join(repoRoot, "verified.git")
	seedWork := filepath.Join(repoRoot, "seed")
	run(t, "", "git", "init", "--bare", remoteDir)
	run(t, "", "git", "clone", remoteDir, seedWork)
	run(t, seedWork, "git", "checkout", "-b", "main")
	writeTestFile(t, seedWork, "README.md", "# Test Repo\n")
	run(t, seedWork, "git", "add", "README.md")
	run(t, seedWork, "git", "-c", "user.name=E2E Test", "-c", "user.email=e2e@test", "commit", "-m", "initial")
	run(t, seedWork, "git", "push", "origin", "main")
	run(t, remoteDir, "git", "config", "uploadpack.allowFilter", "true")
	remoteURL := "file://" + remoteDir
	requiredCommit := strings.TrimSpace(run(t, "", "git", "--git-dir", remoteDir, "rev-parse", "refs/heads/main"))
	updateWork := filepath.Join(t.TempDir(), "update-work")
	run(t, "", "git", "clone", remoteURL, updateWork)
	run(t, updateWork, "git", "checkout", "main")

	root := t.TempDir()
	mountDir := t.TempDir()
	mountPath := filepath.Join(mountDir, repoName)
	cfg := model.RepoConfig{
		Name:                  repoName,
		ID:                    model.RepoID(repoName),
		RemoteURL:             remoteURL,
		RemoteURLRedacted:     auth.RedactRemoteURL(remoteURL),
		Branch:                "refs/heads/main",
		RequiredCommit:        requiredCommit,
		HistoryDepth:          1,
		RefreshInterval:       100 * time.Millisecond,
		RemoteRefreshDisabled: true,
		MountRoot:             mountDir,
		Enabled:               true,
	}

	firstCtx, firstCancel := context.WithCancel(context.Background())
	firstSvc, err := daemon.New(firstCtx, root, logging.NewJSONLogger(os.Stderr, slog.LevelWarn))
	if err != nil {
		t.Fatal(err)
	}
	firstSvc.SetMountRoot(mountDir)
	if err := firstSvc.AddRepo(firstCtx, cfg); err != nil {
		firstCancel()
		_ = firstSvc.Close()
		t.Fatalf("add verified repo: %v", err)
	}

	gitDir := filepath.Join(root, "repos", repoName, "git")
	blobCacheDir := filepath.Join(root, "cache", "blobs", repoName)
	if got := strings.Fields(run(t, "", "git", "--git-dir", gitDir, "ls-files")); !slices.Equal(got, []string{"README.md"}) {
		t.Fatalf("verified Git index before mount = %v, want [README.md]", got)
	}
	readmeOID := strings.TrimSpace(run(t, "", "git", "--git-dir", gitDir, "rev-parse", "HEAD:README.md"))
	readmeCachePath := filepath.Join(blobCacheDir, readmeOID)
	if _, err := os.Stat(readmeCachePath); !os.IsNotExist(err) {
		firstCancel()
		_ = firstSvc.Close()
		t.Fatalf("verified acquisition hydrated README before mount read: %v", err)
	}

	firstErrCh := make(chan error, 1)
	go func() { firstErrCh <- firstSvc.Start(firstCtx) }()
	if !waitForMount(t, mountPath, 60*time.Second) {
		firstCancel()
		_ = firstSvc.Close()
		t.Fatal("verified-source FUSE mount did not appear within timeout")
	}
	firstStopped := false
	defer func() {
		if !firstStopped {
			stopE2EDaemon(t, firstSvc, firstCancel, firstErrCh, mountPath)
		}
	}()
	if got, want := readFileStr(t, filepath.Join(mountPath, ".git")), "gitdir: "+gitDir+"\n"; got != want {
		t.Fatalf("mounted .git = %q, want %q", got, want)
	}
	if got := strings.Fields(gitCmd(t, mountPath, "ls-files")); !slices.Equal(got, []string{"README.md"}) {
		t.Fatalf("verified Git index through mount = %v, want [README.md]", got)
	}

	assertVerifiedSourceStatus(t, firstSvc, requiredCommit)
	if got := readFileStr(t, filepath.Join(mountPath, "README.md")); !strings.Contains(got, "Test Repo") {
		t.Fatalf("README.md = %q", got)
	}
	if _, err := os.Stat(readmeCachePath); err != nil {
		t.Fatalf("mounted read did not hydrate README: %v", err)
	}
	status, err := firstSvc.Status(context.Background(), repoName)
	if err != nil {
		t.Fatal(err)
	}
	if status.HydratedBlobCount == 0 {
		t.Fatalf("mounted status did not report hydrated README: %+v", status)
	}

	overlayReadme := "# local verified overlay\n"
	if err := os.WriteFile(filepath.Join(mountPath, "README.md"), []byte(overlayReadme), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountPath, "local-only.txt"), []byte("overlay\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = firstSvc.Status(context.Background(), repoName)
	if err != nil {
		t.Fatal(err)
	}
	if !status.DirtyOverlay {
		t.Fatalf("mounted status did not report dirty overlay: %+v", status)
	}
	assertGitStatus(t, mountPath, map[string]string{
		"README.md":      " M",
		"local-only.txt": "??",
	})
	gitCmd(t, mountPath, "add", "README.md", "local-only.txt")
	assertGitStatus(t, mountPath, map[string]string{
		"README.md":      "M ",
		"local-only.txt": "A ",
	})

	run(t, updateWork, "git", "fetch", "origin", "main")
	run(t, updateWork, "git", "checkout", "-B", "main", "origin/main")
	writeTestFile(t, updateWork, "REMOTE-ONLY.md", "new remote content\n")
	run(t, updateWork, "git", "add", "REMOTE-ONLY.md")
	run(t, updateWork, "git", "-c", "user.name=E2E Test", "-c", "user.email=e2e@test", "commit", "-m", "advance remote")
	run(t, updateWork, "git", "push", "origin", "main")
	time.Sleep(time.Second)
	assertVerifiedSourceStatus(t, firstSvc, requiredCommit)
	if _, err := os.Stat(filepath.Join(mountPath, "REMOTE-ONLY.md")); !os.IsNotExist(err) {
		t.Fatalf("disabled refresh exposed remote-only file: %v", err)
	}
	remoteTracking := exec.Command("git", "--git-dir", gitDir, "show-ref", "--verify", "--quiet", "refs/remotes/origin/main")
	if err := remoteTracking.Run(); err == nil {
		t.Fatal("disabled refresh fetched refs/remotes/origin/main")
	}

	alternateCommit := strings.TrimSpace(gitCmd(t, mountPath,
		"-c", "user.name=E2E Test",
		"-c", "user.email=e2e@test",
		"commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "local head change",
	))
	gitCmd(t, mountPath, "update-ref", "--no-deref", "HEAD", alternateCommit)
	time.Sleep(time.Second)
	assertVerifiedSourceStatus(t, firstSvc, requiredCommit)

	stopE2EDaemon(t, firstSvc, firstCancel, firstErrCh, mountPath)
	firstStopped = true

	secondCtx, secondCancel := context.WithCancel(context.Background())
	secondSvc, err := daemon.New(secondCtx, root, logging.NewJSONLogger(os.Stderr, slog.LevelWarn))
	if err != nil {
		t.Fatal(err)
	}
	secondSvc.SetMountRoot(mountDir)
	secondErrCh := make(chan error, 1)
	go func() { secondErrCh <- secondSvc.Start(secondCtx) }()
	if !waitForMount(t, mountPath, 60*time.Second) {
		secondCancel()
		_ = secondSvc.Close()
		t.Fatal("verified-source FUSE mount did not reappear after restart")
	}
	defer stopE2EDaemon(t, secondSvc, secondCancel, secondErrCh, mountPath)

	assertVerifiedSourceStatus(t, secondSvc, requiredCommit)
	if head := strings.TrimSpace(gitCmd(t, mountPath, "rev-parse", "HEAD")); head != requiredCommit {
		t.Fatalf("HEAD after restart = %q, want required commit %q", head, requiredCommit)
	}
	if got := readFileStr(t, filepath.Join(mountPath, "README.md")); got != overlayReadme {
		t.Fatalf("README overlay after restart = %q, want %q", got, overlayReadme)
	}
	if got := readFileStr(t, filepath.Join(mountPath, "local-only.txt")); got != "overlay\n" {
		t.Fatalf("local overlay after restart = %q", got)
	}
	assertGitStatus(t, mountPath, map[string]string{
		"README.md":      "M ",
		"local-only.txt": "A ",
	})
}

func assertVerifiedSourceStatus(t *testing.T, svc *daemon.Service, requiredCommit string) {
	t.Helper()
	status, err := svc.Status(context.Background(), repoName)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "mounted" ||
		status.SourceRef != "refs/heads/main" ||
		status.RequiredCommit != requiredCommit ||
		status.CurrentHEADOID != requiredCommit ||
		status.CurrentHEADRef != "refs/heads/main" ||
		status.Acquisition != "verified" ||
		!status.RemoteRefreshDisabled ||
		status.LastFetchResult != "never" {
		t.Fatalf("verified source status = %+v", status)
	}
}

func stopE2EDaemon(t *testing.T, svc *daemon.Service, cancel context.CancelFunc, errCh <-chan error, mountPath string) {
	t.Helper()
	cancel()
	if err := svc.Close(); err != nil {
		t.Errorf("close daemon: %v", err)
	}
	select {
	case <-errCh:
	case <-time.After(10 * time.Second):
		t.Error("daemon did not exit within 10s")
	}
	deadline := time.Now().Add(10 * time.Second)
	for isMounted(mountPath) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if isMounted(mountPath) {
		t.Errorf("mount remained active at %s", mountPath)
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
