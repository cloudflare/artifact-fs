package overlay

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func testStore(t *testing.T) (*Store, model.RepoConfig) {
	t.Helper()
	dir := t.TempDir()
	cfg := model.RepoConfig{
		ID:            "test",
		Name:          "test",
		OverlayDir:    filepath.Join(dir, "overlay"),
		OverlayDBPath: filepath.Join(dir, "overlay", "meta.sqlite"),
		BlobCacheDir:  filepath.Join(dir, "cache"),
	}
	os.MkdirAll(cfg.BlobCacheDir, 0o755)
	s, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, cfg
}

func TestCreateAndGet(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	e, err := s.CreateFile(ctx, "hello.txt", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != "create" || e.Path != "hello.txt" {
		t.Fatalf("unexpected entry: %+v", e)
	}

	got, ok := s.Get("hello.txt")
	if !ok {
		t.Fatal("expected to find hello.txt")
	}
	if got.Kind != "create" || got.BackingPath == "" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestWriteAndRead(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	s.CreateFile(ctx, "f.txt", 0o644)
	n, err := s.WriteFile(ctx, "f.txt", 0, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("wrote %d, want 5", n)
	}

	e, _ := s.Get("f.txt")
	data, err := os.ReadFile(e.BackingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("got %q, want %q", data, "hello")
	}
}

func TestRemoveCreatesWhiteout(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	s.CreateFile(ctx, "del.txt", 0o644)
	if err := s.Remove(ctx, "del.txt"); err != nil {
		t.Fatal(err)
	}
	if !s.HasWhiteout("del.txt") {
		t.Fatal("expected whiteout")
	}
	if _, ok := s.Get("del.txt"); !ok {
		t.Fatal("expected entry (delete kind)")
	}
}

func TestRenameDBFirst(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	s.CreateFile(ctx, "old.txt", 0o644)
	s.WriteFile(ctx, "old.txt", 0, []byte("content"))

	if err := s.Rename(ctx, "old.txt", "new.txt"); err != nil {
		t.Fatal(err)
	}

	// Old path should have a whiteout
	if !s.HasWhiteout("old.txt") {
		t.Fatal("expected whiteout at old path")
	}
	// New path should exist
	got, ok := s.Get("new.txt")
	if !ok || got.Kind != "rename" {
		t.Fatalf("expected rename entry, got %+v ok=%v", got, ok)
	}
	// File content should be readable at new backing path
	data, err := os.ReadFile(got.BackingPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" {
		t.Fatalf("got %q, want %q", data, "content")
	}
}

func TestMkdir(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "subdir", 0o755); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get("subdir")
	if !ok || e.Kind != "mkdir" {
		t.Fatalf("expected mkdir entry, got %+v", e)
	}
}

func TestDirtyCount(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	c, _ := s.DirtyCount(ctx)
	if c != 0 {
		t.Fatalf("expected 0, got %d", c)
	}
	s.CreateFile(ctx, "a.txt", 0o644)
	s.CreateFile(ctx, "b.txt", 0o644)
	c, _ = s.DirtyCount(ctx)
	if c != 2 {
		t.Fatalf("expected 2, got %d", c)
	}
	// Whiteouts don't count as dirty
	s.Remove(ctx, "a.txt")
	c, _ = s.DirtyCount(ctx)
	if c != 1 {
		t.Fatalf("expected 1, got %d", c)
	}
}

func TestListByPrefix(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	s.CreateFile(ctx, "src/a.go", 0o644)
	s.CreateFile(ctx, "src/b.go", 0o644)
	s.CreateFile(ctx, "srclib/c.go", 0o644) // should NOT match src/ prefix
	s.CreateFile(ctx, "readme.md", 0o644)

	entries, err := s.ListByPrefix(ctx, "src")
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, e := range entries {
		paths[e.Path] = true
	}
	if !paths["src/a.go"] || !paths["src/b.go"] {
		t.Fatalf("expected src/a.go and src/b.go, got %v", paths)
	}
	if paths["srclib/c.go"] {
		t.Fatal("srclib/c.go should not match src/ prefix")
	}
	if paths["readme.md"] {
		t.Fatal("readme.md should not match src/ prefix")
	}
}

func TestSetMtime(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	s.CreateFile(ctx, "m.txt", 0o644)
	target := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := s.SetMtime(ctx, "m.txt", target); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get("m.txt")
	if !ok {
		t.Fatal("expected entry")
	}
	got := time.Unix(0, e.MtimeUnixNs)
	if !got.Equal(target) {
		t.Fatalf("mtime = %v, want %v", got, target)
	}
}
