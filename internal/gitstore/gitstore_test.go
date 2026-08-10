package gitstore

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
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

type stagedErrorContext struct {
	context.Context
	errAt int
	calls int
}

func (c *stagedErrorContext) Err() error {
	c.calls++
	if c.calls >= c.errAt {
		return context.Canceled
	}
	return nil
}

func TestResolveHEADAndBuildTreeIndex(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello"), 0o644)
	run(t, "git", "-C", repo, "add", "README.md")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oid, ref, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	if oid == "" || ref == "" {
		t.Fatalf("expected oid/ref, got %q %q", oid, ref)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	found := false
	for _, n := range nodes {
		if n.Path == "README.md" {
			found = true
			if n.Type != "file" {
				t.Fatalf("expected type file, got %q", n.Type)
			}
		}
	}
	if !found {
		t.Fatalf("expected README.md in tree")
	}
}

func TestBuildTreeIndexPreservesGitlinkAsDirectory(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "README.md")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	gitlinkOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "update-index", "--add", "--cacheinfo", "160000,"+gitlinkOID+",vendor/dependency")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "add gitlink")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
	store := New(nil)
	headOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	nodes, err := store.BuildTreeIndex(context.Background(), cfg, headOID)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	for _, node := range nodes {
		if node.Path != "vendor/dependency" {
			continue
		}
		if node.Type != "dir" || node.Mode != 0o160000 || node.ObjectOID != gitlinkOID {
			t.Fatalf("gitlink node = %#v", node)
		}
		if node.SizeState != "known" || node.SizeBytes != 0 {
			t.Fatalf("gitlink size = %q/%d, want known/0", node.SizeState, node.SizeBytes)
		}
		return
	}
	t.Fatal("gitlink missing from tree")
}

func TestConfigureStatusOptimizationDoesNotWriteIndex(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "README.md")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	gitDir := filepath.Join(repo, ".git")
	indexLock := filepath.Join(gitDir, "index.lock")
	if err := os.WriteFile(indexLock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := model.RepoConfig{ID: "repo", Name: "repo", GitDir: gitDir, MountPath: repo}
	if err := New(nil).ConfigureStatusOptimization(ctx, cfg, tmp); err != nil {
		t.Fatalf("ConfigureStatusOptimization: %v", err)
	}
	if _, err := os.Stat(indexLock); err != nil {
		t.Fatalf("daemon changed caller-owned index lock: %v", err)
	}
	if _, err := runGit(ctx, gitDir, "config", "--unset", "core.fsmonitor"); err != nil {
		t.Fatalf("disable test fsmonitor hook: %v", err)
	}
	if err := os.Remove(indexLock); err != nil {
		t.Fatal(err)
	}
}

func TestFSMonitorHookScriptQuotesArgs(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	pwned := filepath.Join(tmp, "pwned")
	hookPath := filepath.Join(tmp, "artifact-fs-fsmonitor")
	script := fsmonitorHookScript(tmp, "/bin/false", "repo$(touch "+pwned+")")
	if err := os.WriteFile(hookPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	_ = exec.Command("sh", hookPath).Run()
	if _, err := os.Stat(pwned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated hook allowed shell command substitution; stat err = %v", err)
	}
}

func TestBlobToCacheBinarySafe(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	// Write a file ending with a newline (should be preserved)
	os.WriteFile(filepath.Join(repo, "file.txt"), []byte("line\n"), 0o644)
	run(t, "git", "-C", repo, "add", "file.txt")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git"), BlobCacheDir: filepath.Join(tmp, "cache")}
	store := New(nil)
	ctx := context.Background()
	oid, _, _ := store.ResolveHEAD(ctx, cfg)
	nodes, _ := store.BuildTreeIndex(ctx, cfg, oid)
	var blobOID string
	for _, n := range nodes {
		if n.Path == "file.txt" {
			blobOID = n.ObjectOID
		}
	}
	if blobOID == "" {
		t.Fatal("no blob OID found")
	}
	dst := filepath.Join(tmp, "cache", blobOID)
	size, err := store.BlobToCache(ctx, cfg, blobOID, dst)
	if err != nil {
		t.Fatalf("BlobToCache: %v", err)
	}
	if size != 5 {
		t.Fatalf("expected size 5 (line\\n), got %d", size)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "line\n" {
		t.Fatalf("expected 'line\\n', got %q", data)
	}
}

func TestBlobToCacheHonorsCanceledContext(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	os.WriteFile(filepath.Join(repo, "file.txt"), []byte("line\n"), 0o644)
	run(t, "git", "-C", repo, "add", "file.txt")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git"), BlobCacheDir: filepath.Join(tmp, "cache")}
	store := New(nil)
	oid, _, err := store.ResolveHEAD(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.BuildTreeIndex(context.Background(), cfg, oid)
	if err != nil {
		t.Fatal(err)
	}
	var blobOID string
	for _, n := range nodes {
		if n.Path == "file.txt" {
			blobOID = n.ObjectOID
		}
	}
	if blobOID == "" {
		t.Fatal("no blob OID found")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := filepath.Join(tmp, "cache", blobOID)
	_, err = store.BlobToCache(ctx, cfg, blobOID, dst)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache file should not be written after cancellation: %v", err)
	}
}

func TestReadBlobRespectsMaxBytes(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	os.WriteFile(filepath.Join(repo, "file.txt"), []byte("line\n"), 0o644)
	run(t, "git", "-C", repo, "add", "file.txt")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
	store := New(nil)
	ctx := context.Background()
	oid, _, _ := store.ResolveHEAD(ctx, cfg)
	nodes, _ := store.BuildTreeIndex(ctx, cfg, oid)
	var blobOID string
	for _, n := range nodes {
		if n.Path == "file.txt" {
			blobOID = n.ObjectOID
		}
	}
	if blobOID == "" {
		t.Fatal("no blob OID found")
	}

	data, err := store.ReadBlob(ctx, cfg, blobOID, 5)
	if err != nil {
		t.Fatalf("ReadBlob at limit: %v", err)
	}
	if string(data) != "line\n" {
		t.Fatalf("data = %q, want line\\n", data)
	}
	_, err = store.ReadBlob(ctx, cfg, blobOID, 4)
	if !errors.Is(err, model.ErrBlobTooLarge) {
		t.Fatalf("err = %v, want ErrBlobTooLarge", err)
	}
	data, err = store.ReadBlob(ctx, cfg, blobOID, 5)
	if err != nil {
		t.Fatalf("ReadBlob after oversized read: %v", err)
	}
	if string(data) != "line\n" {
		t.Fatalf("data after oversized read = %q, want line\\n", data)
	}
}

func TestBuildTreeIndexNonASCIIPaths(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	// Create files with non-ASCII names that git would C-quote without -z.
	os.WriteFile(filepath.Join(repo, "café.txt"), []byte("latte"), 0o644)
	os.WriteFile(filepath.Join(repo, "日本語.md"), []byte("hello"), 0o644)
	run(t, "git", "-C", repo, "add", ".")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "non-ascii files")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oid, _, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, n := range nodes {
		paths[n.Path] = true
	}
	if !paths["café.txt"] {
		t.Fatalf("expected café.txt in tree, got paths: %v", paths)
	}
	if !paths["日本語.md"] {
		t.Fatalf("expected 日本語.md in tree, got paths: %v", paths)
	}
}

func TestCommitTimestamp(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644)
	run(t, "git", "-C", repo, "add", ".")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oid, _, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts, err := store.CommitTimestamp(ctx, cfg, oid)
	if err != nil {
		t.Fatal(err)
	}
	// Timestamp should be recent (within last minute).
	now := time.Now().Unix()
	if ts < now-60 || ts > now+60 {
		t.Fatalf("timestamp %d not within 60s of now %d", ts, now)
	}
}

func TestReadTreeHEAD(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	run(t, "git", "init", repo)
	os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644)
	run(t, "git", "-C", repo, "add", ".")
	run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
	store := New(nil)
	ctx := context.Background()
	// Should not error on a clean repo.
	if err := store.ReadTreeHEAD(ctx, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureIndexInitializedPreservesStagedEntries(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "intent.txt"), []byte("intent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "add", "-N", "intent.txt")

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
	if err := New(nil).EnsureIndexInitialized(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	staged := runOutput(t, "git", "-C", repo, "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "tracked.txt") {
		t.Fatalf("staged entries = %q, want tracked.txt", staged)
	}
	debug := runOutput(t, "git", "-C", repo, "ls-files", "--debug", "intent.txt")
	if !strings.Contains(debug, "flags: 20004000") {
		t.Fatalf("intent-to-add entry was not preserved:\n%s", debug)
	}
}

func TestEnsureIndexInitializedDoesNotRunGitWhenIndexExists(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	if _, err := os.Stat(filepath.Join(repo, ".git", "index")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initial index stat error = %v, want not exist", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
	if err := New(nil).EnsureIndexInitialized(context.Background(), cfg); err != nil {
		t.Fatalf("EnsureIndexInitialized invoked git for existing index: %v", err)
	}
}

func TestEnsureIndexInitializedHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := New(nil).EnsureIndexInitialized(ctx, model.RepoConfig{GitDir: t.TempDir()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureIndexInitialized error = %v, want context canceled", err)
	}
}

func TestEnsureIndexInitializedCreatesMissingIndex(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	indexPath := filepath.Join(repo, ".git", "index")
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}

	cfg := model.RepoConfig{ID: "x", GitDir: filepath.Join(repo, ".git")}
	if err := New(nil).EnsureIndexInitialized(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runOutput(t, "git", "-C", repo, "ls-files")); got != "tracked.txt" {
		t.Fatalf("index entries = %q, want tracked.txt", got)
	}
}

func TestPrepareFetchedBranchPreservesStagedEntries(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", "--initial-branch", "main", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	run(t, "git", "-C", repo, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "feature.txt")
	run(t, "git", "-C", repo, "commit", "-m", "feature")
	featureOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "checkout", "main")
	run(t, "git", "-C", repo, "update-ref", "refs/remotes/origin/feature", featureOID)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "intent.txt"), []byte("intent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "add", "-N", "intent.txt")

	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: filepath.Join(repo, ".git")}
	if err := New(nil).PrepareFetchedBranch(context.Background(), cfg, "feature"); err != nil {
		t.Fatal(err)
	}
	staged := runOutput(t, "git", "-C", repo, "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "tracked.txt") {
		t.Fatalf("staged entries = %q, want tracked.txt", staged)
	}
	debug := runOutput(t, "git", "-C", repo, "ls-files", "--debug", "intent.txt")
	if !strings.Contains(debug, "flags: 20004000") {
		t.Fatalf("intent-to-add entry was not preserved:\n%s", debug)
	}
}

func TestPrepareFetchedBranchConflictDoesNotMoveHEAD(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", "--initial-branch", "main", repo)
	run(t, "git", "-C", repo, "config", "user.name", "test")
	run(t, "git", "-C", repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "base")
	baseOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))

	run(t, "git", "-C", repo, "checkout", "-b", "future")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("remote\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")
	run(t, "git", "-C", repo, "commit", "-m", "remote")
	futureOID := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD"))
	run(t, "git", "-C", repo, "checkout", "main")
	run(t, "git", "-C", repo, "branch", "-D", "future")
	run(t, "git", "-C", repo, "update-ref", "refs/remotes/origin/feature", futureOID)

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", repo, "add", "tracked.txt")

	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: filepath.Join(repo, ".git")}
	err := New(nil).PrepareFetchedBranch(context.Background(), cfg, "feature")
	if err == nil {
		t.Fatal("expected staged conflict")
	}
	if head := strings.TrimSpace(runOutput(t, "git", "-C", repo, "rev-parse", "HEAD")); head != baseOID {
		t.Fatalf("HEAD = %s, want %s", head, baseOID)
	}
	if ref := strings.TrimSpace(runOutput(t, "git", "-C", repo, "symbolic-ref", "--short", "HEAD")); ref != "main" {
		t.Fatalf("HEAD ref = %q, want main", ref)
	}
	if staged := runOutput(t, "git", "-C", repo, "show", ":tracked.txt"); staged != "staged\n" {
		t.Fatalf("staged content = %q, want staged", staged)
	}
	if err := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/feature").Run(); err == nil {
		t.Fatal("feature branch was created despite index conflict")
	}
}

