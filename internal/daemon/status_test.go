package daemon

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
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

func TestFSMonitorDirtyPathsIncludesChangedPathsAndRenameEndpoints(t *testing.T) {
	paths := fsMonitorDirtyPaths([]model.OverlayEntry{
		{Path: "src/pkg/file.go", Kind: model.OverlayKindModify},
		{Path: "new/name.go", Kind: model.OverlayKindRename, TargetPath: "old/name.go"},
		{Path: ".", Kind: model.OverlayKindMkdir},
	})
	want := []string{
		"new/name.go",
		"old/name.go",
		"src/pkg/file.go",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("dirty paths = %#v, want %#v", paths, want)
	}
}

func TestFSMonitorHookOutputsDirtyOverlayPaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	svc, err := New(ctx, root, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	cfg := model.RepoConfig{
		Name:            "repo",
		ID:              "repo",
		Branch:          "main",
		RefreshInterval: time.Minute,
		Enabled:         true,
	}
	svc.fillPaths(&cfg)
	if err := svc.registry.AddRepo(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ov.CreateFile(ctx, "new/file.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ov.Remove(ctx, "deleted.txt"); err != nil {
		t.Fatal(err)
	}
	if err := ov.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := svc.FSMonitorHook(ctx, "repo", &out); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(out.String(), "\x00")
	if !strings.HasPrefix(parts[0], "artifact-fs:repo:") {
		t.Fatalf("token = %q", parts[0])
	}
	got := map[string]bool{}
	for _, p := range parts[1:] {
		if p != "" {
			got[p] = true
		}
	}
	for _, want := range []string{"deleted.txt", "new/file.txt"} {
		if !got[want] {
			t.Fatalf("fsmonitor paths missing %q: %v", want, got)
		}
	}
}
