package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func TestReadPersistedStatusIncludesHydrationStats(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	gitDir := filepath.Join(root, "git")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir cache: %v", err)
	}
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "blob-a"), []byte("abc"), 0o644); err != nil {
		t.Fatalf("write blob-a: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "nested", "blob-b"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write blob-b: %v", err)
	}

	svc := &Service{}
	st := svc.readPersistedStatus(context.Background(), model.RepoConfig{ID: "repo", BlobCacheDir: cacheDir, GitDir: gitDir})

	if st.LastFetchResult != "never" {
		t.Fatalf("LastFetchResult = %q, want never", st.LastFetchResult)
	}
	if !st.LastFetchAt.IsZero() {
		t.Fatalf("LastFetchAt = %v, want zero", st.LastFetchAt)
	}
	if st.HydratedBlobCount != 2 {
		t.Fatalf("HydratedBlobCount = %d, want 2", st.HydratedBlobCount)
	}
	if st.HydratedBlobBytes != 8 {
		t.Fatalf("HydratedBlobBytes = %d, want 8", st.HydratedBlobBytes)
	}
}
