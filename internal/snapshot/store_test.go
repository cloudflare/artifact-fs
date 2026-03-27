package snapshot

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/model"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(context.Background(), filepath.Join(dir, "snap.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPublishAndGetNode(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	nodes := []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "README.md", Type: "file", Mode: 0o644, ObjectOID: "abc123", SizeState: "known", SizeBytes: 42},
		{RepoID: "r", Path: "src", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "src/main.go", Type: "file", Mode: 0o644, ObjectOID: "def456", SizeState: "known", SizeBytes: 100},
	}
	gen, err := s.PublishGeneration(ctx, "r", "head1", "main", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 1 {
		t.Fatalf("expected gen 1, got %d", gen)
	}

	n, ok := s.GetNode("r", gen, "README.md")
	if !ok {
		t.Fatal("expected README.md")
	}
	if n.SizeBytes != 42 || n.ObjectOID != "abc123" {
		t.Fatalf("wrong node: %+v", n)
	}

	_, ok = s.GetNode("r", gen, "nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestListChildren(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	nodes := []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "a1", SizeState: "known", SizeBytes: 1},
		{RepoID: "r", Path: "b.txt", Type: "file", Mode: 0o644, ObjectOID: "b1", SizeState: "known", SizeBytes: 2},
		{RepoID: "r", Path: "sub", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "sub/c.txt", Type: "file", Mode: 0o644, ObjectOID: "c1", SizeState: "known", SizeBytes: 3},
	}
	gen, err := s.PublishGeneration(ctx, "r", "h1", "main", nodes)
	if err != nil {
		t.Fatal(err)
	}

	// Root children
	children, err := s.ListChildren("r", gen, ".")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range children {
		names[c.Path] = true
	}
	if !names["a.txt"] || !names["b.txt"] || !names["sub"] {
		t.Fatalf("expected a.txt, b.txt, sub in root children, got %v", names)
	}
	if names["sub/c.txt"] {
		t.Fatal("sub/c.txt should not be a root child")
	}

	// Sub children
	children, err = s.ListChildren("r", gen, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Path != "sub/c.txt" {
		t.Fatalf("expected [sub/c.txt], got %v", children)
	}
}

func TestGenerationCleanup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	nodes := []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "f.txt", Type: "file", Mode: 0o644, ObjectOID: "x", SizeState: "known"},
	}
	g1, _ := s.PublishGeneration(ctx, "r", "h1", "main", nodes)
	g2, _ := s.PublishGeneration(ctx, "r", "h2", "main", nodes)
	g3, _ := s.PublishGeneration(ctx, "r", "h3", "main", nodes)

	if g1 != 1 || g2 != 2 || g3 != 3 {
		t.Fatalf("unexpected generations: %d %d %d", g1, g2, g3)
	}

	// Generation 1 should be cleaned up after gen 3 publish
	_, ok := s.GetNode("r", 1, "f.txt")
	if ok {
		t.Fatal("gen 1 should be cleaned up")
	}
	// Generation 2 should still exist (gen-1)
	_, ok = s.GetNode("r", 2, "f.txt")
	if !ok {
		t.Fatal("gen 2 should still exist")
	}
}

func TestCurrentGeneration(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	gen, err := s.CurrentGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 0 {
		t.Fatalf("expected 0 for empty store, got %d", gen)
	}

	nodes := []model.BaseNode{{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"}}
	s.PublishGeneration(ctx, "r", "h1", "main", nodes)

	gen, err = s.CurrentGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 1 {
		t.Fatalf("expected 1, got %d", gen)
	}
}