func TestFetchRefNonInteractiveAndPrepareFetchedBranch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "master")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644)
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	run(t, "git", "-C", work, "push", "origin", "master")

	run(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
	run(t, "git", "-C", preparedWorktree, "remote", "add", "origin", "file://"+bare)

	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: preparedGitDir, Branch: "master"}
	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := store.ValidatePreparedGitDir(ctx, cfg); err != nil {
		t.Fatalf("ValidatePreparedGitDir: %v", err)
	}
	if err := store.FetchRefNonInteractive(ctx, cfg, "master"); err != nil {
		t.Fatalf("FetchRefNonInteractive: %v", err)
	}
	if err := store.PrepareFetchedBranch(ctx, cfg, "master"); err != nil {
		t.Fatalf("PrepareFetchedBranch: %v", err)
	}
	oid, ref, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	if ref != "master" {
		t.Fatalf("ref = %q, want master", ref)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	found := false
	for _, n := range nodes {
		if n.Path == "README.md" {
			found = true
		}
	}
	if !found {
		t.Fatal("README.md not found in prepared tree")
	}
}

func TestPrepareFetchedBranchRefusesPreparedGitDirRewind(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "master")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("origin\n"), 0o644)
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "origin")
	run(t, "git", "-C", work, "push", "origin", "master")

	run(t, "git", "clone", bare, preparedWorktree)
	run(t, "git", "-C", preparedWorktree, "checkout", "master")
	localPath := filepath.Join(preparedWorktree, "LOCAL.md")
	os.WriteFile(localPath, []byte("local\n"), 0o644)
	run(t, "git", "-C", preparedWorktree, "add", "LOCAL.md")
	run(t, "git", "-C", preparedWorktree, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "local")
	if err := os.Rename(filepath.Join(preparedWorktree, ".git"), preparedGitDir); err != nil {
		t.Fatal(err)
	}

	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: preparedGitDir, Branch: "master", PreparedGitDir: true}
	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	localOID, err := runGit(ctx, preparedGitDir, "rev-parse", "refs/heads/master")
	if err != nil {
		t.Fatal(err)
	}
	localOID = strings.TrimSpace(localOID)

	if err := store.FetchRefNonInteractive(ctx, cfg, "master"); err != nil {
		t.Fatalf("FetchRefNonInteractive: %v", err)
	}
	err = store.PrepareFetchedBranch(ctx, cfg, "master")
	if err == nil {
		t.Fatal("expected non-fast-forward prepared branch update to fail")
	}
	if strings.Contains(err.Error(), localOID) {
		t.Fatalf("error leaked commit details: %v", err)
	}
	afterOID, err := runGit(ctx, preparedGitDir, "rev-parse", "refs/heads/master")
	if err != nil {
		t.Fatal(err)
	}
	afterOID = strings.TrimSpace(afterOID)
	if afterOID != localOID {
		t.Fatalf("prepared branch moved to %s, want %s", afterOID, localOID)
	}
}

func TestZeroOIDForGitDirMatchesObjectFormat(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			repo := filepath.Join(t.TempDir(), "repo")
			initArgs := []string{"init"}
			if objectFormat == "sha256" {
				initArgs = append(initArgs, "--object-format=sha256")
			}
			initArgs = append(initArgs, repo)
			run(t, "git", initArgs...)
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte(objectFormat+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, "git", "-C", repo, "add", "README.md")
			run(t, "git", "-C", repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", objectFormat)

			gitDir := filepath.Join(repo, ".git")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			zero, err := zeroOIDForGitDir(ctx, gitDir)
			if err != nil {
				t.Fatal(err)
			}
			wantLength := 40
			if objectFormat == "sha256" {
				wantLength = 64
			}
			if len(zero) != wantLength || strings.Trim(zero, "0") != "" {
				t.Fatalf("zero OID = %q, want %d zeros", zero, wantLength)
			}
			head, err := runGit(ctx, gitDir, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runGit(ctx, gitDir, "update-ref", "refs/heads/prepared", head, zero); err != nil {
				t.Fatalf("create prepared branch with zero OID: %v", err)
			}
		})
	}
}

