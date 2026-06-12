package gitstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

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

func TestPrepareExistingCloneNonInteractiveUpdatesBranch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	gitDir := filepath.Join(tmp, "repo.git")

	run(t, "git", "init", "--bare", bare)
	run(t, "git", "clone", bare, work)
	run(t, "git", "-C", work, "checkout", "-b", "master")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("master\n"), 0o644)
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
	t.Parallel()
	tmp := t.TempDir()
	preparedGitDir := filepath.Join(tmp, "prepared.git")
	preparedWorktree := filepath.Join(tmp, "prepared")
	run(t, "git", "init", "--separate-git-dir", preparedGitDir, "--initial-branch", "master", preparedWorktree)
	run(t, "git", "-C", preparedWorktree, "remote", "add", "origin", "ssh:/git:secret@github.com/org/repo.git")

	cfg := model.RepoConfig{ID: "x", Name: "x", GitDir: preparedGitDir, Branch: "master"}
	store := New(nil)
	if err := store.ValidatePreparedGitDir(context.Background(), cfg); err == nil {
		t.Fatal("expected malformed credentialed origin rejection")
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
	foundPassword := false
	for _, e := range env {
		if e == "GIT_CONFIG_VALUE_0=" {
			foundReset = true
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
	if !foundPassword {
		t.Fatalf("expected password env var, got %v", env)
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
	for _, e := range env {
		if e == "GIT_SSH_COMMAND=ssh -o BatchMode=yes" {
			return
		}
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
	first := store.getPool("/tmp/repo-a.git")
	if first.maxSize != 4 {
		t.Fatalf("initial pool maxSize = %d, want 4", first.maxSize)
	}

	store.SetBatchPoolSize(12)
	if first.maxSize != 12 {
		t.Fatalf("updated existing pool maxSize = %d, want 12", first.maxSize)
	}
	second := store.getPool("/tmp/repo-b.git")
	if second.maxSize != 12 {
		t.Fatalf("new pool maxSize = %d, want 12", second.maxSize)
	}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, string(out))
	}
}
