package gitstore

import (
	"context"
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

func TestCredentialEnvEscapesSingleQuotes(t *testing.T) {
	t.Parallel()
	// Password with a single quote should be escaped
	safeURL, env := credentialEnv("https://user:p@ss'word@github.com/org/repo.git")
	if safeURL == "" {
		t.Fatal("expected non-empty safe URL")
	}
	if strings.Contains(safeURL, "p@ss") {
		t.Fatalf("safe URL should not contain password: %s", safeURL)
	}
	// The credential helper env var should contain escaped quote
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_CONFIG_VALUE_0=") {
			found = true
			val := strings.TrimPrefix(e, "GIT_CONFIG_VALUE_0=")
			if strings.Contains(val, "p@ss'word") {
				t.Fatalf("unescaped password in helper: %s", val)
			}
			// Should contain the escaped form
			if !strings.Contains(val, `'\''`) {
				t.Fatalf("expected escaped single quote in helper, got: %s", val)
			}
		}
	}
	if !found {
		t.Fatal("expected GIT_CONFIG_VALUE_0 in env")
	}
}

func TestCredentialEnvNoCredentials(t *testing.T) {
	t.Parallel()
	safeURL, env := credentialEnv("https://github.com/org/repo.git")
	if safeURL != "https://github.com/org/repo.git" {
		t.Fatalf("expected unchanged URL, got %s", safeURL)
	}
	if len(env) != 0 {
		t.Fatalf("expected no env vars, got %v", env)
	}
}

func TestCredentialEnvTokenAsUsername(t *testing.T) {
	t.Parallel()
	safeURL, env := credentialEnv("https://ghp_abc123@github.com/org/repo.git")
	if strings.Contains(safeURL, "ghp_abc123") {
		t.Fatalf("token should be stripped from safe URL: %s", safeURL)
	}
	if len(env) == 0 {
		t.Fatal("expected credential helper env vars")
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