func TestPrepareExistingCloneNonInteractiveUpdatesBranch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	gitDir := filepath.Join(tmp, "repo.git")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "master")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("master\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "master")
	run(t, "git", "-C", work, "push", "origin", "master")
	run(t, "git", "-C", work, "checkout", "-b", "dev")
	os.WriteFile(filepath.Join(work, "DEV.md"), []byte("dev\n"), 0o644)
	run(t, "git", "-C", work, "add", "DEV.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "dev")
	run(t, "git", "-C", work, "push", "origin", "dev")

	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: gitDir, RemoteURL: "file://" + bare, Branch: "master", FetchRef: "master"}
	if err := store.CloneBloblessNonInteractive(ctx, cfg); err != nil {
		t.Fatalf("CloneBloblessNonInteractive: %v", err)
	}
	assertArtifactWorktreeConfig(t, ctx, gitDir)
	cfg.Branch = "dev"
	cfg.FetchRef = "dev"
	if err := store.PrepareExistingCloneNonInteractive(ctx, cfg); err != nil {
		t.Fatalf("PrepareExistingCloneNonInteractive: %v", err)
	}
	oid, ref, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	if ref != "dev" {
		t.Fatalf("ref = %q, want dev", ref)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	found := false
	for _, n := range nodes {
		if n.Path == "DEV.md" {
			found = true
		}
	}
	if !found {
		t.Fatal("DEV.md not found after existing clone prepare")
	}
}

func TestPrepareSourceIsShallowAndVerifiesRequiredCommit(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "main")
	for i, content := range []string{"one\n", "two\n", "three\n"} {
		if err := os.WriteFile(filepath.Join(work, "README.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, "git", "-C", work, "add", "README.md")
		run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", fmt.Sprintf("commit %d", i+1))
	}
	run(t, "git", "-C", work, "push", "origin", "main")
	run(t, "git", "-C", work, "checkout", "-b", "side", "HEAD~2")
	if err := os.WriteFile(filepath.Join(work, "SIDE.md"), []byte("side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", work, "add", "SIDE.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "side")
	run(t, "git", "-C", work, "push", "origin", "side")
	run(t, "git", "-C", work, "checkout", "main")
	run(t, "git", "-C", bare, "config", "uploadpack.allowFilter", "true")

	tip := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
	blobOID := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD:README.md"))
	gitDir := filepath.Join(tmp, "snapshot.git")
	cfg := model.RepoConfig{
		ID:             "verified",
		Name:           "verified",
		GitDir:         gitDir,
		RemoteURL:      "file://" + bare,
		Branch:         "refs/heads/main",
		RequiredCommit: tip,
		HistoryDepth:   1,
		MountPath:      filepath.Join(tmp, "mounted"),
	}
	requirement := model.SourceRequirement{Ref: cfg.Branch, RequiredCommit: tip, Depth: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := New(nil)
	prepared, err := store.PrepareSource(ctx, cfg, requirement)
	if err != nil {
		t.Fatalf("PrepareSource: %v", err)
	}
	assertArtifactWorktreeConfig(t, ctx, gitDir)
	if !prepared.Verified || !prepared.Acquired || prepared.Ref != cfg.Branch || prepared.Commit != tip {
		t.Fatalf("prepared source = %+v", prepared)
	}
	mountedWorktree := cfg.MountPath
	if err := os.MkdirAll(mountedWorktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountedWorktree, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigureStatusOptimization(ctx, cfg, tmp); err != nil {
		t.Fatalf("ConfigureStatusOptimization: %v", err)
	}
	if inside := strings.TrimSpace(runOutput(t, "git", "-C", mountedWorktree, "rev-parse", "--is-inside-work-tree")); inside != "true" {
		t.Fatalf("mounted Git directory reported is-inside-work-tree = %q, want true", inside)
	}
	configuredWorktree, err := runGit(ctx, gitDir, "config", "--path", "core.worktree")
	if err != nil {
		t.Fatal(err)
	}
	resolvedWorktree, err := filepath.EvalSymlinks(configuredWorktree)
	if err != nil {
		t.Fatal(err)
	}
	wantWorktree, err := filepath.EvalSymlinks(mountedWorktree)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedWorktree != wantWorktree {
		t.Fatalf("core.worktree = %q, want %q", resolvedWorktree, wantWorktree)
	}

	count, err := runGit(ctx, gitDir, "rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if count != "1" {
		t.Fatalf("snapshot history count = %q, want 1", count)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "shallow")); err != nil {
		t.Fatalf("snapshot clone is not shallow: %v", err)
	}
	fetchRef, err := runGit(ctx, gitDir, "config", "--get-all", "remote.origin.fetch")
	if err != nil {
		t.Fatal(err)
	}
	wantFetchRef := "+refs/heads/main:" + verifiedSourceTrackingRef
	if fetchRef != wantFetchRef {
		t.Fatalf("remote.origin.fetch = %q, want %q", fetchRef, wantFetchRef)
	}
	// The generated hook targets os.Executable, which is the test runner here.
	// Disable it before fetch so newer Git versions do not recurse into tests.
	if _, err := runGit(ctx, gitDir, "config", "--unset", "core.fsmonitor"); err != nil {
		t.Fatalf("disable test fsmonitor hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "NEW.md"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", work, "add", "NEW.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "advance main")
	run(t, "git", "-C", work, "push", "origin", "main")
	advancedTip := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
	if err := New(nil).Fetch(ctx, cfg); err != nil {
		t.Fatalf("verified source refresh: %v", err)
	}
	count, err = runGit(ctx, gitDir, "rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if count != "1" {
		t.Fatalf("snapshot history count after refresh = %q, want 1", count)
	}
	trackingTip, err := runGit(ctx, gitDir, "rev-parse", verifiedSourceTrackingRef)
	if err != nil {
		t.Fatal(err)
	}
	if trackingTip != advancedTip {
		t.Fatalf("tracking ref = %q, want advanced source %q", trackingTip, advancedTip)
	}
	trackingCount, err := runGit(ctx, gitDir, "rev-list", "--count", verifiedSourceTrackingRef)
	if err != nil {
		t.Fatal(err)
	}
	if trackingCount != "1" {
		t.Fatalf("tracking history count after refresh = %q, want 1", trackingCount)
	}
	if _, err := runGit(ctx, gitDir, "show-ref", "--verify", "refs/remotes/origin/side"); err == nil {
		t.Fatal("verified source refresh fetched an unrelated branch")
	}
	cmd := exec.CommandContext(ctx, "git", "cat-file", "-e", blobOID)
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir, "GIT_NO_LAZY_FETCH=1")
	if err := cmd.Run(); err == nil {
		t.Fatal("snapshot clone eagerly downloaded the HEAD blob")
	}

	// A server without partial-clone filtering still honors depth=1. Git falls
	// back to transferring the tip's blobs, but not the branch's full history.
	run(t, "git", "-C", bare, "config", "uploadpack.allowFilter", "false")
	fallbackDir := filepath.Join(tmp, "fallback.git")
	cfg.GitDir = fallbackDir
	requirement.RequiredCommit = advancedTip
	if _, err := New(nil).PrepareSource(ctx, cfg, requirement); err != nil {
		t.Fatalf("PrepareSource fallback: %v", err)
	}
	fallbackCount, err := runGit(ctx, fallbackDir, "rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if fallbackCount != "1" {
		t.Fatalf("fallback snapshot history count = %q, want 1", fallbackCount)
	}
	cmd = exec.CommandContext(ctx, "git", "cat-file", "-e", blobOID)
	cmd.Env = append(os.Environ(), "GIT_DIR="+fallbackDir, "GIT_NO_LAZY_FETCH=1")
	if err := cmd.Run(); err != nil {
		t.Fatalf("fallback snapshot did not download the HEAD blob: %v", err)
	}

	mismatchDir := filepath.Join(tmp, "mismatch.git")
	cfg.GitDir = mismatchDir
	requirement.RequiredCommit = strings.Repeat("0", 40)
	_, err = New(nil).PrepareSource(ctx, cfg, requirement)
	if err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("mismatched source error = %v", err)
	}
	if _, statErr := os.Stat(mismatchDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("mismatched snapshot left git dir behind: %v", statErr)
	}
}

func TestPrepareSourceSupportsSHA256Remote(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	gitDir := filepath.Join(tmp, "snapshot.git")

	run(t, "git", "init", "--bare", "--object-format=sha256", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("sha256\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "sha256")
	run(t, "git", "-C", work, "push", "origin", "main")

	tip := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
	if len(tip) != 64 {
		t.Fatalf("SHA-256 commit length = %d, want 64", len(tip))
	}
	cfg := model.RepoConfig{
		ID:        "sha256",
		Name:      "sha256",
		GitDir:    gitDir,
		RemoteURL: "file://" + bare,
	}
	requirement := model.SourceRequirement{
		Ref:            "refs/heads/main",
		RequiredCommit: tip,
		Depth:          1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	prepared, err := New(nil).PrepareSource(ctx, cfg, requirement)
	if err != nil {
		t.Fatalf("PrepareSource: %v", err)
	}
	if prepared.Commit != tip || !prepared.Verified || !prepared.Acquired {
		t.Fatalf("prepared source = %+v", prepared)
	}
	format, err := runGit(ctx, gitDir, "rev-parse", "--show-object-format")
	if err != nil {
		t.Fatal(err)
	}
	if format != "sha256" {
		t.Fatalf("object format = %q, want sha256", format)
	}
	head, err := runGit(ctx, gitDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if head != tip {
		t.Fatalf("HEAD = %q, want %q", head, tip)
	}
}

func TestPrepareSourceReacquiresMissingOrCorruptReceiptGitDir(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("verified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "verified")
	run(t, "git", "-C", work, "push", "origin", "main")

	tip := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
	requirement := model.SourceRequirement{
		Ref:            "refs/heads/main",
		RequiredCommit: tip,
		Depth:          1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, test := range []struct {
		name   string
		damage func(t *testing.T, gitDir string)
	}{
		{
			name: "missing",
			damage: func(t *testing.T, gitDir string) {
				t.Helper()
				if err := os.RemoveAll(gitDir); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt",
			damage: func(t *testing.T, gitDir string) {
				t.Helper()
				if err := os.RemoveAll(gitDir); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(gitDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(gitDir, "not-a-repository"), []byte("corrupt\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt_index",
			damage: func(t *testing.T, gitDir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(gitDir, "index"), []byte("broken\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			gitDir := filepath.Join(tmp, test.name+".git")
			cfg := model.RepoConfig{
				ID:        model.RepoID(test.name),
				Name:      test.name,
				GitDir:    gitDir,
				RemoteURL: "file://" + bare,
			}
			store := New(nil)
			if _, err := store.PrepareSource(ctx, cfg, requirement); err != nil {
				t.Fatalf("initial PrepareSource: %v", err)
			}
			oldPool, err := store.getPool(gitDir)
			if err != nil {
				t.Fatal(err)
			}
			test.damage(t, gitDir)

			cfg.AcquiredRef = requirement.Ref
			cfg.AcquiredCommit = requirement.RequiredCommit
			prepared, err := store.PrepareSource(ctx, cfg, requirement)
			if err != nil {
				t.Fatalf("reacquire PrepareSource: %v", err)
			}
			if !prepared.Verified || !prepared.Acquired || prepared.Commit != tip {
				t.Fatalf("prepared source = %+v", prepared)
			}
			head, err := runGit(ctx, gitDir, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if head != tip {
				t.Fatalf("HEAD = %q, want %q", head, tip)
			}
			newPool, err := store.getPool(gitDir)
			if err != nil {
				t.Fatal(err)
			}
			if newPool == oldPool {
				t.Fatal("reacquisition reused the batch pool from the replaced Git directory")
			}
		})
	}
}

func TestPrepareSourceReceiptPreservesIndexAndRestoresHEAD(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	gitDir := filepath.Join(tmp, "snapshot.git")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "main")
	for _, content := range []string{"base\n", "required\n"} {
		if err := os.WriteFile(filepath.Join(work, "README.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, "git", "-C", work, "add", "README.md")
		run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", strings.TrimSpace(content))
	}
	run(t, "git", "-C", work, "push", "origin", "main")

	requiredCommit := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
	cfg := model.RepoConfig{
		ID:        "verified",
		Name:      "verified",
		GitDir:    gitDir,
		RemoteURL: "file://" + bare,
	}
	requirement := model.SourceRequirement{
		Ref:            "refs/heads/main",
		RequiredCommit: requiredCommit,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := New(nil)
	if _, err := store.PrepareSource(ctx, cfg, requirement); err != nil {
		t.Fatalf("initial PrepareSource: %v", err)
	}

	stagedPath := filepath.Join(tmp, "staged-readme")
	if err := os.WriteFile(stagedPath, []byte("staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stagedOID, err := runGit(ctx, gitDir, "hash-object", "-w", stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, gitDir, "update-index", "--add", "--cacheinfo", "100644", stagedOID, "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(ctx, gitDir, "update-ref", "--no-deref", "HEAD", requiredCommit+"^"); err != nil {
		t.Fatal(err)
	}

	cfg.AcquiredRef = requirement.Ref
	cfg.AcquiredCommit = requirement.RequiredCommit
	prepared, err := store.PrepareSource(ctx, cfg, requirement)
	if err != nil {
		t.Fatalf("receipt PrepareSource: %v", err)
	}
	if prepared.Acquired {
		t.Fatal("receipt reuse unexpectedly performed remote acquisition")
	}
	head, err := runGit(ctx, gitDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if head != requiredCommit {
		t.Fatalf("HEAD = %q, want required commit %q", head, requiredCommit)
	}
	staged, err := runGit(ctx, gitDir, "diff", "--cached", "--name-only", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if staged != "README.md" {
		t.Fatalf("staged paths = %q, want README.md", staged)
	}
}

func TestCloneBloblessUsesExactCanonicalTag(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			tmp := t.TempDir()
			bare := filepath.Join(tmp, "origin.git")
			work := filepath.Join(tmp, "work")
			gitDir := filepath.Join(tmp, "repo.git")

			initArgs := []string{"init", "--bare"}
			if objectFormat == "sha256" {
				initArgs = append(initArgs, "--object-format=sha256")
			}
			initArgs = append(initArgs, bare)
			run(t, "git", initArgs...)
			run(t, "git", "clone", bare, work)
			run(t, "git", "-C", work, "checkout", "-b", "main")
			if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("tag\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, "git", "-C", work, "add", "README.md")
			run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "tag target")
			tagCommit := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
			run(t, "git", "-C", work, "update-ref", "refs/tags/release", tagCommit)

			run(t, "git", "-C", work, "checkout", "-b", "release")
			if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("branch\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, "git", "-C", work, "add", "README.md")
			run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "same-named branch")
			branchCommit := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
			if branchCommit == tagCommit {
				t.Fatal("branch and tag unexpectedly resolve to the same commit")
			}
			run(t, "git", "-C", work, "push", "origin",
				"refs/heads/main:refs/heads/main",
				"refs/heads/release:refs/heads/release",
				"refs/tags/release:refs/tags/release",
			)
			run(t, "git", "-C", bare, "config", "uploadpack.allowFilter", "true")

			cfg := model.RepoConfig{
				ID:           "tag",
				Name:         "tag",
				GitDir:       gitDir,
				RemoteURL:    "file://" + bare,
				Branch:       "refs/tags/release",
				HistoryDepth: 1,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := New(nil).CloneBlobless(ctx, cfg); err != nil {
				t.Fatalf("CloneBlobless: %v", err)
			}
			head, err := runGit(ctx, gitDir, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if head != tagCommit {
				t.Fatalf("HEAD = %q, want tag commit %q; same-named branch is %q", head, tagCommit, branchCommit)
			}
			format, err := runGit(ctx, gitDir, "rev-parse", "--show-object-format")
			if err != nil {
				t.Fatal(err)
			}
			if format != objectFormat {
				t.Fatalf("object format = %q, want %q", format, objectFormat)
			}
			fetchRef, err := runGit(ctx, gitDir, "config", "--get-all", "remote.origin.fetch")
			if err != nil {
				t.Fatal(err)
			}
			wantFetchRef := "+refs/tags/release:" + fetchedFullRefRemoteTrackingRef
			if fetchRef != wantFetchRef {
				t.Fatalf("remote.origin.fetch = %q, want %q", fetchRef, wantFetchRef)
			}
		})
	}
}

func TestCloneAndFetchRefSkipTags(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	gitDir := filepath.Join(tmp, "repo.git")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "master")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("master\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "master")
	run(t, "git", "-C", work, "push", "origin", "master")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(tmp, "git.log")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GIT_COMMAND_LOG\"\nexec \"$REAL_GIT\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_COMMAND_LOG", logPath)
	t.Setenv("REAL_GIT", realGit)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: gitDir, RemoteURL: "file://" + bare, Branch: "master", FetchRef: "master"}
	oldPool, err := store.getPool(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CloneBloblessNonInteractive(ctx, cfg); err != nil {
		t.Fatalf("CloneBloblessNonInteractive: %v", err)
	}
	newPool, err := store.getPool(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	if newPool == oldPool {
		t.Fatal("clone reused the batch pool from the previously missing Git directory")
	}
	if err := store.FetchRefNonInteractive(ctx, cfg, cfg.FetchRef); err != nil {
		t.Fatalf("FetchRefNonInteractive: %v", err)
	}
	if err := store.Fetch(ctx, cfg); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "clone --filter=blob:none --no-checkout --single-branch --no-tags --branch master") {
		t.Fatalf("clone did not include --no-tags; git log:\n%s", logText)
	}
	if !strings.Contains(logText, "fetch --filter=blob:none --no-tags origin +refs/heads/master:refs/remotes/origin/master") {
		t.Fatalf("fetch did not include --no-tags; git log:\n%s", logText)
	}
	if !strings.Contains(logText, "fetch --no-tags origin") {
		t.Fatalf("refresh fetch did not include --no-tags; git log:\n%s", logText)
	}
}

func TestCloneBloblessRetriesTransientTransportErrors(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(tmp, "clone-count")
	commandPath := filepath.Join(tmp, "git-commands")
	fakeGit := filepath.Join(bin, "git")
	fakeGitScript := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$AFS_GIT_COMMANDS\"\n" +
		"case \"$1\" in\n" +
		"  clone)\n" +
		"    count=0; [ -f \"$AFS_CLONE_COUNT\" ] && count=$(cat \"$AFS_CLONE_COUNT\")\n" +
		"    count=$((count + 1)); printf '%s' \"$count\" > \"$AFS_CLONE_COUNT\"\n" +
		"    if [ \"$count\" -lt 3 ]; then\n" +
		"      printf '%s\\n' 'error: RPC failed; HTTP 503 curl 22' 'fetch-pack: unexpected disconnect while reading sideband packet' 'fatal: early EOF' 'fatal: index-pack failed' >&2\n" +
		"      exit 1\n" +
		"    fi\n" +
		"    for arg in \"$@\"; do dest=\"$arg\"; done\n" +
		"    mkdir -p \"$dest/.git\"\n" +
		"    ;;\n" +
		"  read-tree) ;;\n" +
		"esac\n"
	if err := os.WriteFile(fakeGit, []byte(fakeGitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_CLONE_COUNT", countPath)
	t.Setenv("AFS_GIT_COMMANDS", commandPath)

	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	store.gitRetryDelays = []time.Duration{0, 0}
	cfg := model.RepoConfig{
		Name:      "https://log-user:log-secret@example.com/openai-sites",
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "https://user:super-secret@example.com/org/repo.git",
		Branch:    "main",
	}
	if err := store.CloneBlobless(context.Background(), cfg); err != nil {
		t.Fatalf("CloneBlobless: %v", err)
	}
	count, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(count); got != "3" {
		t.Fatalf("clone attempts = %q, want 3", got)
	}
	commands, err := os.ReadFile(commandPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commands), "super-secret") || strings.Contains(string(commands), "user@example.com") {
		t.Fatalf("credential appeared in git command: %s", commands)
	}
	logOutput := logs.String()
	if !strings.Contains(logOutput, `"msg":"`+logGitOperationAttemptFailed+`"`) ||
		!strings.Contains(logOutput, `"msg":"`+logGitOperationRecovered+`"`) ||
		!strings.Contains(logOutput, `"error":"error: RPC failed; HTTP 503 curl 22`) ||
		!strings.Contains(logOutput, `"duration_ms":`) ||
		!strings.Contains(logOutput, `"next_attempt":2`) ||
		strings.Contains(logOutput, "super-secret") || strings.Contains(logOutput, "log-secret") {
		t.Fatalf("clone retry logs were not structured/redacted: %s", logs.String())
	}
}

func TestCloneBloblessDoesNotRetryPermanentError(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(tmp, "clone-count")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\ncount=0; [ -f \"$AFS_CLONE_COUNT\" ] && count=$(cat \"$AFS_CLONE_COUNT\")\ncount=$((count + 1)); printf '%s' \"$count\" > \"$AFS_CLONE_COUNT\"\nprintf '%s\\n' 'remote: credential-user' 'remote: super-secret' 'fatal: Authentication failed for https://credential-user:super-secret@example.com/org/repo.git; HTTP 401' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_CLONE_COUNT", countPath)

	store := New(nil)
	store.gitRetryDelays = []time.Duration{0, 0}
	err := store.CloneBlobless(context.Background(), model.RepoConfig{GitDir: filepath.Join(tmp, "repo.git"), RemoteURL: "https://credential-user:super-secret@example.com/org/repo.git", Branch: "main"})
	var operationErr *GitOperationError
	if !errors.As(err, &operationErr) {
		t.Fatalf("error = %T %v, want GitOperationError", err, err)
	}
	if operationErr.Operation != GitOperationClone || operationErr.Attempts != 1 || operationErr.Retryable {
		t.Fatalf("GitOperationError = %+v, want one non-retryable clone attempt", operationErr)
	}
	if strings.Contains(err.Error(), "credential-user") || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("clone error leaked credentials: %v", err)
	}
	count, readErr := os.ReadFile(countPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(count) != "1" {
		t.Fatalf("clone attempts = %q, want 1", count)
	}
}

func TestCloneBloblessPreservesTransportFailureAtCallerDeadline(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(tmp, "clone-count")
	fakeGit := filepath.Join(bin, "git")
	script := "#!/bin/sh\n" +
		"count=0; [ -f \"$AFS_CLONE_COUNT\" ] && count=$(cat \"$AFS_CLONE_COUNT\")\n" +
		"count=$((count + 1)); printf '%s' \"$count\" > \"$AFS_CLONE_COUNT\"\n" +
		"printf '%s\\n' 'error: RPC failed; HTTP 500 curl 22' >&2\n" +
		"exec sleep 10\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_CLONE_COUNT", countPath)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	err := store.CloneBlobless(ctx, model.RepoConfig{GitDir: filepath.Join(tmp, "repo.git"), RemoteURL: "https://example.com/org/repo.git", Branch: "main"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	var operationErr *GitOperationError
	if !errors.As(err, &operationErr) {
		t.Fatalf("error = %T %v, want GitOperationError", err, err)
	}
	if operationErr.Attempts != 1 || !operationErr.Retryable {
		t.Fatalf("GitOperationError = %+v, want one retryable attempt", operationErr)
	}
	if !strings.Contains(err.Error(), "HTTP 500") || strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("error did not safely preserve transport failure: %v", err)
	}
	count, readErr := os.ReadFile(countPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(count) != "1" {
		t.Fatalf("clone attempts = %q, want 1", count)
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, `"msg":"`+logGitOperationAttemptFailed+`"`) ||
		!strings.Contains(logOutput, `"timed_out":true`) ||
		!strings.Contains(logOutput, `"error":"error: RPC failed; HTTP 500 curl 22`) {
		t.Fatalf("deadline failure was not logged with its transport cause: %s", logOutput)
	}
}

func TestRetryGitOperationCancellationRacePreservesBothCauses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transportErr := errors.New("HTTP 500 transport failure")
	attempts := 0
	err := New(nil).retryGitOperation(ctx, GitOperationClone, "repo", func() error {
		attempts++
		cancel()
		return transportErr
	})
	if !errors.Is(err, transportErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want transport and cancellation causes", err)
	}
	var operationErr *GitOperationError
	if !errors.As(err, &operationErr) || !operationErr.Retryable || operationErr.Attempts != 1 {
		t.Fatalf("error = %#v, want one retryable attempt", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestRetryGitOperationCancellationBeforeNextAttemptPreservesFailure(t *testing.T) {
	ctx := &stagedErrorContext{Context: context.Background(), errAt: 3}
	transportErr := errors.New("HTTP 500 transport failure")
	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	store.gitRetryDelays = []time.Duration{0}
	attempts := 0
	err := store.retryGitOperation(ctx, GitOperationClone, "repo", func() error {
		attempts++
		return transportErr
	})
	if !errors.Is(err, transportErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want transport and cancellation causes", err)
	}
	var operationErr *GitOperationError
	if !errors.As(err, &operationErr) || !operationErr.Retryable || operationErr.Attempts != 1 {
		t.Fatalf("error = %#v, want the first retryable attempt", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, `"msg":"`+logGitOperationInterrupted+`"`) || !strings.Contains(logOutput, `"canceled":true`) {
		t.Fatalf("next-attempt cancellation terminal log incomplete: %s", logOutput)
	}
}

func TestRetryGitOperationLogsCancellationDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	store.gitRetryDelays = []time.Duration{time.Second}
	err := store.retryGitOperation(ctx, GitOperationFetch, "repo", func() error {
		time.AfterFunc(20*time.Millisecond, cancel)
		return errors.New("HTTP 503 transport failure")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, `"msg":"`+logGitOperationInterrupted+`"`) ||
		!strings.Contains(logOutput, `"canceled":true`) || !strings.Contains(logOutput, `"attempts":1`) {
		t.Fatalf("backoff cancellation terminal log incomplete: %s", logOutput)
	}
}

func TestTransientGitErrorIndependentSignatures(t *testing.T) {
	for _, message := range []string{
		"error: RPC failed; HTTP 408 curl 22",
		"error: RPC failed; HTTP 500 curl 22",
		"fatal: unable to access remote: The requested URL returned error: 522",
		"fatal: unable to access remote: The requested URL returned error: 524",
		"error: RPC failed; curl 18 transfer closed with 66 bytes remaining to read",
		"error: RPC failed; curl 55 Send failure: Broken pipe",
		"error: RPC failed; curl 56 The TLS connection was non-properly terminated",
		"error: RPC failed; curl 92 HTTP/2 stream 5 was not closed cleanly: CANCEL",
		"fatal: unable to access remote: Operation timed out after 60000 milliseconds",
		"fatal: unable to access remote: Empty reply from server",
		"fatal: unable to access remote: Could not resolve host: github.com",
		"fatal: unable to access remote: Failed to connect to github.com port 443: Couldn't connect to server",
		"fetch-pack: unexpected disconnect while reading sideband packet",
		"fetch-pack: 12 bytes of body are still expected",
		"fetch-pack: 3 bytes of length header were received",
		"fatal: early EOF",
	} {
		t.Run(message, func(t *testing.T) {
			if !isTransientGitError(errors.New(message)) {
				t.Fatalf("isTransientGitError(%q) = false, want true", message)
			}
		})
	}
}

func TestTransientGitErrorRejectsStandaloneLocalIndexPackFailure(t *testing.T) {
	message := "fatal: unable to write file: Input/output error\nfatal: index-pack failed"
	if isTransientGitError(errors.New(message)) {
		t.Fatalf("isTransientGitError(%q) = true, want false", message)
	}
}

func TestBoundedGitErrorPreservesClassificationAfterTruncation(t *testing.T) {
	var stderr boundedGitError
	stderr.limit = 16
	_, _ = stderr.Write([]byte(strings.Repeat("x", 32)))
	_, _ = stderr.Write([]byte(" HTTP 503 Service Unavailable"))
	if !stderr.Retryable() {
		t.Fatal("bounded stderr lost transient classification after truncation")
	}
	if got := stderr.String(); len(got) > stderr.limit+len(gitErrorTruncatedLabel)+1 || !strings.Contains(got, gitErrorTruncatedLabel) {
		t.Fatalf("bounded stderr = %q, want capped output with truncation label", got)
	}

	var permanent boundedGitError
	permanent.limit = 16
	_, _ = permanent.Write([]byte("HTTP 503"))
	_, _ = permanent.Write([]byte(" permission denied"))
	if permanent.Retryable() {
		t.Fatal("local permanent failure was classified as retryable")
	}
}

func TestRedactTruncatedCredentialPrefix(t *testing.T) {
	credential := "super-secret-token"
	message := "fatal: authentication failed: super-secret-tok\n" + gitErrorTruncatedLabel
	got := redactTruncatedCredentialPrefix(message, []string{credential})
	if strings.Contains(got, "super-secret") || !strings.Contains(got, "REDACTED") {
		t.Fatalf("truncated credential prefix leaked in %q", got)
	}
}

func TestRunGitClassifiesBeforeCredentialRedaction(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' 'HTTP 500 transport failure' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := runGitWithEnv(context.Background(), "", []string{"ARTIFACT_FS_GIT_PASSWORD=0"}, "version")
	_, _, retryable := gitOperationCauses(err)
	if !retryable {
		t.Fatalf("redacted command error was not retryable: %v", err)
	}
	if strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("command error leaked credential value: %v", err)
	}
}

func TestRunGitCancellationKillsDescendants(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(tmp, "started")
	survived := filepath.Join(tmp, "survived")
	script := "#!/bin/sh\n(sleep 0.2; : > \"$AFS_CHILD_SURVIVED\") &\n: > \"$AFS_GIT_STARTED\"\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_GIT_STARTED", started)
	t.Setenv("AFS_CHILD_SURVIVED", survived)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := runGit(ctx, "", "version")
		errCh <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("git command did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("runGit error = %v, want context canceled", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(survived); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("git descendant survived cancellation")
	}
}

func TestKillCommandProcessGroupReportsExitedProcess(t *testing.T) {
	cmd := exec.Command("git", "version")
	configureCommandProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := killCommandProcessGroup(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill exited process error = %v, want os.ErrProcessDone", err)
	}
}

func TestRetryGitOperationBoundedExhaustion(t *testing.T) {
	store := New(nil)
	store.gitRetryDelays = []time.Duration{0, 0}
	attempts := 0
	err := store.retryGitOperation(context.Background(), GitOperationClone, "repo", func() error {
		attempts++
		return errors.New("HTTP 500 transport failure")
	})
	var operationErr *GitOperationError
	if !errors.As(err, &operationErr) || operationErr.Attempts != 3 || !operationErr.Retryable {
		t.Fatalf("error = %#v, want retryable exhaustion after three attempts", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetryGitOperationRedactsExactLocalRemote(t *testing.T) {
	const remote = "/private/customer/repo.git"
	sentinel := errors.New("sentinel failure")
	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	store.gitRetryDelays = nil
	err := store.retryGitOperationForRemotes(context.Background(), GitOperationClone, "repo", []string{remote}, func() error {
		return fmt.Errorf("HTTP 503 cloning %s: %w", remote, sentinel)
	})
	if err == nil {
		t.Fatal("expected clone failure")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want preserved sentinel cause", err)
	}
	if strings.Contains(err.Error(), remote) || !strings.Contains(err.Error(), "REDACTED_REMOTE") {
		t.Fatalf("returned error did not redact remote: %v", err)
	}
	if logOutput := logs.String(); strings.Contains(logOutput, remote) || !strings.Contains(logOutput, "REDACTED_REMOTE") {
		t.Fatalf("local remote was not redacted from retry log: %s", logOutput)
	}
}

func TestFetchStartsBeforeRemoteLookupCompletes(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fetchStarted := filepath.Join(tmp, "fetch-started")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  'remote get-url origin') exec sleep 10;;\n" +
		"  'fetch --no-tags origin') : > \"$AFS_FETCH_STARTED\"; printf '%s\\n' 'HTTP 503 transport failure' >&2; exit 1;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_FETCH_STARTED", fetchStarted)

	var logs bytes.Buffer
	store := New(slog.New(slog.NewJSONHandler(&logs, nil)))
	store.gitRetryDelays = nil
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := store.Fetch(ctx, model.RepoConfig{Name: "repo", GitDir: filepath.Join(tmp, "repo.git"), RemoteURL: "/private/configured/repo.git"})
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Fetch error = %v, want immediate transport failure", err)
	}
	if _, statErr := os.Stat(fetchStarted); statErr != nil {
		t.Fatalf("fetch did not start while remote lookup was blocked: %v", statErr)
	}
	var operationErr *GitOperationError
	if !errors.As(err, &operationErr) || operationErr.Attempts != 1 {
		t.Fatalf("Fetch error = %#v, want one attempted fetch", err)
	}
	if logOutput := logs.String(); !strings.Contains(logOutput, `"msg":"`+logGitOperationAttemptFailed+`"`) {
		t.Fatalf("fetch failure terminal log missing: %s", logOutput)
	}
}

func TestRemoteLookupIncludesConfiguredAndActualOrigin(t *testing.T) {
	tmp := t.TempDir()
	gitDir := filepath.Join(tmp, "repo.git")
	run(t, "git", "init", "--bare", gitDir)
	const configured = "https://example.com/old/repo.git"
	actual := filepath.Join(tmp, "private", "repo.git")
	run(t, "git", "--git-dir", gitDir, "remote", "add", "origin", actual)
	remotesForLogging, cancel := New(nil).startRemotesForLogging(context.Background(), model.RepoConfig{GitDir: gitDir, RemoteURL: configured})
	defer cancel()
	deadline := time.Now().Add(time.Second)
	for {
		remotes, complete := remotesForLogging()
		if complete {
			if !slices.Contains(remotes, configured) || !slices.Contains(remotes, actual) {
				t.Fatalf("remotesForLogging = %q, want configured and actual remotes", remotes)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("remote lookup did not complete")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRetryGitOperationPermanentFailures(t *testing.T) {
	for _, message := range []string{
		"HTTP 429 Too Many Requests",
		"HTTP 501 Not Implemented",
		"HTTP 505 Version Not Supported",
		"fatal: Authentication failed",
		"fetch-pack failed",
		"packfile could not be read",
		"No space left on device; index-pack failed",
		"Disk quota exceeded; pack is corrupted",
		"Permission denied; early EOF",
		"Read-only file system; invalid index-pack output",
	} {
		t.Run(message, func(t *testing.T) {
			store := New(nil)
			store.gitRetryDelays = []time.Duration{0, 0}
			attempts := 0
			err := store.retryGitOperation(context.Background(), GitOperationClone, "repo", func() error {
				attempts++
				return errors.New(message)
			})
			var operationErr *GitOperationError
			if !errors.As(err, &operationErr) || operationErr.Retryable || operationErr.Attempts != 1 {
				t.Fatalf("error = %#v, want one permanent attempt", err)
			}
			if attempts != 1 {
				t.Fatalf("attempts = %d, want 1", attempts)
			}
		})
	}
}

func TestFetchRefNonInteractiveRetriesTransientFailures(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(tmp, "fetch-count")
	fakeGit := filepath.Join(bin, "git")
	script := "#!/bin/sh\n" +
		"if [ \"$*\" = 'remote get-url origin' ]; then exit 0; fi\n" +
		"count=0; [ -f \"$AFS_FETCH_COUNT\" ] && count=$(cat \"$AFS_FETCH_COUNT\")\n" +
		"count=$((count + 1)); printf '%s' \"$count\" > \"$AFS_FETCH_COUNT\"\n" +
		"if [ \"$count\" -lt 3 ]; then printf '%s\\n' 'HTTP 503 Service Unavailable' >&2; exit 1; fi\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_FETCH_COUNT", countPath)

	store := New(nil)
	store.gitRetryDelays = []time.Duration{0, 0}
	repo := model.RepoConfig{Name: "prepared", GitDir: filepath.Join(tmp, "prepared.git"), Branch: "main"}
	if err := store.FetchRefNonInteractive(context.Background(), repo, "main"); err != nil {
		t.Fatalf("FetchRefNonInteractive: %v", err)
	}
	count, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(count) != "3" {
		t.Fatalf("fetch attempts = %q, want 3", count)
	}
}

func TestPrepareExistingCloneRejectsCredentialedRemoteBeforeSetURL(t *testing.T) {
	tmp := t.TempDir()
	gitDir := filepath.Join(tmp, "repo.git")
	worktree := filepath.Join(tmp, "worktree")
	run(t, "git", "init", "--separate-git-dir", gitDir, "--initial-branch", "master", worktree)
	run(t, "git", "-C", worktree, "remote", "add", "origin", "https://github.com/org/repo.git")

	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "set-url-invoked")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\ncase \"$*\" in *\"remote set-url\"*) : > \"$GIT_SET_URL_MARKER\";; esac\nexec /usr/bin/git \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SET_URL_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	store := New(nil)
	for _, remote := range []string{
		"ssh:/git:secret@github.com/org/repo.git",
		"ssh//git:secret@github.com/org/repo.git",
		"ssh:/git:pa/ss@github.com/org/repo.git",
		"alice:ghp_secret@github.com:org/repo.git",
	} {
		t.Run(remote, func(t *testing.T) {
			cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: gitDir, RemoteURL: remote, Branch: "master", FetchRef: "master"}
			err := store.PrepareExistingCloneNonInteractive(context.Background(), cfg)
			if err == nil {
				t.Fatal("expected credentialed remote rejection")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked credential: %v", err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("git remote set-url was invoked before rejecting credentialed remote")
			}
		})
	}
}

func TestExistingCloneCredentialOperationsKeepCredentialsOutOfArguments(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	commands := filepath.Join(tmp, "commands")
	credentials := filepath.Join(tmp, "credentials")
	fakeGit := filepath.Join(bin, "git")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"$AFS_GIT_COMMANDS\"\n" +
		"if [ \"$*\" = 'remote get-url origin' ]; then printf '%s\\n' 'https://example.com/old/repo.git'; exit 0; fi\n" +
		"if [ \"$1\" = 'fetch' ]; then printf '%s:%s\\n' \"$ARTIFACT_FS_GIT_USERNAME\" \"$ARTIFACT_FS_GIT_PASSWORD\" > \"$AFS_GIT_CREDENTIALS\"; fi\n"
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AFS_GIT_COMMANDS", commands)
	t.Setenv("AFS_GIT_CREDENTIALS", credentials)

	store := New(nil)
	repo := model.RepoConfig{
		Name:      "repo",
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "https://alice:super-secret@example.com/org/repo.git",
		Branch:    "main",
	}
	if err := store.ConfigureRemoteWithCredentials(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if err := store.FetchRefWithCredentials(context.Background(), repo, "main"); err != nil {
		t.Fatal(err)
	}

	commandData, err := os.ReadFile(commands)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commandData), "alice") || strings.Contains(string(commandData), "super-secret") {
		t.Fatalf("credentials appeared in git arguments: %s", commandData)
	}
	if !strings.Contains(string(commandData), "remote set-url origin https://example.com/org/repo.git") {
		t.Fatalf("sanitized remote was not configured: %s", commandData)
	}
	credentialData, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if string(credentialData) != "alice:super-secret\n" {
		t.Fatalf("credential helper environment = %q", credentialData)
	}
}

func TestValidatePreparedGitDirRejectsCredentialedOrigin(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")
	run(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
	run(t, "git", "-C", preparedWorktree, "remote", "add", "origin", "https://ghp_secret@github.com/org/repo.git")

	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: preparedGitDir, Branch: "master"}
	store := New(nil)
	err := store.ValidatePreparedGitDir(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected credentialed origin rejection")
	}
	if strings.Contains(err.Error(), "ghp_secret") {
		t.Fatalf("error leaked origin credential: %v", err)
	}
}

func TestValidatePreparedGitDirRejectsMalformedCredentialedOrigin(t *testing.T) {
	for _, remote := range []string{
		"ssh:/git:secret@github.com/org/repo.git",
		"alice:ghp_secret@github.com:org/repo.git",
	} {
		t.Run(remote, func(t *testing.T) {
			tmp := t.TempDir()
			preparedGitDir := filepath.Join(tmp, "prepared.git")
			preparedWorktree := filepath.Join(tmp, "prepared")
			run(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
			run(t, "git", "-C", preparedWorktree, "remote", "add", "origin", remote)

			cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: preparedGitDir, Branch: "master"}
			store := New(nil)
			if err := store.ValidatePreparedGitDir(context.Background(), cfg); err == nil {
				t.Fatal("expected malformed credentialed origin rejection")
			}
		})
	}
}

func TestValidatePreparedGitDirAllowsAtInHTTPSOriginPath(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")
	run(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
	run(t, "git", "-C", preparedWorktree, "remote", "add", "origin", "https://git.example.com/team/repo@2026.git")

	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: preparedGitDir, Branch: "master"}
	store := New(nil)
	if err := store.ValidatePreparedGitDir(context.Background(), cfg); err != nil {
		t.Fatalf("ValidatePreparedGitDir: %v", err)
	}
}

func TestFetchRefNonInteractiveFullRefPreparesDetachedHEAD(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "master")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644)
	run(t, "git", "-C", work, "add", "README.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "init")
	run(t, "git", "-C", work, "push", "origin", "master")
	run(t, "git", "-C", work, "checkout", "-b", "pull-request")
	os.WriteFile(filepath.Join(work, "PR.md"), []byte("pull request\n"), 0o644)
	run(t, "git", "-C", work, "add", "PR.md")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "pr")
	run(t, "git", "-C", work, "push", "origin", "HEAD:refs/pull/10/head")

	run(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
	run(t, "git", "-C", preparedWorktree, "remote", "add", "origin", "file://"+bare)

	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: preparedGitDir, Branch: "master"}
	store := New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := store.FetchRefNonInteractive(ctx, cfg, "refs/pull/10/head"); err != nil {
		t.Fatalf("FetchRefNonInteractive: %v", err)
	}
	if _, err := runGit(ctx, preparedGitDir, "rev-parse", "--verify", fetchedFullRefRemoteTrackingRef+"^{commit}"); err != nil {
		t.Fatalf("expected fetched full ref at safe remote-tracking ref: %v", err)
	}
	if err := store.PrepareFetchedBranch(ctx, cfg, "refs/pull/10/head"); err != nil {
		t.Fatalf("PrepareFetchedBranch: %v", err)
	}
	oid, ref, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatalf("ResolveHEAD: %v", err)
	}
	if ref != "DETACHED" {
		t.Fatalf("ref = %q, want DETACHED", ref)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	found := false
	for _, n := range nodes {
		if n.Path == "PR.md" {
			found = true
		}
	}
	if !found {
		t.Fatal("PR.md not found in prepared tree")
	}
}

func TestCredentialEnvKeepsSecretsOutOfHelperCommand(t *testing.T) {
	t.Parallel()
	safeURL, env, err := credentialEnv("https://user:p@ss'word@github.com/org/repo.git")
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if safeURL == "" {
		t.Fatal("expected non-empty safe URL")
	}
	if strings.Contains(safeURL, "p@ss") {
		t.Fatalf("safe URL should not contain password: %s", safeURL)
	}
	foundHelper := false
	foundReset := false
	foundScope := false
	foundPassword := false
	for _, e := range env {
		if e == "GIT_CONFIG_VALUE_0=" {
			foundReset = true
		}
		if e == "GIT_CONFIG_KEY_1=credential.https://github.com.helper" {
			foundScope = true
		}
		if val, ok := strings.CutPrefix(e, "GIT_CONFIG_VALUE_1="); ok {
			foundHelper = true
			if strings.Contains(val, "p@ss'word") {
				t.Fatalf("password leaked in helper command: %s", val)
			}
		}
		if e == "ARTIFACT_FS_GIT_PASSWORD=p@ss'word" {
			foundPassword = true
		}
	}
	if !foundReset {
		t.Fatal("expected empty credential.helper reset")
	}
	if !foundHelper {
		t.Fatal("expected GIT_CONFIG_VALUE_1 in env")
	}
	if !foundScope {
		t.Fatalf("expected URL-scoped credential helper, got %v", env)
	}
	if !foundPassword {
		t.Fatalf("expected password env var, got %v", env)
	}
}

func TestCredentialEnvScopesCredentialsToOriginalAuthority(t *testing.T) {
	_, credentialEnvVars, err := credentialEnv("https://alice:secret@example.com:8443/org/repo.git")
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	fill := func(host string) (string, error) {
		cmd := exec.Command("git", "credential", "fill")
		cmd.Env = append(os.Environ(), credentialEnvVars...)
		cmd.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\n\n")
		out, err := cmd.Output()
		return string(out), err
	}

	original, err := fill("example.com:8443")
	if err != nil {
		t.Fatalf("credential fill for original authority: %v", err)
	}
	if !strings.Contains(original, "username=alice") || !strings.Contains(original, "password=secret") {
		t.Fatalf("original authority did not receive credentials: %q", original)
	}
	different, _ := fill("other.example.com:8443")
	if strings.Contains(different, "username=") || strings.Contains(different, "password=") {
		t.Fatalf("different authority received credentials: %q", different)
	}
}

func TestCredentialEnvRejectsCredentialedHTTPWithoutAuthority(t *testing.T) {
	t.Parallel()
	if _, _, err := credentialEnv("https://alice:secret@/attacker/repo.git"); err == nil {
		t.Fatal("expected missing HTTP authority rejection")
	}
}

func TestCredentialEnvNoCredentials(t *testing.T) {
	t.Parallel()
	safeURL, env, err := credentialEnv("https://github.com/org/repo.git")
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if safeURL != "https://github.com/org/repo.git" {
		t.Fatalf("expected unchanged URL, got %s", safeURL)
	}
	if len(env) != 0 {
		t.Fatalf("expected no env vars, got %v", env)
	}
}

func TestCredentialEnvAllowsFileURLPathWithAtSign(t *testing.T) {
	t.Parallel()
	const remote = "file:///tmp/repo@2026.git"
	safeURL, env, err := credentialEnv(remote)
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if safeURL != remote {
		t.Fatalf("safe URL = %q, want %q", safeURL, remote)
	}
	if len(env) != 0 {
		t.Fatalf("expected no env vars, got %v", env)
	}
}

func TestCredentialEnvAllowsSCPStyleRootPathWithAtSign(t *testing.T) {
	t.Parallel()
	const remote = "git@example.com:repo:v1@2026.git"
	safeURL, env, err := credentialEnv(remote)
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if safeURL != remote {
		t.Fatalf("safe URL = %q, want %q", safeURL, remote)
	}
	if len(env) != 0 {
		t.Fatalf("expected no env vars, got %v", env)
	}
}

func TestCredentialEnvRejectsQueryAndFragment(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://github.com/org/repo.git?access_token=secret",
		"https://github.com/org/repo.git#access_token=secret",
		"https://github.com/org/repo.git#",
		"git@github.com:org/repo.git?access_token=secret",
		"git@github.com:org/repo.git#access_token=secret",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := credentialEnv(raw); err == nil {
				t.Fatal("expected query or fragment rejection")
			}
		})
	}
}

func TestCredentialEnvTokenAsUsername(t *testing.T) {
	t.Parallel()
	safeURL, env, err := credentialEnv("https://ghp_abc123@github.com/org/repo.git")
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if strings.Contains(safeURL, "ghp_abc123") {
		t.Fatalf("token should be stripped from safe URL: %s", safeURL)
	}
	if len(env) == 0 {
		t.Fatal("expected credential helper env vars")
	}
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_CONFIG_VALUE_1=") && strings.Contains(e, "ghp_abc123") {
			t.Fatalf("credential helper command leaked token: %s", e)
		}
	}
}

func TestCredentialEnvPreservesSSHUsername(t *testing.T) {
	t.Parallel()
	safeURL, env, err := credentialEnv("ssh://git@github.com/org/repo.git")
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if safeURL != "ssh://git@github.com/org/repo.git" {
		t.Fatalf("safe URL = %q, want SSH username preserved", safeURL)
	}
	if len(env) != 0 {
		t.Fatalf("expected no credential helper env for SSH username, got %v", env)
	}
}

func TestCredentialEnvRejectsGitProtocolUsername(t *testing.T) {
	t.Parallel()
	if _, _, err := credentialEnv("git://ghp_secret@github.com/org/repo.git"); err == nil {
		t.Fatal("expected git protocol username rejection")
	}
}

func TestCredentialEnvRejectsSSHTokenUsername(t *testing.T) {
	t.Parallel()
	if _, _, err := credentialEnv("ssh://ghp_abcdefghijklmnopqrstuvwxyz@github.com/org/repo.git"); err == nil {
		t.Fatal("expected SSH token username rejection")
	}
}

func TestCredentialEnvRejectsSSHPassword(t *testing.T) {
	t.Parallel()
	if _, _, err := credentialEnv("ssh://git:secret@github.com/org/repo.git"); err == nil {
		t.Fatal("expected SSH password rejection")
	}
}

func TestCredentialEnvRejectsMalformedSSHPassword(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"ssh:/git:secret@github.com/org/repo.git",
		"ssh:/git:bad%zz@github.com/org/repo.git",
		"alice:ghp_secret@github.com:org/repo.git",
		"x-token-auth:secret@bitbucket.org/org/repo.git",
		"https://ghp_secret/part@example.com/org/repo.git",
		"https://ghp_secret/part@example.com",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := credentialEnv(raw); err == nil {
				t.Fatal("expected malformed SSH password rejection")
			}
		})
	}
}

func TestCloneBloblessRejectsMalformedCredentialURLBeforeGit(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "git-invoked")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > \"$GIT_INVOKED_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INVOKED_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := model.RepoConfig{
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "https://user:bad%zz@example.com/org/repo.git",
		Branch:    "main",
	}
	store := New(nil)
	err := store.CloneBlobless(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected malformed remote URL error")
	}
	if strings.Contains(err.Error(), "bad%zz") || strings.Contains(err.Error(), "user") {
		t.Fatalf("error leaked credential URL: %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("git was invoked before rejecting malformed URL")
	}
}

func TestCloneBloblessRejectsMalformedHTTPSUserinfoBeforeGit(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "git-invoked")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > \"$GIT_INVOKED_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INVOKED_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := model.RepoConfig{
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "https:/user:ghp_secret@github.com/org/repo.git",
		Branch:    "main",
	}
	store := New(nil)
	err := store.CloneBlobless(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected malformed remote URL error")
	}
	if strings.Contains(err.Error(), "ghp_secret") || strings.Contains(err.Error(), "user") {
		t.Fatalf("error leaked credential URL: %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("git was invoked before rejecting malformed URL")
	}
}

func TestCloneBloblessRejectsMalformedHTTPParseErrorBeforeGit(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "git-invoked")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > \"$GIT_INVOKED_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INVOKED_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := model.RepoConfig{
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "https//ghp_secret%zz@github.com/org/repo.git",
		Branch:    "main",
	}
	store := New(nil)
	err := store.CloneBlobless(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected malformed remote URL error")
	}
	if strings.Contains(err.Error(), "ghp_secret") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("error leaked credential URL: %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("git was invoked before rejecting malformed URL")
	}
}

func TestCloneBloblessRejectsMalformedGitStyleCredentialBeforeGit(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "git-invoked")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > \"$GIT_INVOKED_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INVOKED_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := model.RepoConfig{
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "git:secret@github.com:org/repo.git",
		Branch:    "main",
	}
	store := New(nil)
	err := store.CloneBlobless(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected malformed remote URL error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaked credential URL: %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("git was invoked before rejecting malformed URL")
	}
}

func TestCloneBloblessRejectsPathSplitHTTPCredentialsBeforeGit(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "git-invoked")
	fakeGit := filepath.Join(bin, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\n: > \"$GIT_INVOKED_MARKER\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INVOKED_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := model.RepoConfig{
		GitDir:    filepath.Join(tmp, "repo.git"),
		RemoteURL: "https://user:123/ss@example.com/org/repo.git",
		Branch:    "main",
	}
	store := New(nil)
	err := store.CloneBlobless(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected malformed remote URL error")
	}
	if strings.Contains(err.Error(), "123") || strings.Contains(err.Error(), "ss") {
		t.Fatalf("error leaked credential URL: %v", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("git was invoked before rejecting malformed URL")
	}
}

func TestCredentialEnvRejectsHTTPSLikeUserinfoTypos(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https:/user:ghp_secret@github.com/org/repo.git",
		"https//ghp_secret@github.com/org/repo.git",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := credentialEnv(raw); err == nil {
				t.Fatal("expected malformed HTTP-like remote rejection")
			}
		})
	}
}

func TestCredentialEnvAllowsAtInHTTPSPath(t *testing.T) {
	t.Parallel()
	safeURL, env, err := credentialEnv("https://git.example.com/team/repo:v1@2026.git")
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}
	if safeURL != "https://git.example.com/team/repo:v1@2026.git" {
		t.Fatalf("safe URL = %q", safeURL)
	}
	if len(env) != 0 {
		t.Fatalf("expected no credential helper env, got %v", env)
	}
}

func TestNonInteractiveGitEnvForcesSSHBatchMode(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "ssh -o BatchMode=no -i /secrets/deploy_key -o IdentitiesOnly=yes")
	env := nonInteractiveGitEnv()
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			if !strings.Contains(e, "-i /secrets/deploy_key") {
				t.Fatalf("expected existing identity option to be preserved, got %q", e)
			}
			if strings.Contains(e, "BatchMode=no") {
				t.Fatalf("expected existing BatchMode option to be replaced, got %q", e)
			}
			if strings.Contains(e, "BatchMode=yes") {
				return
			}
			break
		}
	}
	t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
}

func TestNonInteractiveGitEnvDefaultSSHBatchMode(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "")
	env := nonInteractiveGitEnv()
	if slices.Contains(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes") {
		return
	}
	t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
}

