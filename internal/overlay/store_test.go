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
	if e.Kind != model.OverlayKindCreate || e.Path != "hello.txt" {
		t.Fatalf("unexpected entry: %+v", e)
	}

	got, ok := s.Get("hello.txt")
	if !ok {
		t.Fatal("expected to find hello.txt")
	}
	if got.Kind != model.OverlayKindCreate || got.BackingPath == "" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestNewRepairsZeroCtimeBackfill(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateFile(ctx, "old.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE overlay_entries SET ctime_unix_ns=0 WHERE path=?`, "old.txt"); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	got, ok := reopened.Get("old.txt")
	if !ok {
		t.Fatal("expected entry")
	}
	if got.CtimeUnixNs == 0 {
		t.Fatal("expected zero ctime to be repaired")
	}
}

func TestWriteAndRead(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	s.CreateFile(ctx, "f.txt", 0o644)
	created, _ := s.Get("f.txt")
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
	if e.CtimeUnixNs < created.CtimeUnixNs {
		t.Fatalf("ctime moved backward: got %d before %d", e.CtimeUnixNs, created.CtimeUnixNs)
	}
	if e.MtimeUnixNs != e.CtimeUnixNs {
		t.Fatalf("write should set mtime and ctime together: mtime=%d ctime=%d", e.MtimeUnixNs, e.CtimeUnixNs)
	}
}

func TestRemoveCreatesWhiteout(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	s.CreateFile(ctx, "del.txt", 0o644)
	if err := s.Remove(ctx, "del.txt"); err != nil {
		t.Fatal(err)
	}
	if e, ok := s.Get("del.txt"); !ok || !e.IsDeleted() {
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
	if e, ok := s.Get("old.txt"); !ok || !e.IsDeleted() {
		t.Fatal("expected whiteout at old path")
	}
	// New path should exist
	got, ok := s.Get("new.txt")
	if !ok || got.Kind != model.OverlayKindRename {
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
	if got.SizeBytes != int64(len("content")) {
		t.Fatalf("size = %d, want %d", got.SizeBytes, len("content"))
	}
}

func TestEnsureCopyOnWritePreservesSize(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()

	const oid = "abc123"
	if err := os.WriteFile(filepath.Join(cfg.BlobCacheDir, oid), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	e, err := s.EnsureCopyOnWrite(ctx, cfg, "tracked.txt", model.BaseNode{
		RepoID:    cfg.ID,
		Path:      "tracked.txt",
		Type:      "file",
		Mode:      0o644,
		ObjectOID: oid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.SizeBytes != int64(len("payload")) {
		t.Fatalf("size = %d, want %d", e.SizeBytes, len("payload"))
	}

	got, ok := s.Get("tracked.txt")
	if !ok {
		t.Fatal("expected tracked.txt entry")
	}
	if got.SizeBytes != int64(len("payload")) {
		t.Fatalf("stored size = %d, want %d", got.SizeBytes, len("payload"))
	}
}

func TestMkdir(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "subdir", 0o755); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get("subdir")
	if !ok || e.Kind != model.OverlayKindMkdir {
		t.Fatalf("expected mkdir entry, got %+v", e)
	}
}

func TestTruncateUpdatesSizeAndTimes(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateFile(ctx, "trunc.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, "trunc.txt", 0, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	target := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := s.SetMtime(ctx, "trunc.txt", target); err != nil {
		t.Fatal(err)
	}
	if err := s.Truncate(ctx, "trunc.txt", 2); err != nil {
		t.Fatal(err)
	}

	e, ok := s.Get("trunc.txt")
	if !ok {
		t.Fatal("expected entry")
	}
	if e.SizeBytes != 2 {
		t.Fatalf("size = %d, want 2", e.SizeBytes)
	}
	if e.MtimeUnixNs == target.UnixNano() || e.CtimeUnixNs == target.UnixNano() {
		t.Fatalf("truncate should replace user mtime with mutation time: mtime=%d ctime=%d", e.MtimeUnixNs, e.CtimeUnixNs)
	}
	if e.MtimeUnixNs != e.CtimeUnixNs {
		t.Fatalf("truncate should set mtime and ctime together: mtime=%d ctime=%d", e.MtimeUnixNs, e.CtimeUnixNs)
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

func TestReconcileAfterCommit(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()

	// Simulate: user modified foo.txt (source_oid="aaa") then committed.
	// After commit, the base has foo.txt with a new OID ("bbb").
	base := model.BaseNode{RepoID: cfg.ID, Path: "foo.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}
	s.EnsureCopyOnWrite(ctx, cfg, "foo.txt", base)
	s.WriteFile(ctx, "foo.txt", 0, []byte("modified"))

	// Also create a new file that was committed.
	s.CreateFile(ctx, "new.txt", 0o644)

	// And a whiteout for a file that was deleted in the commit.
	s.Remove(ctx, "gone.txt")

	// Reconcile against a base where:
	// - foo.txt exists with different OID (was committed)
	// - new.txt exists (was committed)
	// - gone.txt doesn't exist (was removed)
	baseLookup := func(path string) (model.BaseNode, bool) {
		switch path {
		case "foo.txt":
			return model.BaseNode{Path: "foo.txt", ObjectOID: "bbb"}, true
		case "new.txt":
			return model.BaseNode{Path: "new.txt", ObjectOID: "ccc"}, true
		default:
			return model.BaseNode{}, false
		}
	}
	if err := s.Reconcile(ctx, baseLookup); err != nil {
		t.Fatal(err)
	}

	// All three entries should be removed.
	if _, ok := s.Get("foo.txt"); ok {
		t.Fatal("foo.txt should be removed (base OID changed)")
	}
	if _, ok := s.Get("new.txt"); ok {
		t.Fatal("new.txt should be removed (now in base)")
	}
	if _, ok := s.Get("gone.txt"); ok {
		t.Fatal("gone.txt whiteout should be removed (not in base)")
	}
}

func TestReconcileKeepsValidEntries(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()

	// Modify foo.txt from base OID "aaa" -- but base hasn't changed.
	base := model.BaseNode{RepoID: cfg.ID, Path: "foo.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}
	s.EnsureCopyOnWrite(ctx, cfg, "foo.txt", base)
	s.WriteFile(ctx, "foo.txt", 0, []byte("local change"))

	// Create a file that doesn't exist in base.
	s.CreateFile(ctx, "local-only.txt", 0o644)

	// Whiteout a file that still exists in base.
	s.Remove(ctx, "hidden.txt")

	baseLookup := func(path string) (model.BaseNode, bool) {
		switch path {
		case "foo.txt":
			// Same OID as source_oid -- base unchanged, keep overlay.
			return model.BaseNode{Path: "foo.txt", ObjectOID: "aaa"}, true
		case "hidden.txt":
			return model.BaseNode{Path: "hidden.txt", ObjectOID: "ddd"}, true
		default:
			return model.BaseNode{}, false
		}
	}
	if err := s.Reconcile(ctx, baseLookup); err != nil {
		t.Fatal(err)
	}

	// All three entries should be kept.
	if _, ok := s.Get("foo.txt"); !ok {
		t.Fatal("foo.txt should be kept (base OID unchanged)")
	}
	if _, ok := s.Get("local-only.txt"); !ok {
		t.Fatal("local-only.txt should be kept (not in base)")
	}
	if e, ok := s.Get("hidden.txt"); !ok || !e.IsDeleted() {
		t.Fatal("hidden.txt whiteout should be kept (still in base)")
	}
}

func TestReconcileNilLookup(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	s.CreateFile(ctx, "a.txt", 0o644)

	// nil baseLookup should be a no-op.
	if err := s.Reconcile(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("a.txt"); !ok {
		t.Fatal("entry should survive nil reconcile")
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
	if e.CtimeUnixNs == target.UnixNano() {
		t.Fatalf("ctime should not be caller-controlled: ctime=%v target=%v", time.Unix(0, e.CtimeUnixNs), target)
	}
	if e.CtimeUnixNs == 0 {
		t.Fatal("ctime should be non-zero")
	}
}
