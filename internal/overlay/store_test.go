package overlay

import (
	"context"
	"errors"
	iofs "io/fs"
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
	s, cfg := testStore(t)
	ctx := context.Background()

	base := model.BaseNode{Path: "old.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}
	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "old.txt", base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, "old.txt", 0, []byte("content")); err != nil {
		t.Fatal(err)
	}
	target := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := s.SetMtime(ctx, "old.txt", target); err != nil {
		t.Fatal(err)
	}
	before, ok := s.Get("old.txt")
	if !ok {
		t.Fatal("expected old entry")
	}

	if err := s.Rename(ctx, "old.txt", "new.txt"); err != nil {
		t.Fatal(err)
	}

	// Old path should have a whiteout
	if e, ok := s.Get("old.txt"); !ok || !e.IsDeleted() {
		t.Fatal("expected whiteout at old path")
	} else {
		if e.MtimeUnixNs == target.UnixNano() || e.CtimeUnixNs == target.UnixNano() {
			t.Fatalf("whiteout times should not inherit user mtime: %+v", e)
		}
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
	if got.MtimeUnixNs != target.UnixNano() {
		t.Fatalf("mtime = %v, want %v", time.Unix(0, got.MtimeUnixNs), target)
	}
	if got.CtimeUnixNs == target.UnixNano() || got.CtimeUnixNs == 0 {
		t.Fatalf("ctime should advance independently from mtime: %v", time.Unix(0, got.CtimeUnixNs))
	}
	if got.CtimeUnixNs == before.CtimeUnixNs {
		t.Fatalf("rename should bump ctime: before=%v after=%v", time.Unix(0, before.CtimeUnixNs), time.Unix(0, got.CtimeUnixNs))
	}
}

func TestRenameSamePathNoop(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateFile(ctx, "same.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "same.txt", "same.txt"); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("same.txt")
	if !ok {
		t.Fatal("expected entry")
	}
	if got.IsDeleted() {
		t.Fatalf("same-path rename should not create whiteout: %+v", got)
	}
}

func TestRenameSameMissingPathReturnsNotExist(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Rename(ctx, "missing.txt", "missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

func TestRenameCreateRemainsCreateAcrossReconcile(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if _, err := s.CreateFile(ctx, "tmp.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteFile(ctx, "tmp.txt", 0, []byte("content")); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "tmp.txt", "kept.txt"); err != nil {
		t.Fatal(err)
	}

	before, ok := s.Get("kept.txt")
	if !ok {
		t.Fatal("expected renamed create entry")
	}
	if before.Kind != model.OverlayKindCreate {
		t.Fatalf("kind = %q, want create", before.Kind)
	}
	if _, ok := s.Get("tmp.txt"); ok {
		t.Fatal("untracked create rename should not leave a source whiteout")
	}

	baseLookup := func(string) (model.BaseNode, bool) { return model.BaseNode{}, false }
	if err := s.Reconcile(ctx, baseLookup); err != nil {
		t.Fatal(err)
	}
	after, ok := s.Get("kept.txt")
	if !ok || after.IsDeleted() {
		t.Fatalf("reconcile should keep renamed create, got %+v ok=%v", after, ok)
	}
}

func TestRenamePreservesDirectoryKindAndRepairsMode(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o40000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE overlay_entries SET mode=? WHERE path=?`, 0o40000, "src"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.backingPath("src"), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "src", "dst"); err != nil {
		t.Fatal(err)
	}

	got, ok := s.Get("dst")
	if !ok {
		t.Fatal("expected renamed entry")
	}
	if got.Kind != model.OverlayKindMkdir || got.NodeType() != "dir" {
		t.Fatalf("expected renamed directory entry, got %+v", got)
	}
	if got.Mode != 0o755 {
		t.Fatalf("mode = %#o, want 0755", got.Mode)
	}
	st, err := os.Stat(got.BackingPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("backing mode = %#o, want 0755", st.Mode().Perm())
	}
}

func TestRenameRejectsNonEmptyOverlayDirectory(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "src/a.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "src", "dst"); !errors.Is(err, iofs.ErrInvalid) {
		t.Fatalf("err = %v, want fs.ErrInvalid", err)
	}
	if _, ok := s.Get("src"); !ok {
		t.Fatal("source directory should remain after rejected rename")
	}
	if _, ok := s.Get("dst"); ok {
		t.Fatal("destination directory should not exist after rejected rename")
	}
}

func TestRenameOverlayDirectoryIgnoresDeletedDescendants(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "a_b", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "a_b/a.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "a_b/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "axb/secret.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "a_b", "dst"); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("dst")
	if !ok || got.Kind != model.OverlayKindMkdir {
		t.Fatalf("expected renamed directory, got %+v ok=%v", got, ok)
	}
	if e, ok := s.Get("a_b/a.txt"); ok {
		t.Fatalf("deleted descendant tombstone should be removed after directory rename, got %+v", e)
	}
	if e, ok := s.Get("axb/secret.txt"); !ok || !e.IsDeleted() {
		t.Fatalf("unrelated wildcard-like tombstone should remain, got %+v ok=%v", e, ok)
	}
}

func TestRenameOverlayDirectoryCleansUTF8DeletedDescendants(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	dir := "\u00e9"
	child := dir + "/a.txt"

	if err := s.Mkdir(ctx, dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, child, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, child); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, dir, "dst"); err != nil {
		t.Fatal(err)
	}
	if e, ok := s.Get(child); ok {
		t.Fatalf("UTF-8 deleted descendant tombstone should be removed after directory rename, got %+v", e)
	}
}

func TestRenameOverlayDirectoryRollbackRestoresDeletedDescendants(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFile(ctx, "src/a.txt", 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, "src/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.backingPath("dst"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.backingPath("dst"), "file.txt"), []byte("busy"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Rename(ctx, "src", "dst"); err == nil {
		t.Fatal("expected filesystem rename failure")
	}
	if e, ok := s.Get("src/a.txt"); !ok || !e.IsDeleted() {
		t.Fatalf("deleted descendant tombstone should be restored on rollback, got %+v ok=%v", e, ok)
	}
}

func TestRenameChainPreservesOriginalSourceForReconcile(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	base := model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}

	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "a.txt", base); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "b.txt", "c.txt"); err != nil {
		t.Fatal(err)
	}

	before, ok := s.Get("c.txt")
	if !ok {
		t.Fatal("expected final rename entry")
	}
	if before.TargetPath != "a.txt" {
		t.Fatalf("target path = %q, want original source a.txt", before.TargetPath)
	}

	baseLookup := func(path string) (model.BaseNode, bool) {
		if path == "a.txt" {
			return base, true
		}
		return model.BaseNode{}, false
	}
	if err := s.Reconcile(ctx, baseLookup); err != nil {
		t.Fatal(err)
	}

	after, ok := s.Get("c.txt")
	if !ok || after.IsDeleted() {
		t.Fatalf("reconcile should keep chained rename, got %+v ok=%v", after, ok)
	}
	if after.TargetPath != "a.txt" {
		t.Fatalf("target path after reconcile = %q, want a.txt", after.TargetPath)
	}
}

func TestReconcileRemovesSourceWhiteoutForStaleRename(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	base := model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}

	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "a.txt", base); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if e, ok := s.Get("a.txt"); !ok || !e.IsDeleted() {
		t.Fatalf("expected source whiteout before reconcile, got %+v ok=%v", e, ok)
	}

	baseLookup := func(path string) (model.BaseNode, bool) {
		if path == "a.txt" {
			return model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "bbb"}, true
		}
		return model.BaseNode{}, false
	}
	if err := s.Reconcile(ctx, baseLookup); err != nil {
		t.Fatal(err)
	}
	if e, ok := s.Get("b.txt"); ok {
		t.Fatalf("stale rename should be removed, got %+v", e)
	}
	if e, ok := s.Get("a.txt"); ok {
		t.Fatalf("paired source whiteout should be removed with stale rename, got %+v", e)
	}
}

func TestReconcileRemovesIntermediateWhiteoutsForStaleChainedRename(t *testing.T) {
	s, cfg := testStore(t)
	ctx := context.Background()
	base := model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "aaa"}

	if _, err := s.EnsureCopyOnWrite(ctx, cfg, "a.txt", base); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(ctx, "b.txt", "c.txt"); err != nil {
		t.Fatal(err)
	}
	if e, ok := s.Get("b.txt"); !ok || !e.IsDeleted() {
		t.Fatalf("expected intermediate whiteout before reconcile, got %+v ok=%v", e, ok)
	}

	baseLookup := func(path string) (model.BaseNode, bool) {
		switch path {
		case "a.txt":
			return model.BaseNode{Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "bbb"}, true
		case "b.txt":
			return model.BaseNode{Path: "b.txt", Type: "file", Mode: 0o644, ObjectOID: "new-b"}, true
		default:
			return model.BaseNode{}, false
		}
	}
	if err := s.Reconcile(ctx, baseLookup); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"a.txt", "b.txt", "c.txt"} {
		if e, ok := s.Get(path); ok {
			t.Fatalf("%s should be removed after stale chained rename reconcile, got %+v", path, e)
		}
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

func TestMkdirNormalizesGitTreeMode(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o40000); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get("src")
	if !ok {
		t.Fatal("expected entry")
	}
	if e.Mode != 0o755 {
		t.Fatalf("mode = %#o, want 0755", e.Mode)
	}
}

func TestMkdirPreservesExplicitZeroMode(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "private", 0); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Get("private")
	if !ok {
		t.Fatal("expected entry")
	}
	if e.Mode != 0 {
		t.Fatalf("mode = %#o, want 0", e.Mode)
	}
}

func TestSetMtimeRepairsGitTreeDirectoryMode(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()

	if err := s.Mkdir(ctx, "src", 0o40000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE overlay_entries SET mode=? WHERE path=?`, 0o40000, "src"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.backingPath("src"), 0); err != nil {
		t.Fatal(err)
	}
	target := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := s.SetMtime(ctx, "src", target); err != nil {
		t.Fatal(err)
	}

	e, ok := s.Get("src")
	if !ok {
		t.Fatal("expected entry")
	}
	if e.Mode != 0o755 {
		t.Fatalf("mode = %#o, want 0755", e.Mode)
	}
	st, err := os.Stat(e.BackingPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("backing mode = %#o, want 0755", st.Mode().Perm())
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