func TestNonInteractiveGitEnvStripsQuotedBatchMode(t *testing.T) {
	for _, command := range []string{
		`ssh -o "BatchMode=no" -i /secrets/deploy_key`,
		`ssh -o BatchMode="no" -i /secrets/deploy_key`,
		`ssh -o "BatchMode"=no -i /secrets/deploy_key`,
		`ssh -o 'BatchMode no' -i /secrets/deploy_key`,
		`ssh '-o' 'BatchMode=no' -i /secrets/deploy_key`,
		`ssh -oBatchMode="no" -i /secrets/deploy_key`,
	} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("GIT_SSH_COMMAND", command)
			env := nonInteractiveGitEnv()
			for _, e := range env {
				if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
					if strings.Contains(e, "BatchMode=no") || strings.Contains(e, `BatchMode="no"`) {
						t.Fatalf("expected quoted BatchMode option to be replaced, got %q", e)
					}
					if !strings.Contains(e, "-i /secrets/deploy_key") || !strings.Contains(e, "BatchMode=yes") {
						t.Fatalf("expected identity and BatchMode=yes, got %q", e)
					}
					return
				}
			}
			t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
		})
	}
}

func TestNonInteractiveGitEnvPreservesProxyCommand(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "ssh -o ProxyCommand='ssh -o BatchMode=no bastion' -i /secrets/deploy_key")
	env := nonInteractiveGitEnv()
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			if !strings.Contains(e, "ProxyCommand=ssh -o BatchMode=no bastion") {
				t.Fatalf("expected ProxyCommand to be preserved, got %q", e)
			}
			if !strings.Contains(e, "BatchMode=yes") {
				t.Fatalf("expected top-level BatchMode=yes, got %q", e)
			}
			return
		}
	}
	t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
}

