package snapshot

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudflare/artifact-fs/internal/meta"
	"github.com/cloudflare/artifact-fs/internal/model"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
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
	gen, err := s.PublishGeneration(ctx, "head1", "main", nodes)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 1 {
		t.Fatalf("expected gen 1, got %d", gen)
	}

	n, ok := s.GetNode(gen, "README.md")
	if !ok {
		t.Fatal("expected README.md")
	}
	if n.SizeBytes != 42 || n.ObjectOID != "abc123" {
		t.Fatalf("wrong node: %+v", n)
	}

	_, ok = s.GetNode(gen, "nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}

// otherWriter opens a second connection to the database at path, as another
// process would. Its busy_timeout is 0, so it fails at once instead of waiting
// for the write lock.
func otherWriter(t *testing.T, path string) *sql.Conn {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(0)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// The CLI's sync prepare and the daemon's HEAD reconcile publish into one
// database. SQLite fails a deferred transaction's upgrade from a read to a
// write at once with SQLITE_BUSY, without the busy handler, while another
// connection holds the write lock. So a publish holds the write lock from its
// read of the current generation, and another writer waits for it.
func TestPublishHoldsTheWriteLockFromItsGenerationRead(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "snap.sqlite")
	s, err := New(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conn := otherWriter(t, path)
	var lockErr error
	testHookAfterGenerationRead = func() {
		if _, lockErr = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); lockErr == nil {
			lockErr = errors.New("the other writer took the write lock")
		}
	}
	defer func() { testHookAfterGenerationRead = nil }()

	gen, err := s.PublishGeneration(ctx, "head1", "main", []model.BaseNode{{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"}})
	// The other writer holds the lock only when it took it inside the publish.
	_, _ = conn.ExecContext(ctx, `ROLLBACK`)
	if err != nil {
		t.Fatalf("publish with another writer between its read and its write: %v", err)
	}
	if sqliteErr, ok := errors.AsType[*sqlite.Error](lockErr); !ok || sqliteErr.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Fatalf("other writer inside the publish: %v; want SQLITE_BUSY", lockErr)
	}
	if gen != 1 {
		t.Fatalf("published generation %d, want 1", gen)
	}
}

// Opening a store on a current schema only reads, as status does for each
// repo, so it does not wait for another connection's write.
func TestNewOnACurrentSchemaDoesNotWaitForAWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "snap.sqlite")
	s, err := New(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	conn := otherWriter(t, path)
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, `ROLLBACK`)

	s, err = New(ctx, path)
	if err != nil {
		t.Fatalf("open while another connection holds the write lock: %v", err)
	}
	s.Close()
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
	gen, err := s.PublishGeneration(ctx, "h1", "main", nodes)
	if err != nil {
		t.Fatal(err)
	}

	// Root children
	children, err := s.ListChildren(gen, ".")
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
	children, err = s.ListChildren(gen, "sub")
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
	g1, _ := s.PublishGeneration(ctx, "h1", "main", nodes)
	g2, _ := s.PublishGeneration(ctx, "h2", "main", nodes)
	g3, _ := s.PublishGeneration(ctx, "h3", "main", nodes)

	if g1 != 1 || g2 != 2 || g3 != 3 {
		t.Fatalf("unexpected generations: %d %d %d", g1, g2, g3)
	}

	if err := s.PruneGenerations(ctx, g2); err != nil {
		t.Fatal(err)
	}
	// Generation 1 should be cleaned up once the caller publishes the view.
	_, ok := s.GetNode(1, "f.txt")
	if ok {
		t.Fatal("gen 1 should be cleaned up")
	}
	// Generation 2 should still exist (gen-1)
	_, ok = s.GetNode(2, "f.txt")
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
	s.PublishGeneration(ctx, "h1", "main", nodes)

	gen, err = s.CurrentGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gen != 1 {
		t.Fatalf("expected 1, got %d", gen)
	}
}

func TestUpdateSizeUsesGenerationObjectOIDIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	nodes := []model.BaseNode{
		{RepoID: "r", Path: ".", Type: "dir", Mode: 0o755, SizeState: "known"},
		{RepoID: "r", Path: "a.txt", Type: "file", Mode: 0o644, ObjectOID: "shared", SizeState: "unknown"},
		{RepoID: "r", Path: "b.txt", Type: "file", Mode: 0o644, ObjectOID: "other", SizeState: "unknown"},
	}
	gen, err := s.PublishGeneration(ctx, "h1", "main", nodes)
	if err != nil {
		t.Fatal(err)
	}

	var id, parent, notUsed int
	var detail string
	err = s.db.QueryRowContext(ctx, `EXPLAIN QUERY PLAN UPDATE base_nodes SET size_bytes=?, size_state='known' WHERE generation=? AND object_oid=?`, 42, gen, "shared").Scan(&id, &parent, &notUsed, &detail)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "idx_base_nodes_gen_oid") {
		t.Fatalf("UpdateSize plan = %q, want idx_base_nodes_gen_oid", detail)
	}

	if err := s.UpdateSizes(ctx, gen, map[string]int64{"shared": 42, "other": 7}); err != nil {
		t.Fatal(err)
	}
	n, ok := s.GetNode(gen, "a.txt")
	if !ok {
		t.Fatal("a.txt not found")
	}
	if n.SizeState != "known" || n.SizeBytes != 42 {
		t.Fatalf("a.txt size = %s/%d, want known/42", n.SizeState, n.SizeBytes)
	}
	n, ok = s.GetNode(gen, "b.txt")
	if !ok {
		t.Fatal("b.txt not found")
	}
	if n.SizeState != "known" || n.SizeBytes != 7 {
		t.Fatalf("b.txt size = %s/%d, want known/7", n.SizeState, n.SizeBytes)
	}
}

func TestNewDropsLegacyTables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snap.sqlite")
	db, err := meta.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `
		CREATE TABLE learned_path_stats (
			path TEXT PRIMARY KEY,
			access_count INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE blob_cache_index (
			object_oid TEXT PRIMARY KEY,
			cache_path TEXT NOT NULL
		);
	`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := New(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	assertTableMissing := func(name string) {
		t.Helper()
		row := s.db.QueryRowContext(context.Background(), `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name)
		var got string
		if err := row.Scan(&got); err == nil {
			t.Fatalf("table %q should be dropped", name)
		} else if err != sql.ErrNoRows {
			t.Fatalf("lookup table %q: %v", name, err)
		}
	}

	assertTableMissing("learned_path_stats")
	assertTableMissing("blob_cache_index")
}