func TestNonInteractiveGitEnvQuotesShellMetacharacters(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", `ssh -i /tmp/key\ prod -o UserKnownHostsFile=/tmp/known\ hosts`)
	env := nonInteractiveGitEnv()
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			if !strings.Contains(e, "'/tmp/key prod'") || !strings.Contains(e, "'UserKnownHostsFile=/tmp/known hosts'") {
				t.Fatalf("expected escaped shell paths to be quoted, got %q", e)
			}
			if !strings.Contains(e, "BatchMode=yes") {
				t.Fatalf("expected BatchMode=yes, got %q", e)
			}
			return
		}
	}
	t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
}

func TestNonInteractiveGitEnvPreservesShellExpansion(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", `ssh -i "$HOME/.ssh/deploy key"`)
	env := nonInteractiveGitEnv()
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			if !strings.Contains(e, "$HOME/.ssh/deploy key") {
				t.Fatalf("expected HOME expansion to be preserved, got %q", e)
			}
			if strings.Contains(e, "'$HOME/.ssh/deploy key'") {
				t.Fatalf("expected HOME expansion not to be single-quoted, got %q", e)
			}
			return
		}
	}
	t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
}

func TestNonInteractiveGitEnvPreservesEscapedDollar(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", `ssh -i '/tmp/key$prod dir'`)
	env := nonInteractiveGitEnv()
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			if !strings.Contains(e, `/tmp/key\$prod dir`) {
				t.Fatalf("expected escaped dollar to be preserved, got %q", e)
			}
			if strings.Contains(e, `/tmp/key$prod dir`) {
				t.Fatalf("expected literal dollar not to become expandable, got %q", e)
			}
			return
		}
	}
	t.Fatalf("expected forced GIT_SSH_COMMAND, got %v", env)
}

func TestSetBatchPoolSizeUpdatesExistingAndNewPools(t *testing.T) {
	t.Parallel()
	store := New(nil)
	first, err := store.getPool("/tmp/repo-a.git")
	if err != nil {
		t.Fatal(err)
	}
	if first.maxSize != 4 {
		t.Fatalf("initial pool maxSize = %d, want 4", first.maxSize)
	}

	store.SetBatchPoolSize(12)
	if first.maxSize != 12 {
		t.Fatalf("updated existing pool maxSize = %d, want 12", first.maxSize)
	}
	second, err := store.getPool("/tmp/repo-b.git")
	if err != nil {
		t.Fatal(err)
	}
	if second.maxSize != 12 {
		t.Fatalf("new pool maxSize = %d, want 12", second.maxSize)
	}
}

func TestReadNullDelimitedRejectsPartialFinalRecord(t *testing.T) {
	var records []string
	err := readNullDelimited(strings.NewReader("first\x00partial"), func(record string) {
		records = append(records, record)
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
	if !slices.Equal(records, []string{"first"}) {
		t.Fatalf("records = %v, want [first]", records)
	}
}

func TestStreamTreeRecordsPreservesDeadline(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := streamTreeRecords(ctx, filepath.Join(tmp, "repo.git"), "HEAD", func(string) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("streamTreeRecords error = %v, want deadline exceeded", err)
	}
}

func TestBatchPoolBoundsConcurrentProcesses(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	run(t, "git", "init", repo)
	p := &batchPool{
		gitDir:  filepath.Join(repo, ".git"),
		logger:  slog.Default(),
		maxSize: 1,
		all:     map[*batchCatFile]struct{}{},
		changed: make(chan struct{}),
	}
	first, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire err = %v, want deadline exceeded", err)
	}
	p.release(first)
	second, err := p.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.release(second)
	p.closeAll()
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	_ = runOutput(t, name, args...)
}

func assertArtifactWorktreeConfig(t *testing.T, ctx context.Context, gitDir string) {
	t.Helper()
	for key, want := range map[string]string{
		"core.filemode":   "true",
		"core.ignorecase": "false",
		"core.symlinks":   "true",
	} {
		got, err := runGit(ctx, gitDir, "config", "--local", "--get", key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func runOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
	return string(out)
}
